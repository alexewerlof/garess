package tui

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"garess/internal/chat"
	"garess/internal/harness"
	"garess/internal/memory"
	"garess/internal/tools"
)

// blockingReader never returns data or EOF, so the program only receives the
// messages we inject via Program.Send.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) {
	select {}
}

// scriptedModel plays a fixed script of final responses, one per model call,
// emitting text/thought partials before each final response when streaming.
type scriptedModel struct {
	name  string
	turns []*model.LLMResponse
}

func (s *scriptedModel) Name() string { return s.name }

func (s *scriptedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if len(s.turns) == 0 {
			yield(&model.LLMResponse{Content: genai.NewContentFromText("", genai.RoleModel), TurnComplete: true}, nil)
			return
		}
		final := s.turns[0]
		s.turns = s.turns[1:]
		if !stream {
			yield(final, nil)
			return
		}
		for _, p := range final.Content.Parts {
			if p.Text == "" {
				continue
			}
			if p.Thought {
				yield(&model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{{Text: p.Text, Thought: true}}, genai.RoleModel), Partial: true}, nil)
			} else {
				yield(&model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText(p.Text)}, genai.RoleModel), Partial: true}, nil)
			}
		}
		yield(final, nil)
	}
}

func textResponse(s string) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromText(s, genai.RoleModel), TurnComplete: true}
}

func thoughtResponse(thinking, text string) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{
		{Text: thinking, Thought: true},
		{Text: text},
	}, genai.RoleModel), TurnComplete: true}
}

func toolCallResponse(name, id, args string) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{
		{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: mustParseArgs(args)}},
	}, genai.RoleModel), TurnComplete: true}
}

func mustParseArgs(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

// testProvider builds a harness provider wired to a scripted model.
func testProvider(t *testing.T, svc session.Service, policy tools.Policy, m model.LLM, preamble *harness.Preamble) *harness.Provider {
	t.Helper()
	ts, err := tools.BuildTools(nil, "", policy)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := llmagent.New(llmagent.Config{
		Name:  harness.AppName,
		Model: m,
		Tools: ts,
		InstructionProvider: func(ctx agent.ReadonlyContext) (string, error) {
			return preamble.Get()
		},
		BeforeToolCallbacks: []llmagent.BeforeToolCallback{tools.DenyCallback(policy)},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: harness.AppName, Agent: ag, SessionService: svc, AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	return &harness.Provider{Name: "fake", Model: "m", Agent: ag, Runner: r}
}

type testEnv struct {
	svc      session.Service
	sessID   string
	mem      *memory.Store
	preamble *harness.Preamble
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	svc := chat.NewService(filepath.Join(dir, "sessions"), 100)
	mem := memory.New(filepath.Join(dir, "local"), filepath.Join(dir, "global"))
	return &testEnv{
		svc:      svc,
		sessID:   chat.NewSessionID(),
		mem:      mem,
		preamble: harness.NewPreamble(),
	}
}

func newModel(t *testing.T, env *testEnv, policy tools.Policy, m model.LLM) *Model {
	t.Helper()
	prov := testProvider(t, env.svc, policy, m, env.preamble)
	model, err := New(map[string]*harness.Provider{"fake": prov}, "fake", "dark", "local", env.sessID, env.mem, env.preamble, t.TempDir(), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

// run drives the program: initial size, then the drive callback, and waits
// for the program to quit. It returns the final model returned by Run.
func run(t *testing.T, m *Model, drive func(*tea.Program)) Model {
	t.Helper()
	prog := tea.NewProgram(m, tea.WithInput(blockingReader{}), tea.WithOutput(io.Discard))
	go func() {
		time.Sleep(150 * time.Millisecond)
		prog.Send(tea.WindowSizeMsg{Width: 100, Height: 30})
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

func typeText(prog *tea.Program, s string) {
	for _, r := range s {
		prog.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// sessionEvents reads the persisted events for a session from the service.
// It returns nil while the session has not been created yet (polling helper).
func sessionEvents(t *testing.T, svc session.Service, id string) []*session.Event {
	t.Helper()
	resp, err := svc.Get(context.Background(), &session.GetRequest{AppName: harness.AppName, UserID: "local", SessionID: id})
	if err != nil {
		return nil
	}
	var out []*session.Event
	for ev := range resp.Session.Events().All() {
		out = append(out, ev)
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// drain lets the tea program process any events still held by the pump
// goroutine after the persisted state satisfies a condition (persistence
// happens in the runner before the event is delivered to the UI).
func drain() { time.Sleep(300 * time.Millisecond) }

func eventText(ev *session.Event) string {
	if ev.Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range ev.Content.Parts {
		if p.Text != "" && !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func TestChatFlow(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{textResponse("Hello world")}}
	m := newModel(t, env, tools.Policy{}, sm)

	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, "hello")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 2 })
		drain()
		prog.Send(tea.QuitMsg{})
	})

	// Display history: user echo + assistant reply.
	if len(final.events) < 2 {
		t.Fatalf("display events = %d, want >= 2", len(final.events))
	}
	if eventText(final.events[0]) != "hello" {
		t.Errorf("first event text = %q, want user 'hello'", eventText(final.events[0]))
	}
	if eventText(final.events[len(final.events)-1]) != "Hello world" {
		t.Errorf("last event text = %q, want 'Hello world'", eventText(final.events[len(final.events)-1]))
	}

	// Transcript: user + assistant persisted (partials are not).
	persisted := sessionEvents(t, env.svc, env.sessID)
	if len(persisted) != 2 {
		t.Fatalf("persisted events = %d, want 2", len(persisted))
	}
	if eventText(persisted[0]) != "hello" || eventText(persisted[1]) != "Hello world" {
		t.Errorf("persisted = %q, %q", eventText(persisted[0]), eventText(persisted[1]))
	}
}

func TestThinkingToggle(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{thoughtResponse("reasoning here", "answer")}}
	m := newModel(t, env, tools.Policy{}, sm)

	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, "hi")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 2 })
		drain()
		// Toggle thinking visible, then hide again.
		prog.Send(tea.KeyMsg{Type: tea.KeyCtrlT})
		time.Sleep(50 * time.Millisecond)
		prog.Send(tea.QuitMsg{})
	})

	// The assistant event carries a thought part.
	var hasThought bool
	for _, ev := range final.events {
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.Thought {
				hasThought = true
			}
		}
	}
	if !hasThought {
		t.Fatal("expected a thought part in the assistant event")
	}
	// After ctrl+t the thinking text should be rendered.
	if !strings.Contains(strings.Join(final.rendered, "\n"), "reasoning here") {
		t.Error("thinking text not rendered after ctrl+t")
	}
}

func TestToolCallLoop(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{
		toolCallResponse("bash", "call_1", `{"command":"printf tool-ran"}`),
		textResponse("The tool output was: tool-ran"),
	}}
	m := newModel(t, env, tools.Policy{}, sm)

	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, "run a tool")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool {
			evs := sessionEvents(t, env.svc, env.sessID)
			return len(evs) > 0 && eventText(evs[len(evs)-1]) == "The tool output was: tool-ran"
		})
		drain()
		prog.Send(tea.QuitMsg{})
	})

	var gotFC, gotFR, gotFinal bool
	for _, ev := range final.events {
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			switch {
			case p.FunctionCall != nil && p.FunctionCall.Name == "bash":
				gotFC = true
			case p.FunctionResponse != nil && p.FunctionResponse.Name == "bash":
				gotFR = true
				if out, ok := p.FunctionResponse.Response["output"]; !ok || out != "tool-ran" {
					t.Errorf("bash output = %v", p.FunctionResponse.Response)
				}
			case p.Text != "" && !p.Thought && p.Text == "The tool output was: tool-ran":
				gotFinal = true
			}
		}
	}
	if !gotFC || !gotFR || !gotFinal {
		t.Errorf("tool loop incomplete: fc=%v fr=%v final=%v", gotFC, gotFR, gotFinal)
	}
}

func TestConfirmationPrompt(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{
		toolCallResponse("bash", "call_1", `{"command":"printf secret"}`),
		textResponse("approved"),
	}}
	m := newModel(t, env, tools.Policy{Ask: []string{".*secret.*"}}, sm)

	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, "do it")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		// Run 1 ends in a confirmation request; the TUI enters confirmation
		// mode once the wrapper event lands and the run drains.
		waitFor(t, func() bool { return hasConfirmationWrapper(t, env.svc, env.sessID) })
		time.Sleep(200 * time.Millisecond)
		// Approve with y; wait until the resumed run produces the final reply.
		prog.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
		waitFor(t, func() bool {
			evs := sessionEvents(t, env.svc, env.sessID)
			return len(evs) > 0 && eventText(evs[len(evs)-1]) == "approved"
		})
		drain()
		prog.Send(tea.QuitMsg{})
	})

	// The tool must have executed on resume (output present), and the final
	// reply must be rendered. The run-1 placeholder FR carries an error, so
	// only count a FunctionResponse that actually has an output.
	var gotOutput, approved bool
	for _, ev := range final.events {
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			switch {
			case p.FunctionResponse != nil && p.FunctionResponse.Name == "bash":
				if _, ok := p.FunctionResponse.Response["output"]; ok {
					gotOutput = true
				}
			case p.Text != "" && !p.Thought && p.Text == "approved":
				approved = true
			}
		}
	}
	if !gotOutput {
		t.Error("bash tool did not execute on resume")
	}
	if !approved {
		t.Error("final approved reply missing after confirmation resume")
	}
}

func hasConfirmationWrapper(t *testing.T, svc session.Service, id string) bool {
	t.Helper()
	for _, ev := range sessionEvents(t, svc, id) {
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionCall != nil && p.FunctionCall.Name == toolconfirmation.FunctionCallName {
				return true
			}
		}
	}
	return false
}

func TestNewSessionResets(t *testing.T) {
	env := newEnv(t)
	sm := &scriptedModel{name: "fake", turns: []*model.LLMResponse{textResponse("hi")}}
	m := newModel(t, env, tools.Policy{}, sm)

	final := run(t, m, func(prog *tea.Program) {
		typeText(prog, "hello")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		waitFor(t, func() bool { return len(sessionEvents(t, env.svc, env.sessID)) >= 2 })
		// /new starts a fresh session and clears the display history.
		typeText(prog, "/new")
		prog.Send(tea.KeyMsg{Type: tea.KeyEnter})
		time.Sleep(50 * time.Millisecond)
		prog.Send(tea.QuitMsg{})
	})

	if final.sessionID == env.sessID {
		t.Error("session id did not change after /new")
	}
	if len(final.events) != 0 {
		t.Errorf("display events = %d, want 0 after /new", len(final.events))
	}
}
