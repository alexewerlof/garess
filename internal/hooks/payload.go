package hooks

import (
	"strconv"
	"strings"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
)

// Payload is the JSON object piped to a hook on stdin. Only the fields that
// apply to an event are populated; optional fields use omitempty so payloads
// stay small. Text and tool arguments are truncated (see clip) so a hook can
// never be flooded with a whole file or transcript.
type Payload struct {
	Event    string `json:"event"`
	AppName  string `json:"app_name,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	Session  string `json:"session_id,omitempty"`
	InvokeID string `json:"invocation_id,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Branch   string `json:"branch,omitempty"`

	// Tool events (before_tool, after_tool, on_tool_error).
	Tool string         `json:"tool,omitempty"`
	Args map[string]any `json:"args,omitempty"`

	// Model events (before_model, after_model, on_model_error).
	Model        string `json:"model,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`

	// on_event metadata.
	EventID string `json:"event_id,omitempty"`
	Author  string `json:"author,omitempty"`
	Final   bool   `json:"final,omitempty"`

	// Human-readable text (user message, assistant reply, model/tool error).
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

// maxRunes caps string fields that can carry arbitrary content (text, errors)
// so a payload can never exceed a few KB.
const maxRunes = 4000

// maxArgRunes caps each string tool argument. write_file content is the usual
// overflow risk.
const maxArgRunes = 2000

// fillContext populates the invocation/session fields every payload carries.
func fillContext(p *Payload, ictx agent.InvocationContext) {
	if ictx == nil {
		return
	}
	p.InvokeID = ictx.InvocationID()
	p.Branch = ictx.Branch()
	if a := ictx.Agent(); a != nil {
		p.Agent = a.Name()
	}
	if s := ictx.Session(); s != nil {
		p.AppName = s.AppName()
		p.UserID = s.UserID()
		p.Session = s.ID()
	}
}

// fillContent pulls human-readable text out of a genai content (user message,
// assistant reply, ...).
func fillContent(p *Payload, c *genai.Content) {
	if c == nil {
		return
	}
	p.Text = clip(contentText(c), maxRunes)
}

// sanitizeArgs returns a copy of args with every string value truncated, so a
// write_file body cannot bloat a payload. Non-string values pass through.
func sanitizeArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	san := make(map[string]any, len(args))
	for k, v := range args {
		if s, ok := v.(string); ok {
			san[k] = clip(s, maxArgRunes)
		} else {
			san[k] = v
		}
	}
	return san
}

// fillArgs adds sanitized tool arguments: string values are truncated so a
// write_file body cannot bloat the payload.
func fillArgs(p *Payload, args map[string]any) {
	if len(args) == 0 {
		return
	}
	p.Args = sanitizeArgs(args)
}

// contentText joins the text parts of a content into one string. Function
// calls/responses have no text and yield "".
func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range c.Parts {
		if part.Text != "" && !part.Thought {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// clip truncates s to at most n runes, appending an ellipsis note so hooks can
// tell when content was cut. It never splits a UTF-8 rune.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…[+" + strconv.Itoa(len(r)-n) + " runes]"
}
