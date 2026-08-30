// Package llm implements a Google ADK model.LLM adapter over OpenAI-compatible
// /v1/chat/completions endpoints (llama.cpp, DeepSeek, OpenRouter, ...).
//
// It translates genai content to and from the Chat Completions wire format:
//   - the ADK system instruction (req.Config.SystemInstruction) becomes a
//     `system` message;
//   - genai FunctionCall parts become assistant `tool_calls[]` (arguments as
//     JSON text, the full array, as OpenAI requires);
//   - genai FunctionResponse parts become `tool` role messages keyed by
//     `tool_call_id`;
//   - DeepSeek-style `reasoning_content` is mapped to genai.Part{Thought: true}
//     so the TUI can hide it; thinking is never sent back to the model.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
)

// ChatCompletionsModel implements model.LLM for an OpenAI-compatible endpoint.
type ChatCompletionsModel struct {
	providerName string // display name (config provider name)
	modelName    string // model id sent to the endpoint
	endpoint     string // base URL, must include /v1 (no trailing slash)
	apiKey       string
	client       *http.Client
}

// NewChatCompletionsModel builds an ADK model.LLM for an OpenAI-compatible
// /v1/chat/completions endpoint. The endpoint must include /v1.
func NewChatCompletionsModel(providerName, endpoint, apiKey, modelName string) (*ChatCompletionsModel, error) {
	if providerName == "" {
		return nil, errors.New("llm: provider name is required")
	}
	if endpoint == "" {
		return nil, errors.New("llm: endpoint is required")
	}
	if modelName == "" {
		return nil, errors.New("llm: model name is required")
	}
	trimmed := strings.TrimSuffix(endpoint, "/")
	if !strings.HasSuffix(trimmed, "/v1") {
		return nil, fmt.Errorf("llm: endpoint %q must include /v1", endpoint)
	}
	return &ChatCompletionsModel{
		providerName: providerName,
		modelName:    modelName,
		endpoint:     trimmed,
		apiKey:       apiKey,
		client:       &http.Client{},
	}, nil
}

// Name implements model.LLM. Note the ADK flow sets LLMRequest.Model from this
// value, so GenerateContent always uses the configured modelName instead.
func (m *ChatCompletionsModel) Name() string { return m.providerName }

// GenerateContent implements model.LLM. With stream=false it yields exactly
// one final response; with stream=true it yields partial deltas (text and
// thought) followed by one final non-partial response.
func (m *ChatCompletionsModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if !stream {
			resp, err := m.complete(ctx, req)
			if err != nil {
				yield(nil, err)
				return
			}
			yield(resp, nil)
			return
		}
		for r, err := range m.stream(ctx, req) {
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(r, nil) {
				return
			}
		}
	}
}

// complete performs a single non-streaming chat completion.
func (m *ChatCompletionsModel) complete(ctx context.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	resp, err := m.doPost(ctx, m.buildChatRequest(req, false))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: decode response: %w", m.providerName, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %s", m.providerName, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("%s: empty choices", m.providerName)
	}
	msg := out.Choices[0].Message
	return buildFinalLLMResponse(
		msg.Content,
		msg.ReasoningContent,
		parseToolCalls(msg.ToolCalls),
		mapFinishReason(out.Choices[0].FinishReason),
		mapUsage(out.Usage),
	), nil
}

// stream performs a streaming chat completion, yielding partial deltas then a
// final assembled response.
func (m *ChatCompletionsModel) stream(ctx context.Context, req *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		resp, err := m.doPost(ctx, m.buildChatRequest(req, true))
		if err != nil {
			yield(nil, err)
			return
		}
		defer resp.Body.Close()

		var content, reasoning strings.Builder
		type accToolCall struct {
			id   string
			name string
			args strings.Builder
		}
		toolByIndex := make(map[int]*accToolCall)
		var toolOrder []int
		var usage *usage
		var finish genai.FinishReason

		rd := bufio.NewReader(resp.Body)
		for {
			payload, err := nextSSEData(rd)
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				yield(nil, fmt.Errorf("%s: stream read: %w", m.providerName, err))
				return
			}
			if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
				break
			}
			var chunk chatStreamChunk
			if err := json.Unmarshal(payload, &chunk); err != nil {
				continue // keepalive / non-JSON payload
			}
			if chunk.Error != nil {
				yield(nil, fmt.Errorf("%s: %s", m.providerName, chunk.Error.Message))
				return
			}
			if len(chunk.Choices) > 0 {
				ch := chunk.Choices[0]
				if ch.Delta.Content != "" {
					content.WriteString(ch.Delta.Content)
					if !yield(partialTextResponse(ch.Delta.Content), nil) {
						return
					}
				}
				if ch.Delta.ReasoningContent != "" {
					reasoning.WriteString(ch.Delta.ReasoningContent)
					if !yield(partialThoughtResponse(ch.Delta.ReasoningContent), nil) {
						return
					}
				}
				for _, tc := range ch.Delta.ToolCalls {
					acc, ok := toolByIndex[tc.Index]
					if !ok {
						acc = &accToolCall{}
						toolByIndex[tc.Index] = acc
						toolOrder = append(toolOrder, tc.Index)
					}
					if tc.ID != "" {
						acc.id = tc.ID
					}
					if tc.Function.Name != "" {
						acc.name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						acc.args.WriteString(tc.Function.Arguments)
					}
				}
				if ch.FinishReason != nil {
					finish = mapFinishReason(ch.FinishReason)
				}
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
		}

		var parts []*genai.Part
		if reasoning.Len() > 0 {
			parts = append(parts, &genai.Part{Text: reasoning.String(), Thought: true})
		}
		if content.Len() > 0 {
			parts = append(parts, genai.NewPartFromText(content.String()))
		}
		for _, idx := range toolOrder {
			acc := toolByIndex[idx]
			args := map[string]any{}
			if s := acc.args.String(); s != "" {
				if err := json.Unmarshal([]byte(s), &args); err != nil {
					args = map[string]any{"_raw": s}
				}
			}
			parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: acc.id, Name: acc.name, Args: args}})
		}
		final := &model.LLMResponse{
			Content:       genai.NewContentFromParts(parts, genai.RoleModel),
			FinishReason:  finish,
			UsageMetadata: mapUsage(usage),
			TurnComplete:  true,
		}
		yield(final, nil)
	}
}

// buildChatRequest converts an ADK LLMRequest to the OpenAI wire format.
func (m *ChatCompletionsModel) buildChatRequest(req *model.LLMRequest, stream bool) chatRequest {
	cr := chatRequest{
		Model:    m.modelName,
		Messages: m.toChatMessages(req),
		Stream:   stream,
	}
	if tools := toChatTools(req); len(tools) > 0 {
		cr.Tools = tools
	}
	if req.Config != nil {
		if req.Config.Temperature != nil {
			t := float64(*req.Config.Temperature)
			cr.Temperature = &t
		}
		if req.Config.MaxOutputTokens > 0 {
			cr.MaxTokens = int(req.Config.MaxOutputTokens)
		}
		if len(req.Config.StopSequences) > 0 {
			cr.Stop = req.Config.StopSequences
		}
	}
	return cr
}

// toChatMessages converts the system instruction plus conversation contents to
// OpenAI chat messages. Thought parts are dropped (thinking is not sent back).
func (m *ChatCompletionsModel) toChatMessages(req *model.LLMRequest) []chatMessage {
	var out []chatMessage
	if req.Config != nil && req.Config.SystemInstruction != nil {
		if text := textOf(req.Config.SystemInstruction); text != "" {
			out = append(out, chatMessage{Role: "system", Content: text})
		}
	}
	for _, c := range req.Contents {
		out = append(out, contentToMessages(c)...)
	}
	return out
}

// contentToMessages maps one genai.Content to one or more chat messages.
func contentToMessages(c *genai.Content) []chatMessage {
	if c == nil {
		return nil
	}
	oaRole := mapRole(c.Role)
	var msgs []chatMessage
	var text strings.Builder
	var calls []chatToolCall
	flush := func() {
		if text.Len() == 0 && len(calls) == 0 {
			return
		}
		msg := chatMessage{Role: oaRole}
		if text.Len() > 0 {
			msg.Content = text.String()
		}
		if len(calls) > 0 {
			msg.ToolCalls = calls
		}
		msgs = append(msgs, msg)
		text.Reset()
		calls = nil
	}
	for _, p := range c.Parts {
		switch {
		case p.FunctionCall != nil:
			fc := p.FunctionCall
			argsJSON, err := json.Marshal(fc.Args)
			if err != nil {
				argsJSON = []byte("{}")
			}
			calls = append(calls, chatToolCall{
				ID:   fc.ID,
				Type: "function",
				Function: chatToolCallFunc{
					Name:      fc.Name,
					Arguments: string(argsJSON),
				},
			})
		case p.FunctionResponse != nil:
			flush()
			fr := p.FunctionResponse
			respJSON, err := json.Marshal(fr.Response)
			if err != nil {
				respJSON = []byte("{}")
			}
			msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: fr.ID, Content: string(respJSON)})
		case p.Thought:
			// thinking is never sent back to the model (garess parity).
		default:
			if p.Text != "" {
				text.WriteString(p.Text)
			}
		}
	}
	flush()
	return msgs
}

// textOf returns the concatenated non-thought text of a content.
func textOf(c *genai.Content) string {
	var b strings.Builder
	for _, p := range c.Parts {
		if p.Text != "" && !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// mapRole maps a genai role to the OpenAI chat role.
func mapRole(role string) string {
	switch role {
	case string(genai.RoleModel):
		return "assistant"
	case "system":
		return "system"
	case "function":
		return "tool"
	default:
		return "user"
	}
}

// toChatTools converts the packed genai tool declarations to the OpenAI tools
// array.
func toChatTools(req *model.LLMRequest) []chatTool {
	if req.Config == nil {
		return nil
	}
	var out []chatTool
	for _, t := range req.Config.Tools {
		if t == nil {
			continue
		}
		for _, decl := range t.FunctionDeclarations {
			if decl == nil || decl.Name == "" {
				continue
			}
			ct := chatTool{
				Type: "function",
				Function: chatToolFunction{
					Name:        decl.Name,
					Description: decl.Description,
				},
			}
			if decl.ParametersJsonSchema != nil {
				ct.Function.Parameters = jsonRoundTrip(decl.ParametersJsonSchema)
			} else if decl.Parameters != nil {
				ct.Function.Parameters = jsonRoundTrip(decl.Parameters)
			}
			out = append(out, ct)
		}
	}
	return out
}

// jsonRoundTrip marshals v to JSON and back to a plain map, so any schema
// object becomes a clean JSON Schema object for the wire.
func jsonRoundTrip(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// buildFinalLLMResponse assembles a final (non-partial) model response.
func buildFinalLLMResponse(contentText, reasoningText string, calls []*genai.FunctionCall, finish genai.FinishReason, usage *genai.GenerateContentResponseUsageMetadata) *model.LLMResponse {
	var parts []*genai.Part
	if reasoningText != "" {
		parts = append(parts, &genai.Part{Text: reasoningText, Thought: true})
	}
	if contentText != "" {
		parts = append(parts, genai.NewPartFromText(contentText))
	}
	for _, fc := range calls {
		parts = append(parts, &genai.Part{FunctionCall: fc})
	}
	return &model.LLMResponse{
		Content:       genai.NewContentFromParts(parts, genai.RoleModel),
		FinishReason:  finish,
		UsageMetadata: usage,
		TurnComplete:  true,
	}
}

func partialTextResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{
		Content: genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText(text)}, genai.RoleModel),
		Partial: true,
	}
}

func partialThoughtResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{
		Content: genai.NewContentFromParts([]*genai.Part{{Text: text, Thought: true}}, genai.RoleModel),
		Partial: true,
	}
}

// parseToolCalls converts OpenAI tool_calls into genai function calls.
func parseToolCalls(calls []chatToolCall) []*genai.FunctionCall {
	var out []*genai.FunctionCall
	for _, c := range calls {
		if c.Function.Name == "" {
			continue
		}
		args := map[string]any{}
		if c.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
				args = map[string]any{"_raw": c.Function.Arguments}
			}
		}
		out = append(out, &genai.FunctionCall{ID: c.ID, Name: c.Function.Name, Args: args})
	}
	return out
}

func mapFinishReason(fr *string) genai.FinishReason {
	if fr == nil {
		return genai.FinishReasonUnspecified
	}
	switch *fr {
	case "stop":
		return genai.FinishReasonStop
	case "length", "max_tokens":
		return genai.FinishReasonMaxTokens
	default:
		return genai.FinishReasonOther
	}
}

func mapUsage(u *usage) *genai.GenerateContentResponseUsageMetadata {
	if u == nil {
		return nil
	}
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     int32(u.PromptTokens),
		CandidatesTokenCount: int32(u.CompletionTokens),
		ThoughtsTokenCount:   int32(u.CompletionTokensDetails.ReasoningTokens),
	}
}

// doPost sends a JSON request to /chat/completions.
func (m *ChatCompletionsModel) doPost(ctx context.Context, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", m.providerName, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.apiKey)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.providerName, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: chat completion failed (%d): %s", m.providerName, resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}
	return resp, nil
}
