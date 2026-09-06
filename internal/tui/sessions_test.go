package tui

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"garess/internal/chat"
	"garess/internal/harness"
	"garess/internal/tools"
)

// seedSession writes plain user messages into a concrete chat service session
// so the rail/picker has something to list and resume.
func seedSession(t *testing.T, svc *chat.Service, id string, msgs []string) {
	t.Helper()
	created, err := svc.Create(context.Background(), &session.CreateRequest{
		AppName: harness.AppName, UserID: "local", SessionID: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range msgs {
		if err := svc.AppendEvent(context.Background(), created.Session, &session.Event{
			LLMResponse: model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleUser)},
			Author:      "user",
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// runSize is run() with an explicit initial window size (the rail only
// activates at width >= railMinWidth).
func runSize(t *testing.T, m *Model, w, h int, drive func(*tea.Program)) Model {
	t.Helper()
	prog := tea.NewProgram(m, tea.WithInput(blockingReader{}), tea.WithOutput(io.Discard))
	go func() {
		time.Sleep(150 * time.Millisecond)
		prog.Send(tea.WindowSizeMsg{Width: w, Height: h})
		time.Sleep(100 * time.Millisecond)
		drive(prog)
	}()
	final, err := prog.Run()
	if err != nil {
		t.Fatal(err)
	}
	switch f := final.(type) {
	case Model:
		return f
	case *Model:
		return *f
	default:
		t.Fatalf("unexpected final model type %T", final)
		return Model{}
	}
}

func withSessions(t *testing.T, env *testEnv, m model.LLM) *Model {
	t.Helper()
	svc, ok := env.svc.(*chat.Service)
	if !ok {
		t.Fatalf("env service is %T, not *chat.Service", env.svc)
	}
	return newModelOpts(t, env, tools.Policy{}, m, &Options{SessionService: svc})
}

func TestRailActiveOnlyWideWithService(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake"}
	svc := env.svc.(*chat.Service)

	// Wide + service: rail on.
	m, err := New(map[string]*harness.Provider{"fake": providerFor(t, env, sm)}, "fake", "dark", "local", env.sessID,
		env.mem, env.preamble, t.TempDir(), 160, 40, Options{SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	if !m.railActive() {
		t.Error("rail should be active at width 160 with a session service")
	}
	if got := m.contentWidth(); got != 160-railWidth {
		t.Errorf("contentWidth = %d, want %d", got, 160-railWidth)
	}

	// Narrow: rail off even with a service.
	m2, err := New(map[string]*harness.Provider{"fake": providerFor(t, env, sm)}, "fake", "dark", "local", env.sessID,
		env.mem, env.preamble, t.TempDir(), 100, 30, Options{SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	if m2.railActive() {
		t.Error("rail must stay off below railMinWidth (Pi-like terminals)")
	}

	// Wide but no service: rail off.
	m3, err := New(map[string]*harness.Provider{"fake": providerFor(t, env, sm)}, "fake", "dark", "local", env.sessID,
		env.mem, env.preamble, t.TempDir(), 160, 40)
	if err != nil {
		t.Fatal(err)
	}
	if m3.railActive() {
		t.Error("rail must stay off without a session service")
	}
}

// providerFor builds a bare provider for pure-render tests (no runner run).
func providerFor(t *testing.T, env *testEnv, m model.LLM) *harness.Provider {
	t.Helper()
	return testProvider(t, env.svc, tools.Policy{}, m, env.preamble)
}

func TestRailRendersSessionList(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake"}
	svc := env.svc.(*chat.Service)
	seedSession(t, svc, "20260905T100000-aaaa", []string{"how do rails work?", "and resume?"})

	m, err := New(map[string]*harness.Provider{"fake": providerFor(t, env, sm)}, "fake", "dark", "local", env.sessID,
		env.mem, env.preamble, t.TempDir(), 160, 40, Options{SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the async list load completing (loadSessions -> sessionsMsg).
	cmd := m.loadSessions()
	if cmd == nil {
		t.Fatal("loadSessions returned nil with a service wired")
	}
	msg := cmd().(sessionsMsg)
	if msg.err != nil {
		t.Fatalf("list load: %v", msg.err)
	}
	loaded, _ := m.handleSessions(msg)
	final := loaded.(Model)

	if len(final.sessions) != 1 || final.sessions[0].ID != "20260905T100000-aaaa" {
		t.Fatalf("sessions = %+v, want the seeded session", final.sessions)
	}
	if !strings.Contains(final.sessions[0].Preview, "how do rails work?") {
		t.Errorf("preview = %q", final.sessions[0].Preview)
	}

	view := final.View()
	for _, want := range []string{"sessions", "past", "how do rails work?", "│"} {
		if !strings.Contains(view, want) {
			t.Errorf("rail view missing %q", want)
		}
	}
	// Every rail row must fit within the reserved column: the widest line
	// must not exceed the content width + rail reservation.
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > 160 {
			t.Errorf("frame line %d cells wide overflows the %d-col terminal", lipgloss.Width(line), 160)
		}
	}
}

func TestSessionsPickerResumes(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{textResponse("picked up")}}
	svc := env.svc.(*chat.Service)
	seedSession(t, svc, "past-1", []string{"first question", "second question"})

	m := withSessions(t, env, sm)
	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, "/sessions")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		// Let the async list load land and the picker refresh.
		time.Sleep(200 * time.Millisecond)
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter}) // resume the selected (only) session
		time.Sleep(100 * time.Millisecond)
		prog.Send(tea.QuitMsg{})
	})

	if final.sessionID != "past-1" {
		t.Errorf("sessionID = %q, want the resumed past-1", final.sessionID)
	}
	if final.sessionsShow || final.railFocused {
		t.Error("picker/focus should close after a resume")
	}
	if len(final.events) != 2 {
		t.Fatalf("display events = %d, want the 2 resumed messages", len(final.events))
	}
	if eventText(final.events[0]) != "first question" {
		t.Errorf("first resumed event = %q", eventText(final.events[0]))
	}
}

func TestTabFocusesRailAndEscReturns(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake"}
	svc := env.svc.(*chat.Service)
	seedSession(t, svc, "past-1", []string{"old question"})

	m := withSessions(t, env, sm)
	// Enter the rail via tab at a wide size.
	focused := runSize(t, m, 160, 40, func(prog *tea.Program) {
		time.Sleep(250 * time.Millisecond) // async list load
		prog.Send(tea.KeyMsg{Type: tea.KeyTab})
		time.Sleep(80 * time.Millisecond)
		prog.Send(tea.QuitMsg{})
	})
	if !focused.railFocused {
		t.Error("tab should move focus to the rail when sessions exist")
	}

	// Esc returns focus to the composer and clears the rail hint status.
	m2 := withSessions(t, env, sm)
	escaped := runSize(t, m2, 160, 40, func(prog *tea.Program) {
		time.Sleep(250 * time.Millisecond)
		prog.Send(tea.KeyMsg{Type: tea.KeyTab})
		time.Sleep(80 * time.Millisecond)
		prog.Send(tea.KeyMsg{Type: tea.KeyEsc})
		time.Sleep(80 * time.Millisecond)
		prog.Send(tea.QuitMsg{})
	})
	if escaped.railFocused {
		t.Error("esc should release rail focus")
	}
	if strings.Contains(escaped.status, "sessions rail") {
		t.Errorf("rail hint status not cleared: %q", escaped.status)
	}
}

func TestTabDoesNothingWithNoSessions(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake"}

	m := withSessions(t, env, sm)
	final := runSize(t, m, 160, 40, func(prog *tea.Program) {
		time.Sleep(250 * time.Millisecond) // list load returns empty
		prog.Send(tea.KeyMsg{Type: tea.KeyTab})
		time.Sleep(80 * time.Millisecond)
		prog.Send(tea.QuitMsg{})
	})
	if final.railFocused {
		t.Error("tab must not focus the rail when there is nothing to resume")
	}
}

func TestRailEnterResumesFromList(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake"}
	svc := env.svc.(*chat.Service)
	seedSession(t, svc, "old-session", []string{"rails on the Pi?"})

	m := withSessions(t, env, sm)
	final := runSize(t, m, 160, 40, func(prog *tea.Program) {
		time.Sleep(250 * time.Millisecond)
		prog.Send(tea.KeyMsg{Type: tea.KeyTab}) // focus the rail
		time.Sleep(80 * time.Millisecond)
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter}) // resume the selected session
		time.Sleep(100 * time.Millisecond)
		prog.Send(tea.QuitMsg{})
	})
	if final.sessionID != "old-session" {
		t.Errorf("sessionID = %q, want the rail-resumed old-session", final.sessionID)
	}
	if len(final.events) != 1 || !strings.Contains(eventText(final.events[0]), "rails on the Pi?") {
		t.Errorf("resumed events = %d, want the seeded message", len(final.events))
	}
}

// TestResumeThenContinueAppendsToSession is the end-to-end resume proof: after
// resuming a past session, the next user message runs in THAT session (the
// runner reuses the existing session id) and the transcript grows in place.
func TestResumeThenContinueAppendsToSession(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{textResponse("welcome back")}}
	svc := env.svc.(*chat.Service)
	seedSession(t, svc, "past-1", []string{"old question"})

	m := withSessions(t, env, sm)
	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, "/sessions")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		time.Sleep(200 * time.Millisecond)        // list load
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter}) // resume past-1
		time.Sleep(100 * time.Millisecond)
		typeText(prog, "continue here")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, "past-1")) >= 3 })
		drain()
		prog.Send(tea.QuitMsg{})
	})

	if final.sessionID != "past-1" {
		t.Fatalf("sessionID = %q, want past-1", final.sessionID)
	}
	persisted := sessionEvents(t, env.svc, "past-1")
	if len(persisted) != 3 {
		t.Fatalf("persisted events = %d, want 3 (old + continue + reply)", len(persisted))
	}
	if eventText(persisted[1]) != "continue here" || eventText(persisted[2]) != "welcome back" {
		t.Errorf("persisted tail = %q, %q", eventText(persisted[1]), eventText(persisted[2]))
	}
	// The current (abandoned) session must not receive the resumed exchange.
	if got := sessionEvents(t, env.svc, env.sessID); len(got) != 0 {
		t.Errorf("fresh session unexpectedly has %d events", len(got))
	}
}
