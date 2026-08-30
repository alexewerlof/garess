package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
)

// capturedRequest is the subset of the wire request the tests assert on.
type capturedRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
		ToolCallID string `json:"tool_call_id"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	Stream bool `json:"stream"`
}

func newTestModel(t *testing.T, server *httptest.Server) *ChatCompletionsModel {
	t.Helper()
	m, err := NewChatCompletionsModel("test", server.URL+"/v1", "test-key", "test-model")
	if err != nil {
		t.Fatalf("NewChatCompletionsModel: %v", err)
	}
	return m
}

func testRequest(system string, tools ...*genai.FunctionDeclaration) *model.LLMRequest {
	cfg := &genai.GenerateContentConfig{}
	if system != "" {
		cfg.SystemInstruction = genai.NewContentFromText(system, genai.RoleUser)
	}
	if len(tools) > 0 {
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: tools}}
	}
	return &model.LLMRequest{
		Model:    "anything", // ignored; adapter uses configured model
		Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
		Config:   cfg,
	}
}

func readAllResponses(t *testing.T, seq func(yield func(*model.LLMResponse, error) bool)) ([]*model.LLMResponse, error) {
	t.Helper()
	var out []*model.LLMResponse
	var firstErr error
	for r, err := range seq {
		if err != nil && firstErr == nil {
			firstErr = err
			continue
		}
		if r != nil {
			out = append(out, r)
		}
	}
	return out, firstErr
}

func textParts(c *genai.Content) []string {
	var out []string
	for _, p := range c.Parts {
		if p.Text != "" && !p.Thought {
			out = append(out, p.Text)
		}
	}
	return out
}

func thoughtText(c *genai.Content) string {
	var b strings.Builder
	for _, p := range c.Parts {
		if p.Text != "" && p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func functionCalls(c *genai.Content) []*genai.FunctionCall {
	var out []*genai.FunctionCall
	for _, p := range c.Parts {
		if p.FunctionCall != nil {
			out = append(out, p.FunctionCall)
		}
	}
	return out
}

func TestGenerateContentStreaming(t *testing.T) {
	var got capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if gotAuth := r.Header.Get("Authorization"); gotAuth != "Bearer test-key" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"reasoning_content":"Let me think"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":" world"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"com"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"mand\":\"ls\"}"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":7,"completion_tokens_details":{"reasoning_tokens":3}}}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintln(w, c)
		}
	}))
	defer server.Close()

	m := newTestModel(t, server)
	req := testRequest("You are a test agent.", &genai.FunctionDeclaration{
		Name:        "bash",
		Description: "Run a shell command",
		ParametersJsonSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		},
	})

	responses, err := readAllResponses(t, m.GenerateContent(context.Background(), req, true))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}

	if got.Stream != true {
		t.Errorf("stream = %v, want true", got.Stream)
	}
	if got.Model != "test-model" {
		t.Errorf("model = %q, want configured test-model", got.Model)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want [system, user]", len(got.Messages))
	}
	if got.Messages[0].Role != "system" || got.Messages[0].Content != "You are a test agent." {
		t.Errorf("first message = %+v, want system instruction", got.Messages[0])
	}
	if got.Messages[1].Role != "user" || got.Messages[1].Content != "hello" {
		t.Errorf("second message = %+v", got.Messages[1])
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "bash" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if params, ok := got.Tools[0].Function.Parameters["properties"].(map[string]any); !ok || params["command"] == nil {
		t.Errorf("tool parameters not carried through: %+v", got.Tools[0].Function.Parameters)
	}

	// Partials: one thought + two text deltas, then the final response.
	if len(responses) < 3 {
		t.Fatalf("responses = %d, want >= 3 (2 text partials + final)", len(responses))
	}
	if !responses[0].Partial || thoughtText(responses[0].Content) != "Let me think" {
		t.Errorf("partial[0] = %+v, want thought delta", responses[0])
	}
	if !responses[1].Partial || strings.Join(textParts(responses[1].Content), "") != "Hello" {
		t.Errorf("partial[1] = %+v, want 'Hello'", responses[1])
	}
	if !responses[2].Partial || strings.Join(textParts(responses[2].Content), "") != " world" {
		t.Errorf("partial[2] = %+v, want ' world'", responses[2])
	}

	final := responses[len(responses)-1]
	if final.Partial || !final.TurnComplete {
		t.Errorf("final should be non-partial and turn-complete: %+v", final)
	}
	if gotThink := thoughtText(final.Content); gotThink != "Let me think" {
		t.Errorf("final thinking = %q", gotThink)
	}
	if gotText := strings.Join(textParts(final.Content), ""); gotText != "Hello world" {
		t.Errorf("final text = %q", gotText)
	}
	calls := functionCalls(final.Content)
	if len(calls) != 1 {
		t.Fatalf("final function calls = %d, want 1", len(calls))
	}
	if calls[0].Name != "bash" || calls[0].ID != "call_1" {
		t.Errorf("call = %+v", calls[0])
	}
	if got := calls[0].Args["command"]; got != "ls" {
		t.Errorf("call args = %+v, want command=ls", calls[0].Args)
	}
	if final.UsageMetadata == nil || final.UsageMetadata.PromptTokenCount != 5 || final.UsageMetadata.ThoughtsTokenCount != 3 {
		t.Errorf("usage = %+v", final.UsageMetadata)
	}
}

func TestGenerateContentNonStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"42","reasoning_content":"compute","tool_calls":[{"id":"call_9","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"x.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}))
	defer server.Close()

	m := newTestModel(t, server)
	responses, err := readAllResponses(t, m.GenerateContent(context.Background(), testRequest(""), false))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	r := responses[0]
	if r.Partial || !r.TurnComplete {
		t.Errorf("expected final response, got %+v", r)
	}
	if got := strings.Join(textParts(r.Content), ""); got != "42" {
		t.Errorf("text = %q", got)
	}
	if got := thoughtText(r.Content); got != "compute" {
		t.Errorf("thinking = %q", got)
	}
	calls := functionCalls(r.Content)
	if len(calls) != 1 || calls[0].Name != "read_file" || calls[0].Args["path"] != "x.txt" {
		t.Errorf("calls = %+v", calls)
	}
}

func TestGenerateContentRequestConversion(t *testing.T) {
	var got capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	m := newTestModel(t, server)
	req := &model.LLMRequest{
		Contents: []*genai.Content{
			{ // assistant turn: visible text + hidden thought (must be dropped)
				Role: string(genai.RoleModel),
				Parts: []*genai.Part{
					{Text: "I will check.", Thought: true},
					{Text: "Running now"},
				},
			},
			{ // assistant turn with a function call
				Role: string(genai.RoleModel),
				Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{ID: "call_7", Name: "bash", Args: map[string]any{"command": "ls"}}},
				},
			},
			{ // user turn with the function response
				Role: string(genai.RoleUser),
				Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{ID: "call_7", Name: "bash", Response: map[string]any{"output": "file.txt"}}},
				},
			},
		},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("SYS", genai.RoleUser),
		},
	}

	if _, err := readAllResponses(t, m.GenerateContent(context.Background(), req, true)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}

	var roles []string
	for _, msg := range got.Messages {
		roles = append(roles, msg.Role)
	}
	want := []string{"system", "assistant", "assistant", "tool"}
	if fmt.Sprint(roles) != fmt.Sprint(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	// Thought part must NOT appear in outgoing assistant content.
	if got.Messages[1].Content != "Running now" {
		t.Errorf("assistant content = %q, want thought dropped", got.Messages[1].Content)
	}
	// Function call carried with full arguments JSON.
	if len(got.Messages[2].ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", got.Messages[2].ToolCalls)
	}
	tc := got.Messages[2].ToolCalls[0]
	if tc.ID != "call_7" || tc.Function.Name != "bash" || tc.Function.Arguments != `{"command":"ls"}` {
		t.Errorf("tool call = %+v", tc)
	}
	// Tool result paired by call id.
	tm := got.Messages[3]
	if tm.Role != "tool" || tm.ToolCallID != "call_7" || tm.Content != `{"output":"file.txt"}` {
		t.Errorf("tool message = %+v", tm)
	}
}

func TestGenerateContentStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"bad grammar","type":"invalid_request_error"}}`)
	}))
	defer server.Close()

	m := newTestModel(t, server)
	_, err := readAllResponses(t, m.GenerateContent(context.Background(), testRequest(""), true))
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %v", err)
	}
}

func TestNewChatCompletionsModelValidation(t *testing.T) {
	if _, err := NewChatCompletionsModel("p", "http://x:8080", "", "m"); err == nil {
		t.Error("endpoint without /v1 should be rejected")
	}
	if _, err := NewChatCompletionsModel("p", "http://x:8080/v1", "", ""); err == nil {
		t.Error("empty model should be rejected")
	}
	if _, err := NewChatCompletionsModel("", "http://x:8080/v1", "", "m"); err == nil {
		t.Error("empty provider name should be rejected")
	}
}

func TestNextSSEDataBlankLineSeparation(t *testing.T) {
	rd := bufio.NewReader(strings.NewReader(": keepalive\ndata: {\"a\": 1}\n\ndata: [DONE]\n"))
	if payload, err := nextSSEData(rd); err != nil || string(payload) != `{"a": 1}` {
		t.Errorf("payload = %q, err = %v", payload, err)
	}
	if payload, err := nextSSEData(rd); err != nil || string(payload) != "[DONE]" {
		t.Errorf("payload = %q, err = %v", payload, err)
	}
	if _, err := nextSSEData(rd); !errors.Is(err, io.EOF) {
		t.Errorf("want io.EOF, got %v", err)
	}
}
