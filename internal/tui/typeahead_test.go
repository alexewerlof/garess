package tui

// Tests for type-ahead: the composer stays editable while the harness is busy
// (a run is streaming), and a prompt queued with Enter is auto-sent when the
// run finishes cleanly (or handed back to the composer when it is cancelled
// or fails).

import (
	"context"
	"iter"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"

	"garess/internal/tools"
)

// busyTypingModel returns an idle typing model (focused composer) forced into
// the streaming "harness busy" state.
func busyTypingModel(t *testing.T) *Model {
	m := idleTypingModel(t, latencyScenario{name: "typeahead", width: 100, height: 30})
	m.streaming = true
	return m
}

// keyModel routes one key through handleKey and returns the resulting model.
func keyModel(m *Model, key tea.KeyMsg) *Model {
	mm, _ := m.handleKey(key)
	switch v := mm.(type) {
	case Model:
		return &v
	case *Model:
		return v
	default:
		return m
	}
}

// typeKeys types a string into the model through handleKey, one rune per key.
func typeKeys(m *Model, s string) *Model {
	for _, r := range s {
		m = keyModel(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// TestComposerEditsWhileStreaming: typing (and editing) works while the
// harness is busy — this is the core regression this feature fixes.
func TestComposerEditsWhileStreaming(t *testing.T) {
	m := busyTypingModel(t)
	m = typeKeys(m, "next prompt")
	if got := m.textarea.Value(); got != "next prompt" {
		t.Fatalf("composer value while streaming = %q, want %q", got, "next prompt")
	}
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyBackspace})
	if got := m.textarea.Value(); got != "next promp" {
		t.Errorf("composer after backspace = %q, want %q", got, "next promp")
	}
	if !m.streaming {
		t.Error("typing must not end the streaming run")
	}
	if m.queuedContent != nil {
		t.Error("mere typing must not queue anything")
	}
}

// TestEnterWhileStreamingQueuesPrompt: Enter during a run clears the composer
// and queues the drafted prompt for auto-send when the run finishes.
func TestEnterWhileStreamingQueuesPrompt(t *testing.T) {
	m := busyTypingModel(t)
	m = typeKeys(m, "next question")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.queuedContent == nil || m.queuedText != "next question" {
		t.Fatalf("queued = (%v, %q), want the drafted prompt", m.queuedContent != nil, m.queuedText)
	}
	if got := m.textarea.Value(); got != "" {
		t.Errorf("composer should clear on queue, got %q", got)
	}
	if !m.streaming {
		t.Error("queueing must not end the streaming run")
	}
}

// TestEnterWhileStreamingIgnoresEmptyAndCommands: Enter with an empty
// composer or a command line does not queue (commands never run mid-turn).
func TestEnterWhileStreamingIgnoresEmptyAndCommands(t *testing.T) {
	m := busyTypingModel(t)
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.queuedContent != nil {
		t.Error("empty composer must not queue a prompt")
	}
	m = typeKeys(m, "/notes list")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.queuedContent != nil {
		t.Error("command text must not queue while streaming")
	}
	if got := m.textarea.Value(); got != "/notes list" {
		t.Errorf("command draft should stay in the composer, got %q", got)
	}
}

// TestEnterWhileStreamingKeepsDraftWhenAlreadyQueued: a second Enter while a
// prompt is already queued must not silently overwrite the first — the new
// draft stays in the composer.
func TestEnterWhileStreamingKeepsDraftWhenAlreadyQueued(t *testing.T) {
	m := busyTypingModel(t)
	m = typeKeys(m, "first")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeKeys(m, "second")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.queuedText != "first" {
		t.Errorf("queued text = %q, want the first prompt kept", m.queuedText)
	}
	if got := m.textarea.Value(); got != "second" {
		t.Errorf("second draft should stay in the composer, got %q", got)
	}
}

// TestEscWhileStreamingCancelsButKeepsDraft: esc still cancels the run while
// the composer is being edited, and the draft is preserved.
func TestEscWhileStreamingCancelsButKeepsDraft(t *testing.T) {
	m := busyTypingModel(t)
	m = typeKeys(m, "draft")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEsc})
	if !m.cancelled {
		t.Error("esc during streaming should cancel the run")
	}
	if got := m.textarea.Value(); got != "draft" {
		t.Errorf("esc should keep the draft in the composer, got %q", got)
	}
}

// TestStatusLineShowsQueuedHint: while a prompt is queued the bottom bar says
// so, so the user knows the Enter was not lost.
func TestStatusLineShowsQueuedHint(t *testing.T) {
	m := busyTypingModel(t)
	m = typeKeys(m, "next")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.statusLine(); !strings.Contains(got, "queued") {
		t.Errorf("status line while a prompt is queued = %q, want a queued hint", got)
	}
}

// typeAheadModel plays fixed replies but holds each stream open (no final
// event) for hold before answering, so a test can interact while the run is
// still busy. On context cancel it stops early, like a real interrupted call.
type typeAheadModel struct {
	name    string
	replies []string
	hold    time.Duration
}

func (s *typeAheadModel) Name() string { return s.name }

func (s *typeAheadModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if len(s.replies) == 0 {
			yield(&model.LLMResponse{Content: genai.NewContentFromText("", genai.RoleModel), TurnComplete: true}, nil)
			return
		}
		text := s.replies[0]
		s.replies = s.replies[1:]
		select {
		case <-time.After(s.hold):
		case <-ctx.Done():
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel), TurnComplete: true}, nil)
	}
}

// TestTypeAheadQueuedPromptAutoSends: a prompt typed and queued (Enter) while
// the first run is still streaming is auto-sent when that run finishes, with
// no further keypress — one -> first reply -> two -> second reply.
func TestTypeAheadQueuedPromptAutoSends(t *testing.T) {
	env := newEnv(t)
	sm := &typeAheadModel{name: "fake", replies: []string{"first reply", "second reply"}, hold: 900 * time.Millisecond}
	m := newModel(t, env, tools.Policy{}, sm)

	final := run(t, m, func(prog *tea.Program) {
		// Turn 1: user message; the model holds the stream open for hold.
		typeText(prog, "one")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})

		// While turn 1 is still in flight, type and queue the next prompt.
		time.Sleep(150 * time.Millisecond)
		typeText(prog, "two")
		if evs := sessionEvents(t, env.svc, env.sessID); len(evs) > 1 {
			t.Fatalf("turn 1 already finished before the type-ahead (persisted=%d) — test timing is off", len(evs))
		}
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})

		// No further input: the queued prompt must auto-send after turn 1.
		waitFor(t, func() bool {
			evs := sessionEvents(t, env.svc, env.sessID)
			return len(evs) >= 4 && eventText(evs[len(evs)-1]) == "second reply"
		})
		drain()
		prog.Send(tea.QuitMsg{})
	})

	persisted := sessionEvents(t, env.svc, env.sessID)
	texts := make([]string, 0, len(persisted))
	for _, ev := range persisted {
		texts = append(texts, eventText(ev))
	}
	want := []string{"one", "first reply", "two", "second reply"}
	if !reflect.DeepEqual(texts, want) {
		t.Errorf("persisted texts = %v, want %v", texts, want)
	}
	if final.queuedContent != nil || final.queuedText != "" {
		t.Error("queued prompt should be consumed after auto-send")
	}
	if got := final.textarea.Value(); got != "" {
		t.Errorf("composer should be empty after the auto-sent prompt, got %q", got)
	}
}

// TestTypeAheadPromptRestoredOnCancel: when the busy run is cancelled (esc)
// after a prompt was queued, the queued text is handed back to the composer
// instead of being fired as an unwanted follow-up run.
func TestTypeAheadPromptRestoredOnCancel(t *testing.T) {
	env := newEnv(t)
	sm := &typeAheadModel{name: "fake", replies: []string{"first reply"}, hold: 5 * time.Second}
	m := newModel(t, env, tools.Policy{}, sm)

	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, "one")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})

		time.Sleep(150 * time.Millisecond)
		typeText(prog, "two")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		// Sanity: the prompt really is queued behind the running turn (only
		// the turn-1 echo has persisted so far).
		time.Sleep(100 * time.Millisecond)
		if evs := sessionEvents(t, env.svc, env.sessID); len(evs) > 1 {
			t.Fatalf("turn 1 already finished before the type-ahead (persisted=%d)", len(evs))
		}
		// Cancel the running turn; the queued prompt must NOT auto-send.
		prog.Send(tea.KeyMsg{Type: tea.KeyEsc})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 1 })
		// The run unwinds promptly on cancel. Give it time to settle, then
		// assert nothing beyond the turn-1 echo persisted — an auto-send of
		// the queued prompt would have added a "two" user echo right away
		// (turn 1's own reply can never persist: the hold outlives the test).
		time.Sleep(600 * time.Millisecond)
		if evs := sessionEvents(t, env.svc, env.sessID); len(evs) != 1 {
			t.Fatalf("persisted events after cancel = %d, want only the turn-1 echo", len(evs))
		}
		drain()
		prog.Send(tea.QuitMsg{})
	})

	// The queued text is back in the composer for the user to re-send.
	if got := final.textarea.Value(); got != "two" {
		t.Errorf("queued text should be restored to the composer on cancel, got %q", got)
	}
	if final.queuedContent != nil || final.queuedText != "" {
		t.Error("queued prompt should be cleared once restored")
	}
}
