package llm

import (
	"bufio"
	"bytes"
)

// OpenAI Chat Completions wire types. Only the fields garess uses are modeled;
// unknown fields are ignored by encoding/json.

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools,omitempty"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stop        []string      `json:"stop,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type,omitempty"`
	Function chatToolCallFunc `json:"function"`
}

type chatToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Role             string         `json:"role"`
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage    `json:"usage,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string              `json:"content"`
			ReasoningContent string              `json:"reasoning_content"`
			ToolCalls        []chatToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage    `json:"usage,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type chatToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type usage struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// nextSSEData reads the next SSE event's data payload. OpenAI-compatible chat
// completions send one `data:` line per event (optionally followed by a blank
// line); comment and `event:` lines are ignored. Returns io.EOF at end of
// stream.
func nextSSEData(rd *bufio.Reader) ([]byte, error) {
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			return nil, err // io.EOF or real read error
		}
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0:
			// blank separator between events — ignore.
		case bytes.HasPrefix(line, []byte("data:")):
			return bytes.TrimSpace(line[len("data:"):]), nil
		default:
			// comment (`: ...`) or other SSE field — ignore.
		}
	}
}
