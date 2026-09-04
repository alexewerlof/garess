package tui

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"

	"garess/internal/chat"
	"garess/internal/compress"
	"garess/internal/tools"
)

// usageResponse is a final assistant response that also reports the exact
// server prompt token count for that call.
func usageResponse(text string, prompt int32) *model.LLMResponse {
	r := textResponse(text)
	r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: prompt}
	return r
}

// recordModel wraps another model and keeps the last request it saw, so tests
// can inspect the summarizer prompt.
type recordModel struct {
	model.LLM
	lastReq *model.LLMRequest
}

func (r *recordModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	r.lastReq = req
	return r.LLM.GenerateContent(ctx, req, stream)
}

// chatSvc recovers the concrete chat service behind a test env.
func chatSvc(t *testing.T, env *testEnv) *chat.Service {
	t.Helper()
	svc, ok := env.svc.(*chat.Service)
	if !ok {
		t.Fatal("test env session service is not *chat.Service")
	}
	return svc
}

// jsonlLineCount counts the session transcript lines (full history, never
// rewritten by compaction).
func jsonlLineCount(t *testing.T, env *testEnv) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(chatSvc(t, env).Dir(), env.sessID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// metaHasCompaction reports whether the session metadata records a compaction.
func metaHasCompaction(t *testing.T, env *testEnv) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(chatSvc(t, env).Dir(), env.sessID+".meta.json"))
	if err != nil {
		return false
	}
	var m struct {
		Compaction *json.RawMessage `json:"compaction"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m.Compaction != nil
}

func viewHasSummaryHead(t *testing.T, env *testEnv) bool {
	evs := sessionEvents(t, env.svc, env.sessID)
	return len(evs) > 0 && strings.HasPrefix(eventText(evs[0]), compress.SummaryHeader)
}

func TestAutoCompressAfterTurn(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{
		textResponse("reply one"),
		textResponse("reply two"),
		textResponse("SUMMARY DIGEST"),
	}}
	// No server usage in this script: the trigger + readout run on the local
	// estimate (the fallback path for servers without include_usage).
	opts := &Options{AutoCompress: true, AutoCompressPct: 80, ContextWindows: map[string]int{"fake": 1200}}
	m := newModelOpts(t, env, tools.Policy{}, sm, opts)
	big1 := strings.Repeat("a", 2000) // ≈500 tokens
	big2 := strings.Repeat("b", 2000) // pushes the estimate past the 80% (960) threshold

	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, big1)
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 2 })
		drain()
		typeText(prog, big2)
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		// The turn-end auto-compress fires after the run; the session view is
		// rewritten to start with the summary event.
		waitFor(t, func() bool { return viewHasSummaryHead(t, env) })
		drain()
		prog.Send(tea.QuitMsg{})
	})

	if final.summaryEventID == "" {
		t.Fatal("expected a summary event after auto-compression")
	}
	if len(final.events) < 3 {
		t.Fatalf("display events = %d, want summary + tail", len(final.events))
	}
	if !strings.HasPrefix(eventText(final.events[0]), compress.SummaryHeader) {
		t.Errorf("first display event should be the summary, got %q", eventText(final.events[0]))
	}
	if !metaHasCompaction(t, env) {
		t.Error("session metadata should record the compaction")
	}
	// The full transcript is preserved: 2 exchanges + the summary event.
	if n := jsonlLineCount(t, env); n != 5 {
		t.Errorf("transcript lines = %d, want 5 (full history kept)", n)
	}
	// The usage estimate dropped below the threshold and is still positive.
	if final.ctxEst <= 0 || final.ctxEst >= 960 {
		t.Errorf("usage estimate after compression = %d, want 0 < est < 960", final.ctxEst)
	}
	if !strings.Contains(final.contextReadout(), "ctx") {
		t.Errorf("context readout missing: %q", final.contextReadout())
	}
	// The context stats are always visible on the status line.
	if !strings.Contains(final.statusLine(), "ctx") {
		t.Errorf("status line missing context stats: %q", final.statusLine())
	}
	// Visual feedback card was shown.
	if !strings.Contains(strings.Join(final.ephemeral, "\n"), "Auto-compressed context") {
		t.Error("expected an auto-compress feedback card in the ephemeral area")
	}
}

func TestContextStatsVisibleAndExactAfterFirstTurn(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{
		usageResponse("reply", 300), // the server reports exact usage
	}}
	opts := &Options{AutoCompress: true, AutoCompressPct: 80, ContextWindows: map[string]int{"fake": 1000}}
	m := newModelOpts(t, env, tools.Policy{}, sm, opts)

	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, "hi")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 2 })
		drain()
		prog.Send(tea.QuitMsg{})
	})

	if final.ctxLastPrompt != 300 {
		t.Fatalf("exact prompt tokens = %d, want 300", final.ctxLastPrompt)
	}
	// Always visible on the status line, right after the first response.
	status := final.statusLine()
	if !strings.Contains(status, "ctx") || !strings.Contains(status, "300/1k") {
		t.Errorf("status line should show ctx stats, got %q", status)
	}
	// Exact server usage is not marked as an estimate.
	if strings.Contains(final.contextReadout(), "≈") {
		t.Errorf("exact usage should not be marked approximate: %q", final.contextReadout())
	}
}

func TestAutoCompressBeforeSendDeferredMessage(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{
		usageResponse("reply one", 3000),
		usageResponse("reply two", 5000), // below 80% of the 10k window
		textResponse("SUMMARY DIGEST"),   // summarizer for the pre-send compression
		textResponse("final answer"),     // the queued message's real reply
	}}
	opts := &Options{AutoCompress: true, AutoCompressPct: 80, ContextWindows: map[string]int{"fake": 10000}}
	m := newModelOpts(t, env, tools.Policy{}, sm, opts)

	// Phase 1: two ordinary exchanges; usage stays under the 8k threshold, so
	// no turn-end auto-compress fires.
	m1 := run(t, m, func(prog *tea.Program) {
		typeText(prog, "hi one")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 2 })
		drain()
		typeText(prog, "hi two")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 4 })
		drain()
		prog.Send(tea.QuitMsg{})
	})
	// Simulate a context that crossed the threshold after a turn whose
	// auto-compression was cancelled: sending must now compress first.
	m1.ctxEst = 9000
	m1.ctxLastPrompt = 9000
	m1.ctxUsageThisRun = false

	// Phase 2: sending a message with the context over the threshold compresses
	// behind the scenes, then the queued message goes out.
	final := run(t, &m1, func(prog *tea.Program) {
		typeText(prog, "final question")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		// Compression runs first, then the deferred message: the view ends with
		// the real assistant reply.
		waitFor(t, func() bool {
			evs := sessionEvents(t, env.svc, env.sessID)
			return len(evs) > 0 && eventText(evs[len(evs)-1]) == "final answer"
		})
		drain()
		prog.Send(tea.QuitMsg{})
	})

	if final.pendingSend != nil {
		t.Error("pending message should have been sent")
	}
	if final.summaryEventID == "" {
		t.Fatal("expected a summary event from the pre-send auto-compress")
	}
	if last := eventText(final.events[len(final.events)-1]); last != "final answer" {
		t.Errorf("last display event = %q, want the deferred message's reply", last)
	}
	// The queued user message itself appears in the display after the summary.
	joined := strings.Join(func() []string {
		var out []string
		for _, ev := range final.events {
			out = append(out, eventText(ev))
		}
		return out
	}(), "\n")
	if !strings.Contains(joined, "final question") {
		t.Error("the queued user message was not sent after compression")
	}
}

func TestManualCompressWithInstructions(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{
		textResponse("reply one"),
		textResponse("reply two"),
		textResponse("digest per focus"),
	}}
	rec := &recordModel{LLM: sm}
	m := newModelOpts(t, env, tools.Policy{}, rec, nil)

	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, "question one")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 2 })
		drain()
		typeText(prog, "question two")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 4 })
		drain()

		typeText(prog, "/compress keep the API design details")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return viewHasSummaryHead(t, env) })
		drain()
		prog.Send(tea.QuitMsg{})
	})

	if final.summaryEventID == "" {
		t.Fatal("expected a summary event from /compress")
	}
	if !strings.HasPrefix(eventText(final.events[0]), compress.SummaryHeader) {
		t.Errorf("first display event = %q, want the summary", eventText(final.events[0]))
	}
	if !strings.Contains(strings.Join(final.ephemeral, "\n"), "Context compressed") {
		t.Error("expected a compression feedback card")
	}
	// The optional instructions must reach the summarizer prompt.
	if rec.lastReq == nil || len(rec.lastReq.Contents) == 0 {
		t.Fatal("summarizer request not captured")
	}
	user := rec.lastReq.Contents[0].Parts[0].Text
	if !strings.Contains(user, "keep the API design details") {
		t.Errorf("instructions missing from the summarizer prompt: %q", user)
	}
	if rec.lastReq.Config == nil || rec.lastReq.Config.SystemInstruction == nil {
		t.Error("summarizer system instruction missing")
	}
	// Full transcript preserved: 4 events + summary.
	if n := jsonlLineCount(t, env); n != 5 {
		t.Errorf("transcript lines = %d, want 5", n)
	}
}

func TestCompactAliasAndNothingToCompress(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{
		textResponse("reply one"),
		textResponse("reply two"),
		textResponse("digest"),
	}}
	rec := &recordModel{LLM: sm}
	m := newModelOpts(t, env, tools.Policy{}, rec, nil)

	final := run(t, m, func(prog *tea.Program) {
		// /compact on an empty session: nothing to compress, no model call.
		typeText(prog, "/compact")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		drain()
		if rec.lastReq != nil {
			t.Fatal("no summarizer call expected on an empty session")
		}
		// Two exchanges, then the /compact alias compresses.
		typeText(prog, "one")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 2 })
		drain()
		typeText(prog, "two")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 4 })
		drain()
		typeText(prog, "/compact")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return viewHasSummaryHead(t, env) })
		drain()
		prog.Send(tea.QuitMsg{})
	})

	if final.summaryEventID == "" {
		t.Fatal("/compact should compress like /compress")
	}
	if rec.lastReq == nil {
		t.Fatal("summarizer was not called for /compact")
	}
}
