// Package compress implements context compression for garess: it turns an
// older slice of conversation events into one dense summary so the agent can
// keep working inside its context window. It only prepares text — persisting
// the summary and a compaction marker is chat.Service.Compact's job (see
// internal/chat), and the raw JSONL transcript is never modified.
package compress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
)

const (
	// SummaryHeader prefixes the summary event's text so the model can tell a
	// compressed digest from a live user message.
	SummaryHeader = "Earlier conversation (compressed):\n"

	// DefaultMaxTranscriptChars caps the transcript fed to the summarizer
	// (roughly a quarter of that in tokens). Anything beyond the cap is
	// trimmed from the oldest events first so the freshest context survives.
	DefaultMaxTranscriptChars = 120_000

	// DefaultMaxSummaryTokens bounds the summary output: the digest must stay
	// small so the compressed conversation fits comfortably in the window.
	DefaultMaxSummaryTokens = 2000
)

// EstimateTokens approximates the token count of s. There is no offline
// tokenizer in the pure-Go dependency set (and llama.cpp tokens are
// model-specific), so this is a rune/4 heuristic. The UI labels estimates
// with "≈" and anchors them to the exact usage.prompt_tokens the server
// reports on every model call.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (utf8.RuneCountInString(s) + 3) / 4
}

// PickCutoff returns the index (inclusive) of the last event to summarize,
// leaving everything after it verbatim as the recent tail. The tail always
// starts at the last user-authored message, so the freshest exchange — and
// any tool call/response pairing inside it — survives intact. The bool is
// false when there is nothing worth compressing (no user message, or a single
// exchange).
func PickCutoff(events []*session.Event) (int, bool) {
	lastUser := -1
	userCount := 0
	for i, ev := range events {
		if ev == nil {
			continue
		}
		if ev.Author == "user" {
			userCount++
			lastUser = i
		}
	}
	if lastUser <= 0 || userCount < 2 {
		return 0, false
	}
	return lastUser - 1, true
}

// Transcript renders events as a flat, role-prefixed transcript for the
// summarizer. Thinking parts are omitted; tool calls and results are compacted
// to single lines. When the result exceeds maxChars (<= 0 disables the cap)
// the oldest content is dropped first.
func Transcript(events []*session.Event, maxChars int) string {
	var lines []string
	for _, ev := range events {
		if line := eventLine(ev); line != "" {
			lines = append(lines, line)
		}
	}
	out := strings.Join(lines, "\n")
	if maxChars > 0 && len(out) > maxChars {
		out = "[… earlier events omitted to fit the summary budget …]\n" + out[len(out)-maxChars:]
	}
	return out
}

// eventLine renders one event as a transcript line: a tool call, a tool
// result, or role-prefixed text.
func eventLine(ev *session.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	var calls, results, texts []string
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		switch {
		case p.Thought:
			// Reasoning is hidden from the model; the digest does not need it.
		case p.FunctionCall != nil:
			fc := p.FunctionCall
			args := ""
			if fc.Args != nil {
				if b, err := json.Marshal(fc.Args); err == nil {
					args = " " + string(b)
				}
			}
			calls = append(calls, fc.Name+args)
		case p.FunctionResponse != nil:
			fr := p.FunctionResponse
			res := ""
			if fr.Response != nil {
				if b, err := json.Marshal(fr.Response); err == nil {
					res = string(b)
				}
			}
			results = append(results, fr.Name+" => "+res)
		case p.Text != "":
			texts = append(texts, p.Text)
		}
	}
	switch {
	case len(calls) > 0:
		return "TOOL CALL: " + strings.Join(calls, "\nTOOL CALL: ")
	case len(results) > 0:
		return "TOOL RESULT: " + strings.Join(results, "\nTOOL RESULT: ")
	case len(texts) > 0:
		role := "USER"
		if ev.Content.Role == genai.RoleModel {
			role = "ASSISTANT"
		}
		return role + ": " + strings.Join(texts, "\n")
	default:
		return ""
	}
}

// SummaryPrompt builds the (system, user) messages for a compression model
// call. instruction carries the user's optional /compress guidance ("direct
// the compression"); it is empty for automatic compressions.
func SummaryPrompt(instruction, transcript string) (string, string) {
	system := "You compress a conversation transcript for a coding assistant so it can " +
		"continue working without losing context. Produce ONE information-dense digest, " +
		"in the assistant's own plain voice, that a future you can read instead of the " +
		"original conversation. Preserve, in order of importance: the user's goals and the " +
		"current task; requirements and constraints; decisions and the reasoning behind them; " +
		"files, paths, commands and tools involved and what changed; concrete facts, answers " +
		"and data from tool outputs; errors and how they were resolved; and unresolved or open " +
		"items. Keep names, paths, identifiers, versions and numbers exact. Do not invent " +
		"details. If the transcript already contains an earlier summary, fold it in — do not " +
		"repeat it. Omit pleasantries, restatements and low-signal chatter. Output ONLY the " +
		"digest — no preamble, no headings about the digest itself."
	user := "Compress the conversation below.\n\n" + transcript
	if strings.TrimSpace(instruction) != "" {
		user += "\n\nCompression focus from the user — follow it closely: " + strings.TrimSpace(instruction)
	}
	return system, user
}

// FormatSummary wraps a model-produced digest into the text stored as the
// summary event.
func FormatSummary(digest string) string {
	return SummaryHeader + strings.TrimSpace(digest)
}

// Summarize runs one non-streaming model call that produces the summary text
// for the given system/user prompt. maxOutTokens bounds the output (<= 0 uses
// DefaultMaxSummaryTokens). The first completed (non-partial) response's text
// is returned.
func Summarize(ctx context.Context, m model.LLM, system, user string, maxOutTokens int) (string, error) {
	if m == nil {
		return "", errors.New("compress: no model available")
	}
	if maxOutTokens <= 0 {
		maxOutTokens = DefaultMaxSummaryTokens
	}
	req := &model.LLMRequest{
		Model: m.Name(),
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(system, genai.RoleUser),
			MaxOutputTokens:   int32(maxOutTokens),
		},
		Contents: []*genai.Content{
			genai.NewContentFromText(user, genai.RoleUser),
		},
	}
	for resp, err := range m.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", fmt.Errorf("compress: summarize: %w", err)
		}
		if resp == nil || resp.Partial || resp.Content == nil {
			continue
		}
		var sb strings.Builder
		for _, p := range resp.Content.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				sb.WriteString(p.Text)
			}
		}
		if sb.Len() > 0 {
			return strings.TrimSpace(sb.String()), nil
		}
	}
	return "", errors.New("compress: model returned no summary text")
}
