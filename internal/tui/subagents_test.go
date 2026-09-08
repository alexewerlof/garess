package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"garess/internal/harness"
	"garess/internal/tools"
)

// subFCEvent builds a model function-call display event.
func subFCEvent(id, name, args string) *session.Event {
	return &session.Event{LLMResponse: *toolCallResponse(name, id, args)}
}

// subFREvent builds a model function-response display event.
func subFREvent(name, id string, out string) *session.Event {
	return &session.Event{LLMResponse: model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{
		{FunctionResponse: &genai.FunctionResponse{Name: name, ID: id, Response: map[string]any{"output": out}}},
	}, genai.RoleModel)}}
}

// subScenario builds a model whose display history already contains a
// completed run_subagent tool call and result (⚙ run_subagent … ↳
// run_subagent), ready for live statuses to be fed in.
func subScenario(t *testing.T) Model {
	t.Helper()
	ch := make(chan harness.SubAgentStatus)
	close(ch) // no live pump in pure-render tests; statuses are fed directly
	m := *newModelOpts(t, newEnv(t), tools.Policy{}, &scriptedModel{name: "fake"}, &Options{SubAgentEvents: ch})
	m.streaming = true
	fc := subFCEvent("call_sub_1", harness.RunSubagentTool, `{"agent":"researcher","task":"find the bug"}`)
	fr := subFREvent(harness.RunSubagentTool, "call_sub_1", "the bug is in parse.go")
	m.events = append(m.events, fc, fr)
	m.rendered = append(m.rendered, m.renderEvent(fc), m.renderEvent(fr))
	m.resetView()
	m.updateViewport()
	return m
}

// subStatus builds a status for the researcher scenario.
func subStatus(phase harness.SubAgentPhase) harness.SubAgentStatus {
	return harness.SubAgentStatus{SessionID: "sub_session_1", Agent: "researcher", CallID: "call_sub_1", Phase: phase}
}

// step feeds one live status through handleSubAgent and returns the updated
// model (mirroring what Update does with the returned value).
func step(m Model, st harness.SubAgentStatus) Model {
	mm, _ := m.handleSubAgent(st)
	return mm.(Model)
}

func TestSubAgentLiveRowWhileRunning(t *testing.T) {
	m := subScenario(t)
	if m.subRunning() {
		t.Fatal("no sub-agent should be running before any status")
	}

	// started -> live row appears
	m = step(m, subStatus(harness.SubAgentStarted))
	if !m.subRunning() {
		t.Fatal("sub-agent should be running after started")
	}
	if v := m.View(); !strings.Contains(v, "▸ researcher") {
		t.Errorf("live row missing after start:\n%s", v)
	}

	// inner tool about to run -> live row shows the current tool
	innerFC := subFCEvent("inner_1", "read_file", `{"path":"main.go"}`)
	st := subStatus(harness.SubAgentTool)
	st.Tool = "read_file"
	st.Event = innerFC
	m = step(m, st)
	if v := m.View(); !strings.Contains(v, "running read_file") {
		t.Errorf("live row should show the running tool:\n%s", v)
	}

	// inner tool result -> still running
	st = subStatus(harness.SubAgentEvent)
	st.Event = subFREvent("read_file", "inner_1", "package main")
	m = step(m, st)
	if !m.subRunning() {
		t.Fatal("sub-agent should still be running after an inner event")
	}

	// finished -> live row gone, collapsed block anchored in the history
	st = subStatus(harness.SubAgentFinished)
	st.Result = "the bug is in parse.go"
	m = step(m, st)
	if m.subRunning() {
		t.Fatal("sub-agent should not be running after finished")
	}
	v := m.View()
	if strings.Contains(v, "running read_file") {
		t.Errorf("live row should be gone after finish:\n%s", v)
	}

	// Default collapsed: the block sits between ⚙ run_subagent and ↳.
	idxGear := strings.Index(v, "⚙ run_subagent")
	idxBlock := strings.Index(v, "▸ researcher")
	idxResult := strings.Index(v, "↳ run_subagent")
	if idxGear < 0 || idxBlock < 0 || idxResult < 0 {
		t.Fatalf("view missing expected blocks (gear=%d block=%d result=%d):\n%s", idxGear, idxBlock, idxResult, v)
	}
	if !(idxGear < idxBlock && idxBlock < idxResult) {
		t.Errorf("collapsed block out of order (gear=%d block=%d result=%d):\n%s", idxGear, idxBlock, idxResult, v)
	}
	if strings.Contains(v, "▼ researcher") {
		t.Error("block should be collapsed by default")
	}
	if strings.Contains(v, "task: find the bug") {
		t.Error("prompt must be hidden while collapsed")
	}
}

func TestSubAgentToggleExpands(t *testing.T) {
	m := subScenario(t)
	m = step(m, subStatus(harness.SubAgentStarted))

	innerFC := subFCEvent("inner_1", "read_file", `{"path":"main.go"}`)
	st := subStatus(harness.SubAgentTool)
	st.Tool = "read_file"
	st.Event = innerFC
	m = step(m, st)

	st = subStatus(harness.SubAgentEvent)
	st.Event = subFREvent("read_file", "inner_1", "package main")
	m = step(m, st)

	st = subStatus(harness.SubAgentFinished)
	st.Result = "the bug is in parse.go"
	m = step(m, st)

	// Expand: the prompt, inner tool call/result, and final result show.
	mm, _ := m.toggleSubagents()
	m = mm.(Model)
	if !m.subOpen {
		t.Fatal("toggle should set subOpen")
	}
	v := m.View()
	if !strings.Contains(v, "▼ researcher") {
		t.Errorf("expanded header missing:\n%s", v)
	}
	for _, want := range []string{
		"task: find the bug",
		"⚙ read_file",
		"result: the bug is in parse.go",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("expanded block missing %q:\n%s", want, v)
		}
	}
}

func TestSubAgentFailedBlock(t *testing.T) {
	m := subScenario(t)
	m = step(m, subStatus(harness.SubAgentStarted))
	st := subStatus(harness.SubAgentFailed)
	st.Err = "sub-agent exploded"
	m = step(m, st)

	v := m.View()
	if !strings.Contains(v, "▸ researcher · error") {
		t.Errorf("failed collapsed header missing:\n%s", v)
	}
	mm, _ := m.toggleSubagents()
	m = mm.(Model)
	v = m.View()
	if !strings.Contains(v, "error: sub-agent exploded") {
		t.Errorf("failed expanded body missing:\n%s", v)
	}
}

func TestSubAgentCtrlOKeyToggles(t *testing.T) {
	m := subScenario(t)
	st := subStatus(harness.SubAgentFinished)
	st.Result = "done"
	m = step(m, st)
	if m.subOpen {
		t.Fatal("subOpen should default to false")
	}
	mm, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = mm.(Model)
	if !m.subOpen {
		t.Fatal("ctrl+o should expand sub-agent blocks")
	}
	if v := m.View(); !strings.Contains(v, "▼ researcher") {
		t.Errorf("view not expanded after ctrl+o:\n%s", v)
	}
	mm, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = mm.(Model)
	if m.subOpen {
		t.Fatal("second ctrl+o should collapse sub-agent blocks")
	}
}

// TestSubAgentSinkAbsentRendersPlainBlocks verifies that without a status
// channel the run_subagent tool call renders as an ordinary ⚙/↳ pair (no
// block, no panic).
func TestSubAgentSinkAbsentRendersPlainBlocks(t *testing.T) {
	m := *newModelOpts(t, newEnv(t), tools.Policy{}, &scriptedModel{name: "fake"}, nil)
	m.streaming = true
	fc := subFCEvent("call_x", harness.RunSubagentTool, `{"agent":"a","task":"t"}`)
	fr := subFREvent(harness.RunSubagentTool, "call_x", "ok")
	m.events = append(m.events, fc, fr)
	m.rendered = append(m.rendered, m.renderEvent(fc), m.renderEvent(fr))
	m.resetView()
	m.updateViewport()
	if m.subCh != nil {
		t.Fatal("subCh should be nil without Options.SubAgentEvents")
	}
	v := m.View()
	if !strings.Contains(v, "⚙ run_subagent") || !strings.Contains(v, "↳ run_subagent") {
		t.Errorf("plain ⚙/↳ blocks missing:\n%s", v)
	}
	if strings.Contains(v, "▸ a") {
		t.Errorf("no sub-agent block expected without a channel:\n%s", v)
	}
}
