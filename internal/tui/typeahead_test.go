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
	if len(m.queued) != 0 {
		t.Error("mere typing must not queue anything")
	}
}

// TestEnterWhileStreamingQueuesPrompt: Enter during a run clears the composer
// and queues the drafted prompt for auto-send when the run finishes.
func TestEnterWhileStreamingQueuesPrompt(t *testing.T) {
	m := busyTypingModel(t)
	m = typeKeys(m, "next question")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := queueTexts(m); !reflect.DeepEqual(got, []string{"next question"}) {
		t.Fatalf("queued = %v, want one drafted prompt", got)
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
	if len(m.queued) != 0 {
		t.Error("empty composer must not queue a prompt")
	}
	m = typeKeys(m, "/notes list")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queued) != 0 {
		t.Error("command text must not queue while streaming")
	}
	if got := m.textarea.Value(); got != "/notes list" {
		t.Errorf("command draft should stay in the composer, got %q", got)
	}
}

// TestEnterWhileStreamingQueuesMultipleInOrder: repeated Enters during a run
// append prompts to the queue in submission order (FIFO — each will send one
// per finished turn).
func TestEnterWhileStreamingQueuesMultipleInOrder(t *testing.T) {
	m := busyTypingModel(t)
	m = typeKeys(m, "first")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeKeys(m, "second")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeKeys(m, "third")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := queueTexts(m); !reflect.DeepEqual(got, []string{"first", "second", "third"}) {
		t.Errorf("queued order = %v, want submission order", got)
	}
	if got := m.textarea.Value(); got != "" {
		t.Errorf("composer should clear after each queue, got %q", got)
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
	m = typeKeys(m, "later")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.statusLine(); !strings.Contains(got, "2 queued") {
		t.Errorf("status line while two prompts are queued = %q, want a count hint", got)
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
	if len(final.queued) != 0 {
		t.Errorf("queue should be consumed after auto-send, still has %v", queueTexts(&final))
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
	if len(final.queued) != 0 {
		t.Errorf("queue should be cleared once restored, still has %v", queueTexts(&final))
	}
}

// queueTexts returns the queued prompt texts in submission order (helper).
func queueTexts(m *Model) []string {
	out := make([]string, 0, len(m.queued))
	for _, q := range m.queued {
		out = append(out, q.text)
	}
	return out
}

// TestRestoreQueuedToComposerJoinsInOrder: handing the queue back after an
// interrupt returns every prompt (in order) to the composer so nothing is
// lost, and clears the queue.
func TestRestoreQueuedToComposerJoinsInOrder(t *testing.T) {
	m := busyTypingModel(t)
	m.queued = []queuedPrompt{{text: "first"}, {text: "second"}, {text: "third"}}
	m.restoreQueuedToComposer()
	if len(m.queued) != 0 {
		t.Error("queue should clear on restore")
	}
	if got := m.textarea.Value(); got != "first\nsecond\nthird" {
		t.Errorf("composer after restore = %q, want all prompts in order", got)
	}
}

// TestPendingPromptsRenderInSubmissionOrder: queued prompts render as "Pending"
// blocks (oldest first) in the pinned region between the conversation and the
// composer, and the conversation viewport shrinks to make room for them.
func TestPendingPromptsRenderInSubmissionOrder(t *testing.T) {
	m := busyTypingModel(t)
	before := m.conv.height
	m = typeKeys(m, "first prompt")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeKeys(m, "second prompt")
	m = keyModel(m, tea.KeyMsg{Type: tea.KeyEnter})

	if got := queueTexts(m); !reflect.DeepEqual(got, []string{"first prompt", "second prompt"}) {
		t.Fatalf("queued order = %v", got)
	}
	block := m.pendingBlock()
	if block == "" {
		t.Fatal("pendingBlock empty with two queued prompts")
	}
	if !strings.Contains(block, "Pending") {
		t.Errorf("pending block should be labelled Pending:\n%s", block)
	}
	iFirst := strings.Index(block, "first prompt")
	iSecond := strings.Index(block, "second prompt")
	if iFirst < 0 || iSecond < 0 || iFirst > iSecond {
		t.Errorf("pending block should list prompts in submission order:\n%s", block)
	}
	// The conversation viewport gave up rows for the pinned pending region.
	if m.conv.height >= before {
		t.Errorf("conv height = %d, want < %d with a pending region", m.conv.height, before)
	}
	// With conversation content the pending region renders below it and above
	// the composer, and the whole frame still fills the terminal exactly.
	m.conv.appendStableZ("hello", zoneUser)
	m.conv.gotoBottom()
	frame := m.frame()
	iConv := strings.Index(frame, "hello")
	iPending := strings.Index(frame, "first prompt")
	iComp := strings.Index(frame, "Message garess…")
	if iConv < 0 || iPending < 0 || iComp < 0 || iConv > iPending || iPending > iComp {
		t.Errorf("frame order should be conversation < pending < composer:\n%s", frame)
	}
	if got := len(strings.Split(frame, "\n")); got != m.height {
		t.Errorf("frame rows = %d, want %d", got, m.height)
	}
	// An empty queue renders nothing and restores the viewport height.
	m.queued = nil
	m.layout()
	if got := m.pendingBlock(); got != "" {
		t.Errorf("pendingBlock should be empty with no queue, got %q", got)
	}
	if m.conv.height != before {
		t.Errorf("conv height after clearing queue = %d, want %d", m.conv.height, before)
	}
}

// TestPendingPromptsRenderEmptyWithoutQueue: with nothing queued the pending
// region adds no rows at all.
func TestPendingPromptsRenderEmptyWithoutQueue(t *testing.T) {
	m := busyTypingModel(t)
	if got := m.pendingBlock(); got != "" {
		t.Errorf("pendingBlock with no queue = %q, want empty", got)
	}
	if len(m.pendingRows(m.pendingMaxRows())) != 0 {
		t.Error("pendingRows with no queue should be empty")
	}
}

// TestTypeAheadMultipleQueuedPromptsSendOnePerTurn: prompts queued while a run
// is busy each auto-send, one per finished turn, in submission order — with no
// further keypresses.
func TestTypeAheadMultipleQueuedPromptsSendOnePerTurn(t *testing.T) {
	env := newEnv(t)
	sm := &typeAheadModel{name: "fake", replies: []string{"reply one", "reply two", "reply three"}, hold: 500 * time.Millisecond}
	m := newModel(t, env, tools.Policy{}, sm)

	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, "one")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		time.Sleep(120 * time.Millisecond)
		typeText(prog, "two")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		typeText(prog, "three")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		if evs := sessionEvents(t, env.svc, env.sessID); len(evs) > 1 {
			t.Fatalf("turn 1 already finished before queueing (persisted=%d)", len(evs))
		}
		// No further input: each queued prompt must auto-send after the
		// previous turn finishes.
		waitFor(t, func() bool {
			evs := sessionEvents(t, env.svc, env.sessID)
			return len(evs) >= 6 && eventText(evs[len(evs)-1]) == "reply three"
		})
		drain()
		prog.Send(tea.QuitMsg{})
	})

	texts := make([]string, 0)
	for _, ev := range sessionEvents(t, env.svc, env.sessID) {
		texts = append(texts, eventText(ev))
	}
	want := []string{"one", "reply one", "two", "reply two", "three", "reply three"}
	if !reflect.DeepEqual(texts, want) {
		t.Errorf("persisted texts = %v, want %v", texts, want)
	}
	if len(final.queued) != 0 {
		t.Errorf("queue should be drained after all auto-sends, still has %v", queueTexts(&final))
	}
}
