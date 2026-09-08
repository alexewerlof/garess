// Sub-agent delegation display (run_subagent). A top-level sub-agent run
// (one invoked directly by the main agent) is tracked from the live status
// stream the harness sink emits: while it runs, the TUI shows a live
// in-conversation row ("▸ <agent> · running <tool> …") as the current
// activity slot; once it finishes, a collapsible block is anchored in the
// conversation right after the ⚙ run_subagent tool call and before its ↳
// result. Collapsed by default (agent name); ctrl+o expands it to the prompt,
// the sub-agent's inner tool calls/results, and its final result.
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"google.golang.org/adk/v2/session"

	"garess/internal/harness"
)

// subAgentMsg carries one live status update from a running sub-agent.
type subAgentMsg struct {
	st harness.SubAgentStatus
}

// waitSubAgent fetches the next sub-agent status as a Cmd (mirrors
// waitForADK for runner events).
func waitSubAgent(ch <-chan harness.SubAgentStatus) tea.Cmd {
	return func() tea.Msg {
		st, ok := <-ch
		if !ok {
			return nil
		}
		return subAgentMsg{st: st}
	}
}

// subAgentRecord is the display state for one top-level sub-agent run.
type subAgentRecord struct {
	sessionID  string
	agent      string
	callID     string // the run_subagent function-call id (block anchor)
	running    bool
	failed     bool
	result     string
	errText    string
	activity   string // latest live activity ("running read_file", …)
	inner      []*session.Event
	prompt     string
	promptSet  bool
	anchor     int    // display index of the ⚙ run_subagent event (-1 = unknown)
	block      string // cached rendered block (avoid re-render per tick)
	blockDirty bool
}

// subFind returns the record for a session id.
func (m *Model) subFind(sessionID string) *subAgentRecord {
	for _, r := range m.subs {
		if r.sessionID == sessionID {
			return r
		}
	}
	return nil
}

// handleSubAgent processes one live status from the harness sink.
func (m Model) handleSubAgent(st harness.SubAgentStatus) (tea.Model, tea.Cmd) {
	rec := m.subFind(st.SessionID)
	if rec == nil {
		rec = &subAgentRecord{
			sessionID:  st.SessionID,
			agent:      st.Agent,
			callID:     st.CallID,
			blockDirty: true,
		}
		m.subs = append(m.subs, rec)
	}
	switch st.Phase {
	case harness.SubAgentStarted:
		rec.running = true
		rec.activity = "starting…"
	case harness.SubAgentTool, harness.SubAgentEvent:
		rec.activity = subAgentActivity(st)
		if st.Event != nil {
			rec.inner = append(rec.inner, st.Event)
		}
		rec.blockDirty = true
	case harness.SubAgentFinished:
		rec.running = false
		rec.result = st.Result
		rec.activity = ""
		rec.blockDirty = true
	case harness.SubAgentFailed:
		rec.running = false
		rec.failed = true
		rec.errText = st.Err
		rec.activity = ""
		rec.blockDirty = true
	}
	if !rec.running {
		// The finished/failed block is stable content anchored mid-conversation;
		// rebuild the view cache so it lands after the ⚙ run_subagent event.
		m.resetView()
	}
	m.updateViewport()
	return m, waitSubAgent(m.subCh)
}

// subAgentActivity derives the live activity label for a status update.
func subAgentActivity(st harness.SubAgentStatus) string {
	if st.Tool != "" {
		return "running " + st.Tool
	}
	return "working…"
}

// subRunning reports whether a top-level sub-agent is currently running (its
// live row owns the conversation's live slot).
func (m Model) subRunning() bool {
	if !m.streaming {
		return false
	}
	for _, r := range m.subs {
		if r.running {
			return true
		}
	}
	return false
}

// subAgentLive renders the live in-conversation row for the running top-level
// sub-agent: "▸ <agent> · running <tool> …" with the spinner.
func (m Model) subAgentLive() string {
	for _, r := range m.subs {
		if r.running {
			act := r.activity
			if act == "" {
				act = "working…"
			}
			return ui.subagentHeader.Render(fmt.Sprintf("▸ %s · %s %s", r.agent, act, spinnerFrames[m.spinnerIdx]))
		}
	}
	return ""
}

// subBlock renders (cached) the collapsible block for a finished/failed
// top-level sub-agent. Collapsed: "▸ <agent>"; expanded (ctrl+o): the prompt,
// the inner tool calls/results, and the final result.
func (m *Model) subBlock(rec *subAgentRecord) string {
	if !rec.blockDirty && rec.block != "" {
		return rec.block
	}
	var sb strings.Builder
	header := "▸"
	if m.subOpen {
		header = "▼"
	}
	line := header + " " + rec.agent
	if !m.subOpen && rec.failed {
		line += " · error"
	}
	sb.WriteString(ui.subagentHeader.Render(line))
	if m.subOpen {
		if p := m.subPrompt(rec); p != "" {
			sb.WriteString("\n")
			sb.WriteString(ui.subagentBody.Render(clipDisplay("task: "+p, maxInt(m.contentWidth()-6, 40), 600)))
		}
		for _, ev := range rec.inner {
			s := m.renderEventInner(ev)
			if s == "" {
				continue
			}
			sb.WriteString("\n")
			sb.WriteString(indentLines(s, 2))
		}
		if rec.failed {
			sb.WriteString("\n")
			sb.WriteString(ui.subagentBody.Render(clipDisplay("error: "+rec.errText, maxInt(m.contentWidth()-6, 40), 2000)))
		} else if rec.result != "" {
			sb.WriteString("\n")
			sb.WriteString(ui.subagentBody.Render(clipDisplay("result: "+rec.result, maxInt(m.contentWidth()-6, 40), 4000)))
		}
	}
	rec.block = strings.TrimRight(sb.String(), "\n")
	rec.blockDirty = false
	return rec.block
}

// subPrompt returns the task text from the run_subagent function call that
// started this run, looking it up in the display history by call id.
func (m *Model) subPrompt(rec *subAgentRecord) string {
	if rec.promptSet {
		return rec.prompt
	}
	if ev := m.subAnchorEvent(rec); ev != nil {
		for _, p := range ev.Content.Parts {
			if p.FunctionCall != nil {
				if t, ok := p.FunctionCall.Args["task"].(string); ok {
					rec.prompt = t
					rec.promptSet = true
					return rec.prompt
				}
			}
		}
	}
	rec.promptSet = true // not found; don't rescan every render
	return ""
}

// subAnchorEvent finds the run_subagent function-call event for a record and
// caches its display index. The event history is append-only within a
// session, so the cached anchor stays valid until /new or resume clears subs.
func (m *Model) subAnchorEvent(rec *subAgentRecord) *session.Event {
	for i, ev := range m.events {
		if id := subCallID(ev); id != "" && id == rec.callID {
			rec.anchor = i
			return ev
		}
	}
	return nil
}

// subCallID returns the function-call id when ev is a run_subagent call.
func subCallID(ev *session.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	for _, p := range ev.Content.Parts {
		if p.FunctionCall != nil && p.FunctionCall.Name == harness.RunSubagentTool {
			return p.FunctionCall.ID
		}
	}
	return ""
}

// toggleSubagents flips sub-agent block expansion (ctrl+o), mirroring
// ctrl+t for thinking blocks.
func (m Model) toggleSubagents() (tea.Model, tea.Cmd) {
	m.subOpen = !m.subOpen
	for _, r := range m.subs {
		r.blockDirty = true
	}
	m.resetView()
	m.updateViewport()
	if m.subOpen {
		m.status = "sub-agent details shown — ctrl+o to collapse"
	} else {
		m.status = "sub-agent details hidden — ctrl+o to expand"
	}
	return m, nil
}

// clearSubagents drops all sub-agent display state (on /new and resume).
func (m *Model) clearSubagents() {
	m.subs = nil
	m.subOpen = false
}

// indentLines prefixes every non-empty line with n spaces so nested sub-agent
// activity reads as a child of its block.
func indentLines(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}
