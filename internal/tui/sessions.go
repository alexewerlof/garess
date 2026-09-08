package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"garess/internal/chat"
	"garess/internal/harness"
)

const (
	// sessionListTimeout bounds the async recent-sessions list load.
	sessionListTimeout = 5 * time.Second
	// sessionLoadTimeout bounds loading one session's view on resume.
	sessionLoadTimeout = 5 * time.Second
	// railMaxSessions caps the recent-session list kept for the rail/picker.
	railMaxSessions = 40
)

// sessionsMsg carries the result of an async recent-session list load.
type sessionsMsg struct {
	list []chat.RecentSession
	err  error
}

// loadSessions asynchronously refreshes the cached recent-session list used by
// the right rail and /sessions. It returns nil when no session service is
// wired (the rail and picker are hidden then).
func (m Model) loadSessions() tea.Cmd {
	if m.sessionSvc == nil {
		return nil
	}
	svc := m.sessionSvc
	userID := m.userID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), sessionListTimeout)
		defer cancel()
		list, err := svc.Recent(ctx, harness.AppName, userID, railMaxSessions)
		if err != nil {
			return sessionsMsg{err: err}
		}
		return sessionsMsg{list: list}
	}
}

// handleSessions applies an async list result, keeping the picker/rail view
// live when it is open.
func (m Model) handleSessions(msg sessionsMsg) (tea.Model, tea.Cmd) {
	m.sessions = msg.list
	m.sessionsErr = ""
	if msg.err != nil {
		m.sessionsErr = fmt.Sprintf("sessions: %v", msg.err)
	}
	m.clampSessionsSel()
	if m.sessionsShow || m.railFocused {
		m.updateViewport()
	}
	return m, nil
}

// sessionEntries is the resumable list: every cached session except the one
// currently open (the current session is shown as the rail card instead).
func (m Model) sessionEntries() []chat.RecentSession {
	var out []chat.RecentSession
	for _, s := range m.sessions {
		if s.ID == m.sessionID {
			continue
		}
		out = append(out, s)
	}
	return out
}

// clampSessionsSel keeps the selection within the resumable list.
func (m *Model) clampSessionsSel() {
	if n := len(m.sessionEntries()); n > 0 {
		m.sessionsSel %= n
		if m.sessionsSel < 0 {
			m.sessionsSel = 0
		}
	}
}

// moveSessionsSel steps the selection by d (wrapping).
func (m *Model) moveSessionsSel(d int) {
	n := len(m.sessionEntries())
	if n == 0 {
		m.sessionsSel = 0
		return
	}
	m.sessionsSel = (m.sessionsSel + d + n) % n
}

// sessionsOpen reports whether the /sessions picker is showing (idle only).
func (m Model) sessionsOpen() bool {
	return m.sessionsShow && !m.streaming && !m.confirming && !m.compressing
}

// sessionTimeLabel renders a session id's embedded timestamp compactly
// ("09-05 15:04"); the rail/picker fall back to it when a recent entry has no
// update time.
func sessionTimeLabel(id string) string {
	if len(id) >= 15 && id[8] == 'T' {
		if t, err := time.Parse("20060102T150405", id[:15]); err == nil {
			return t.Format("01-02 15:04")
		}
	}
	return id
}

// sessionTail is the last 4 chars of a session id, to disambiguate sessions
// created in the same minute.
func sessionTail(id string) string {
	if len(id) >= 4 {
		return id[len(id)-4:]
	}
	return id
}

// fit pads (or truncates) a plain — no-ANSI — string to exactly width cells.
func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) > width {
		if width > 1 {
			return string(r[:width-1]) + "…"
		}
		return string(r[:width])
	}
	if len(r) == width {
		return s
	}
	return s + strings.Repeat(" ", width-len(r))
}

// sessionEntryLines renders one session entry as label + preview lines, each
// padded to width. A selected entry paints both rows on the accent background
// (it is drawn before the pad so the background spans the whole row).
func (m Model) sessionEntryLines(r chat.RecentSession, width int, selected bool) []string {
	label := fmt.Sprintf("%s·%s", sessionTimeLabel(r.ID), sessionTail(r.ID))
	if !r.UpdatedAt.IsZero() {
		label = fmt.Sprintf("%s·%s", r.UpdatedAt.Format("01-02 15:04"), sessionTail(r.ID))
	}
	preview := r.Preview
	if preview == "" {
		preview = "(no messages)"
	}
	if selected {
		return []string{
			ui.railSel.Render(fit(label, width)),
			ui.railSel.Render(fit("  "+preview, width)),
		}
	}
	return []string{
		ui.railLabel.Render(fit(label, width)),
		ui.railPreview.Render(fit("  "+preview, width)),
	}
}

// sessionsRows renders the resumable session list (label + preview per entry)
// for the picker or the rail. maxLines bounds the total line count (0 =
// unlimited); entries are kept whole. The highlighted entry is the one the
// selection points at — painted only when the list is interactive (the picker
// is open or the rail has focus).
func (m Model) sessionsRows(maxLines, maxW int) []string {
	var out []string
	addStatus := func(s string) {
		if maxLines > 0 && len(out) >= maxLines {
			return
		}
		out = append(out, ui.railPreview.Render(fit(s, maxW)))
	}
	switch {
	case m.sessions == nil:
		addStatus("loading sessions…")
	case m.sessionsErr != "":
		addStatus(m.sessionsErr)
	case len(m.sessionEntries()) == 0:
		addStatus("no past sessions yet")
	default:
		highlight := m.railFocused || m.sessionsShow
		for i, r := range m.sessionEntries() {
			if maxLines > 0 && len(out)+2 > maxLines {
				break
			}
			out = append(out, m.sessionEntryLines(r, maxW, highlight && i == m.sessionsSel)...)
		}
	}
	return out
}

// sessionsBlock renders the /sessions picker for the conversation live slot.
func (m Model) sessionsBlock() string {
	lines := m.sessionsRows(maxInt(m.conv.height-1, 1), m.contentWidth())
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// enterRailFocus gives keyboard focus to the right rail's session list (the
// composer keeps its text; esc/tab or typing returns focus).
func (m Model) enterRailFocus() (tea.Model, tea.Cmd) {
	m.railFocused = true
	m.sessionsShow = false
	m.clampSessionsSel()
	m.status = "sessions rail: ↑↓ pick · ↵ resume · esc back"
	m.updateViewport()
	return m, nil
}

// exitRailFocus hands focus back to the composer.
func (m *Model) exitRailFocus() {
	m.railFocused = false
	if m.status == "sessions rail: ↑↓ pick · ↵ resume · esc back" {
		m.status = ""
	}
}

// resumeSelectedSession resumes the entry the rail/picker selection points at.
func (m Model) resumeSelectedSession() (tea.Model, tea.Cmd) {
	entries := m.sessionEntries()
	if len(entries) == 0 {
		m.sessionsShow = false
		m.railFocused = false
		m.status = "no past sessions to resume"
		m.updateViewport()
		return m, nil
	}
	i := m.sessionsSel % len(entries)
	return m.resumeSession(entries[i].ID)
}

// resumeSession switches the TUI to a past session: it loads the session's
// model view (compaction-filtered, history-capped — the same transcript the
// runner feeds the model on its next run) into the display and points the
// next runner turn at it. The providers/runners need no rebuild: each Run
// resolves its session through the service.
func (m Model) resumeSession(id string) (tea.Model, tea.Cmd) {
	m.sessionsShow = false
	m.railFocused = false
	svc := m.sessionSvc
	if svc == nil {
		m.err = "session service unavailable — cannot resume"
		return m, nil
	}
	if id == "" || id == m.sessionID {
		m.status = "already in this session"
		m.updateViewport()
		return m, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionLoadTimeout)
	defer cancel()
	events, summaryID, err := svc.SessionView(ctx, harness.AppName, m.userID, id)
	if err != nil {
		m.err = fmt.Sprintf("resume session: %v", err)
		return m, nil
	}

	m.sessionID = id
	m.summaryEventID = summaryID
	m.events = events
	m.ephemeral = nil
	m.assistantActive = false
	m.assistantChunks.reset()
	m.assistantThinking.reset()
	m.thinkingBlock = ""
	m.thinkingFrozen = false
	m.generationPending = false
	m.confirming = false
	m.confirmWrapperIDs = nil
	m.cancelled = false
	m.streamFailed = false
	m.ctxNoAutoAfter = 0
	m.compressIsAuto = false
	m.ctxUsageThisRun = false
	m.ctxLastPrompt = 0
	m.clearSubagents()                   // sub-agent blocks belonged to the previous session's events
	m.ctxEst = m.estimatedPromptTokens() // an estimate until the next model call anchors it
	m.textarea.Reset()
	m.reconcileComposerCursor() // the composer is empty again
	m.rendered = make([]string, 0, len(m.events))
	for _, ev := range m.events {
		m.rendered = append(m.rendered, m.renderEvent(ev))
	}
	m.status = fmt.Sprintf("resumed session from %s · %d messages", sessionTimeLabel(id), len(m.events))
	m.resetView()
	m.updateViewport()
	m.conv.gotoBottom()
	return m, m.loadSessions()
}

// --- right session rail (Phase E) ----------------------------------------

// railRows renders the right session rail column as exactly height rows (it
// sits beside the whole frame): a heading, the current-session card, the
// recent-sessions list (skipping the current session) and a key hint pinned
// to the bottom row.
func (m Model) railRows(height int) []string {
	const inner = railWidth - 2 // divider column + its space
	rows := make([]string, 0, height)
	if height <= 0 {
		return rows
	}
	add := func(s string) { rows = append(rows, fit(s, inner)) }
	add(ui.railTitle.Render("sessions"))
	rows = append(rows, "")
	add(ui.railCurrent.Render("● " + sessionTimeLabel(m.sessionID)))
	if label := m.modelLabel(); label != "" {
		add(ui.railPreview.Render(clipRunes(label, inner)))
	}
	if cr := m.contextReadout(); cr != "" {
		add(ui.railPreview.Render(clipRunes(cr, inner)))
	}
	rows = append(rows, "")
	add(ui.railTitle.Render("past"))
	rows = append(rows, m.sessionsRows(maxInt(height-len(rows)-2, 0), inner)...)

	// Key hint pinned to the bottom row (next to the status line).
	hint := "tab: pick · ↵ resume"
	if m.railFocused || m.sessionsOpen() {
		hint = "↑↓ pick · ↵ resume · esc"
	}
	for len(rows) < height-1 {
		rows = append(rows, "")
	}
	rows = append(rows, fit(ui.railHint.Render(hint), inner))
	if len(rows) > height {
		rows = rows[:height]
	}
	return rows
}

// railFrame lays the frame's rows side by side with the rail column: each left
// row is padded to the content width, then the divider and the rail row are
// appended. The width math runs only while the rail is active (desktop).
func (m Model) railFrame(left string) string {
	lh := strings.Split(left, "\n")
	rail := m.railRows(len(lh))
	leftW := m.contentWidth()
	var b strings.Builder
	b.Grow(len(left) + len(lh)*railWidth)
	for i, l := range lh {
		if w := lipgloss.Width(l); w < leftW {
			l += strings.Repeat(" ", leftW-w)
		}
		b.WriteString(l)
		b.WriteString(ui.railDivider)
		if i < len(rail) {
			b.WriteString(rail[i])
		}
		if i < len(lh)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// wrapFrame adds the right session rail around an already-composed frame when
// it is active; otherwise the frame is returned untouched (no per-frame width
// math on narrow terminals, e.g. the Pi's 118 columns, where the rail is off).
func (m Model) wrapFrame(frame string) string {
	if !m.railActive() {
		return frame
	}
	return m.railFrame(frame)
}

// batchCmds combines cmds, skipping nils (tea.Batch with a nil entry would
// otherwise execute a nil Cmd).
func batchCmds(cmds ...tea.Cmd) tea.Cmd {
	var out []tea.Cmd
	for _, c := range cmds {
		if c != nil {
			out = append(out, c)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	default:
		return tea.Batch(out...)
	}
}
