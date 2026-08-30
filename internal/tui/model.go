// Package tui implements the Claude-Code-style terminal UI for garess. The
// agentic core is a Google ADK runner (see internal/harness): the TUI pumps
// runner events through a goroutine into a channel and renders them, exactly
// like the pre-ADK streaming pipeline. The runner owns persistence — the TUI
// never writes to the session service directly.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"garess/internal/agents"
	"garess/internal/chat"
	"garess/internal/harness"
	"garess/internal/memory"
	"garess/internal/skills"
	"garess/internal/tools"
)

const (
	renderInterval  = 50 * time.Millisecond
	spinnerInterval = 100 * time.Millisecond
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Model is the main Bubble Tea model.
type Model struct {
	providers map[string]*harness.Provider
	current   string
	theme     string
	userID    string
	sessionID string
	memory    *memory.Store
	preamble  *harness.Preamble

	agentsText    string
	agentsSources []agents.Source
	skillsText    string
	skillsSources []skills.Source
	workDir       string

	width  int
	height int

	viewport viewport.Model
	textarea textarea.Model
	md       *markdownRenderer

	// events is the display history (parallel to rendered), fed by the runner.
	events    []*session.Event
	rendered  []string
	ephemeral []string // transient info blocks (command output)

	streaming         bool
	cancelled         bool
	cancel            context.CancelFunc
	adkCh             <-chan adkEvent
	streamBuffer      *strings.Builder
	thinkingBuffer    *strings.Builder
	assistantContent  string
	assistantThinking string
	pendingAssistant  bool
	renderTickActive  bool
	spinnerIdx        int
	streamFailed      bool

	// HITL confirmation mode (ADK tool confirmation round trip).
	confirming        bool
	confirmPrompt     string
	confirmWrapperIDs []string

	showThinking bool // ctrl+t toggles thinking blocks between hidden and visible

	err    string
	status string
}

// adkEvent carries one pumped runner event.
type adkEvent struct {
	ev  *session.Event
	err error
}

// Messages sent to Update.
type (
	spinnerTickMsg struct{}
	renderTickMsg  struct{}
	adkEventMsg    struct {
		evt  adkEvent
		done bool
	}
)

// New builds the model. workDir is the working directory used to discover
// AGENTS.md files. width/height may be 0 until the first resize event.
func New(providers map[string]*harness.Provider, current, theme, userID, sessionID string, mem *memory.Store, preamble *harness.Preamble, workDir string, width, height int) (*Model, error) {
	md, err := newMarkdownRenderer(maxInt(width-4, 40), theme)
	if err != nil {
		return nil, err
	}
	ta := textarea.New()
	ta.Placeholder = "Message garess…  (enter: send · ctrl+j: newline · /help)"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.Focus() // focus must be set before Init (Init runs on a value copy)

	m := &Model{
		providers:      providers,
		current:        current,
		theme:          theme,
		userID:         userID,
		sessionID:      sessionID,
		memory:         mem,
		preamble:       preamble,
		workDir:        workDir,
		width:          width,
		height:         height,
		textarea:       ta,
		md:             md,
		streamBuffer:   &strings.Builder{}, // pointer: the Model is copied by Bubble Tea on every Update
		thinkingBuffer: &strings.Builder{}, // pointer: see streamBuffer
	}
	m.loadAgents()
	m.loadSkills()
	m.viewport = viewport.New(maxInt(width-4, 20), 1)
	m.layout()
	m.renderAll()
	return m, nil
}

// Init starts the cursor blink and the status spinner.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinnerTickMsg{} }),
	)
}

// Update dispatches messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.rebuildRenderer()
		m.layout()
		m.renderAll()
		return m, nil

	case spinnerTickMsg:
		m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
		if m.streaming {
			return m, tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinnerTickMsg{} })
		}
		return m, nil

	case adkEventMsg:
		return m.handleADK(msg)

	case renderTickMsg:
		m.renderTickActive = false
		m.flushStream()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	default:
		if !m.streaming && !m.confirming {
			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(msg)
			return m, cmd
		}
		return m, nil
	}
}

// handleADK processes one pumped runner event.
func (m Model) handleADK(msg adkEventMsg) (tea.Model, tea.Cmd) {
	if msg.evt.err != nil {
		if !m.cancelled {
			m.err = msg.evt.err.Error()
		}
		m.cancelled = false
		m.streamFailed = true
		return m.finishStreaming()
	}
	if msg.done {
		return m.finishStreaming()
	}
	ev := msg.evt.ev
	if ev == nil || ev.Content == nil {
		return m, waitForADK(m.adkCh)
	}

	if ev.Partial {
		// Streaming delta: buffer for the 50ms render tick.
		for _, p := range ev.Content.Parts {
			if p.Thought {
				m.thinkingBuffer.WriteString(p.Text)
			} else if p.Text != "" {
				m.streamBuffer.WriteString(p.Text)
			}
		}
		next := waitForADK(m.adkCh)
		if m.renderTickActive {
			return m, next
		}
		m.renderTickActive = true
		return m, tea.Batch(next, tea.Tick(renderInterval, func(time.Time) tea.Msg { return renderTickMsg{} }))
	}

	// Completed event (user echo, model response, tool call/result, or a
	// confirmation request). The final model event supersedes any streamed
	// partials, so clear the in-flight buffers.
	m.pendingAssistant = false
	m.assistantContent = ""
	m.assistantThinking = ""
	m.streamBuffer.Reset()
	m.thinkingBuffer.Reset()

	if isConfirmationRequest(ev) {
		m.events = append(m.events, ev)
		m.enterConfirmation(ev)
		m.renderAll()
		return m, waitForADK(m.adkCh)
	}

	m.events = append(m.events, ev)
	m.renderAll()
	return m, waitForADK(m.adkCh)
}

// waitForADK fetches the next event from the pump channel as a Cmd.
func waitForADK(ch <-chan adkEvent) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return adkEventMsg{done: true}
		}
		return adkEventMsg{evt: evt}
	}
}

// handleKey routes key presses.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.confirming {
		switch msg.String() {
		case "y", "Y", "enter":
			return m.answerConfirmation(true)
		case "n", "N", "esc":
			return m.answerConfirmation(false)
		case "ctrl+c":
			return m, tea.Quit
		default:
			return m, nil
		}
	}

	if m.streaming {
		switch msg.String() {
		case "esc", "ctrl+c":
			m.cancelStream()
		case "up", "down", "pgup", "pgdown", "home", "end":
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		case "ctrl+t":
			return m.toggleThinking()
		}
		return m, nil
	}

	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		return m.send()
	case "ctrl+j":
		m.textarea.InsertString("\n")
		return m, nil
	case "ctrl+t":
		return m.toggleThinking()
	case "esc":
		return m, nil
	default:
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd
	}
}

// send submits the composer content as a user message.
func (m Model) send() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.textarea.Value())
	if text == "" {
		return m, nil
	}
	m.textarea.Reset()
	m.ephemeral = nil
	if strings.HasPrefix(text, "/") {
		return m.handleCommand(text)
	}
	return m.startStream(genai.NewContentFromText(text, genai.RoleUser))
}

// startStream kicks off a runner turn. content is either a user message or
// (on the confirmation resume) a FunctionResponse carrying the user's
// decision. Returns the model value produced by startStream — never a fresh
// copy (the gotcha: Bubble Tea stores the model returned by Update).
func (m Model) startStream(content *genai.Content) (tea.Model, tea.Cmd) {
	prov := m.providers[m.current]
	if prov == nil {
		m.err = fmt.Sprintf("unknown provider %q", m.current)
		return m, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.streaming = true
	m.cancelled = false
	m.streamFailed = false
	m.assistantContent = ""
	m.assistantThinking = ""
	m.pendingAssistant = false
	m.streamBuffer.Reset()
	m.thinkingBuffer.Reset()

	ch := make(chan adkEvent, 1)
	go func() {
		defer close(ch)
		it := prov.Runner.Run(ctx, m.userID, m.sessionID, content,
			agent.RunConfig{StreamingMode: agent.StreamingModeSSE},
			runner.WithYieldUserMessage())
		for ev, err := range it {
			select {
			case ch <- adkEvent{ev, err}:
			case <-ctx.Done():
				return
			}
		}
	}()
	m.adkCh = ch
	m.updateViewport()
	return m, waitForADK(ch)
}

// flushStream moves buffered streaming deltas into the live assistant slot.
func (m *Model) flushStream() {
	content := m.streamBuffer.String()
	m.streamBuffer.Reset()
	thinking := m.thinkingBuffer.String()
	m.thinkingBuffer.Reset()
	if content == "" && thinking == "" {
		return
	}
	m.assistantContent += content
	m.assistantThinking += thinking
	m.pendingAssistant = true
	m.renderAll()
	m.updateViewport()
}

// finishStreaming finalizes the current run. If the run ended because a tool
// requested confirmation, the TUI stays in confirmation mode waiting for the
// user's y/n.
func (m *Model) finishStreaming() (tea.Model, tea.Cmd) {
	m.streaming = false
	m.cancel = nil
	m.adkCh = nil
	m.pendingAssistant = false
	m.assistantContent = ""
	m.assistantThinking = ""
	m.streamBuffer.Reset()
	m.thinkingBuffer.Reset()
	if m.confirming {
		m.updateViewport()
		return m, nil
	}
	if !m.cancelled && !m.streamFailed {
		m.renderAll()
	}
	m.updateViewport()
	return m, nil
}

// --- HITL confirmation ----------------------------------------------------

// isConfirmationRequest reports whether the event carries an
// adk_request_confirmation function call.
func isConfirmationRequest(ev *session.Event) bool {
	if ev.Content == nil {
		return false
	}
	for _, p := range ev.Content.Parts {
		if p.FunctionCall != nil && p.FunctionCall.Name == toolconfirmation.FunctionCallName {
			return true
		}
	}
	return false
}

// enterConfirmation records the pending wrapper calls and shows a prompt.
func (m *Model) enterConfirmation(ev *session.Event) {
	m.confirming = true
	m.confirmWrapperIDs = nil
	var hints []string
	for _, p := range ev.Content.Parts {
		if p.FunctionCall == nil || p.FunctionCall.Name != toolconfirmation.FunctionCallName {
			continue
		}
		m.confirmWrapperIDs = append(m.confirmWrapperIDs, p.FunctionCall.ID)
		if orig, err := toolconfirmation.OriginalCallFrom(p.FunctionCall); err == nil {
			hints = append(hints, fmt.Sprintf("`%s(%s)`", orig.Name, formatArgs(orig.Args)))
		}
	}
	if len(hints) > 0 {
		m.confirmPrompt = "Approve " + strings.Join(hints, ", ") + "?"
	} else {
		m.confirmPrompt = "Approve this tool call?"
	}
}

// answerConfirmation resumes the run with the user's decision.
func (m Model) answerConfirmation(approved bool) (tea.Model, tea.Cmd) {
	m.confirming = false
	parts := make([]*genai.Part, 0, len(m.confirmWrapperIDs))
	for _, id := range m.confirmWrapperIDs {
		parts = append(parts, &genai.Part{
			FunctionResponse: &genai.FunctionResponse{
				Name:     toolconfirmation.FunctionCallName,
				ID:       id,
				Response: map[string]any{"confirmed": approved},
			},
		})
	}
	m.confirmWrapperIDs = nil
	content := genai.NewContentFromParts(parts, genai.RoleUser)
	return m.startStream(content)
}

// cancelStream stops the current response (user pressed esc).
func (m *Model) cancelStream() {
	m.cancelled = true
	if m.cancel != nil {
		m.cancel()
	}
}

// handleCommand processes slash commands.
func (m Model) handleCommand(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(line)
	cmd := fields[0]
	switch cmd {
	case "/help":
		m.addInfo(helpText)
	case "/quit":
		return m, tea.Quit
	case "/new":
		return m.newSession()
	case "/model":
		if len(fields) < 2 {
			m.addInfo(fmt.Sprintf("Current: **%s** (%s).\n\nProviders: %s.",
				m.current, m.providers[m.current].Model, providerNames(m.providers)))
			return m, nil
		}
		return m.switchProvider(fields[1])
	case "/notes":
		return m.handleNotes(fields[1:])
	case "/agents":
		return m.handleAgents(fields[1:])
	case "/skills":
		return m.handleSkills(fields[1:])
	case "/tools":
		return m.handleTools(fields[1:])
	default:
		m.err = fmt.Sprintf("unknown command %q — type /help", cmd)
	}
	return m, nil
}

func (m Model) handleAgents(args []string) (tea.Model, tea.Cmd) {
	if len(args) > 0 && args[0] == "reload" {
		return m.reloadAgents()
	}
	return m.showAgents()
}

func (m Model) handleSkills(args []string) (tea.Model, tea.Cmd) {
	if len(args) > 0 && args[0] == "reload" {
		return m.reloadSkills()
	}
	return m.showSkills()
}

func (m Model) handleTools(args []string) (tea.Model, tea.Cmd) {
	policy := tools.DefaultPolicy()
	var sb strings.Builder
	sb.WriteString("**Tool policy**\n\n")
	if len(policy.Allow) == 0 && len(policy.Ask) == 0 && len(policy.Deny) == 0 {
		sb.WriteString("No regex rules are configured. Tools are allowed by default.\n\n")
	} else {
		if len(policy.Deny) > 0 {
			sb.WriteString("- deny: " + strings.Join(policy.Deny, ", ") + "\n")
		}
		if len(policy.Ask) > 0 {
			sb.WriteString("- ask: " + strings.Join(policy.Ask, ", ") + "\n")
		}
		if len(policy.Allow) > 0 {
			sb.WriteString("- allow: " + strings.Join(policy.Allow, ", ") + "\n")
		}
	}
	sb.WriteString("\n**Built-ins**\n\n")
	for _, name := range harness.ToolNames() {
		fmt.Fprintf(&sb, "- `%s`\n", name)
	}
	m.addInfo(sb.String())
	return m, nil
}

func (m Model) showAgents() (tea.Model, tea.Cmd) {
	if len(m.agentsSources) == 0 {
		m.addInfo("No instruction files (AGENTS.md / SYSTEM.md) apply here.\n\nCreate one at the project root (AGENTS.md or SYSTEM.md), in `~/.agents/AGENTS.md`, or in `~/.config/garess/SYSTEM.md`, then run `/agents reload`.")
		return m, nil
	}
	var sb strings.Builder
	sb.WriteString("**AGENTS.md / SYSTEM.md in effect**\n\n")
	for _, s := range m.agentsSources {
		fmt.Fprintf(&sb, "- `%s` (%s)\n", s.Path, s.Scope)
	}
	if m.agentsText != "" {
		sb.WriteString("\n---\n\n")
		sb.WriteString(m.agentsText)
	}
	m.addInfo(sb.String())
	return m, nil
}

func (m Model) reloadAgents() (tea.Model, tea.Cmd) {
	sources, err := agents.Discover(m.workDir)
	if err != nil {
		m.err = fmt.Sprintf("reload instruction files: %v", err)
		return m, nil
	}
	text, err := agents.Build(m.workDir)
	if err != nil {
		m.err = fmt.Sprintf("reload instruction files: %v", err)
		return m, nil
	}
	m.agentsSources = sources
	m.agentsText = text
	m.preamble.Set(m.buildPreamble())
	m.status = fmt.Sprintf("AGENTS.md / SYSTEM.md reloaded (%d file(s) in effect)", len(sources))
	return m, nil
}

func (m Model) showSkills() (tea.Model, tea.Cmd) {
	if len(m.skillsSources) == 0 {
		m.addInfo("No skills apply here.\n\nCreate a skill directory under `.garess/skills/` or `~/.config/garess/skills/`, then run `/skills reload`.")
		return m, nil
	}
	var sb strings.Builder
	sb.WriteString("**Skills in effect**\n\n")
	for _, s := range m.skillsSources {
		fmt.Fprintf(&sb, "- `%s` (%s · %s)\n", s.Name, s.Scope, s.Path)
	}
	if m.skillsText != "" {
		sb.WriteString("\n---\n\n")
		sb.WriteString(m.skillsText)
	}
	m.addInfo(sb.String())
	return m, nil
}

func (m Model) reloadSkills() (tea.Model, tea.Cmd) {
	sources, err := skills.Discover(m.workDir)
	if err != nil {
		m.err = fmt.Sprintf("reload skills: %v", err)
		return m, nil
	}
	text, err := skills.Build(m.workDir)
	if err != nil {
		m.err = fmt.Sprintf("reload skills: %v", err)
		return m, nil
	}
	m.skillsSources = sources
	m.skillsText = text
	m.preamble.Set(m.buildPreamble())
	m.status = fmt.Sprintf("skills reloaded (%d file(s) in effect)", len(sources))
	return m, nil
}

// loadAgents discovers AGENTS.md/SYSTEM.md files for the working directory.
// Best-effort: failures are surfaced in the status bar, not fatal.
func (m *Model) loadAgents() {
	sources, err := agents.Discover(m.workDir)
	if err != nil {
		m.err = fmt.Sprintf("instruction files: %v", err)
		return
	}
	text, err := agents.Build(m.workDir)
	if err != nil {
		m.err = fmt.Sprintf("instruction files: %v", err)
		return
	}
	m.agentsSources = sources
	m.agentsText = text
	m.syncPreamble()
}

func (m *Model) loadSkills() {
	sources, err := skills.Discover(m.workDir)
	if err != nil {
		m.err = fmt.Sprintf("skills: %v", err)
		return
	}
	text, err := skills.Build(m.workDir)
	if err != nil {
		m.err = fmt.Sprintf("skills: %v", err)
		return
	}
	m.skillsSources = sources
	m.skillsText = text
	m.syncPreamble()
}

// syncPreamble pushes the combined instruction text to the shared holder the
// agents' InstructionProvider reads on every run.
func (m *Model) syncPreamble() {
	if m.preamble != nil {
		m.preamble.Set(m.buildPreamble())
	}
}

func (m *Model) buildPreamble() string {
	var parts []string
	if m.agentsText != "" {
		parts = append(parts, "Follow the instructions in the AGENTS.md and SYSTEM.md files below:\n\n"+m.agentsText)
	}
	if m.skillsText != "" {
		parts = append(parts, "Use the installed skills below:\n\n"+m.skillsText)
	}
	return strings.Join(parts, "\n\n---\n\n")
}

func (m Model) newSession() (tea.Model, tea.Cmd) {
	m.sessionID = chat.NewSessionID()
	m.events = nil
	m.rendered = nil
	m.ephemeral = nil
	m.status = "new session started"
	m.updateViewport()
	return m, nil
}

func (m Model) switchProvider(arg string) (tea.Model, tea.Cmd) {
	name, model := arg, ""
	if i := strings.Index(arg, "/"); i > 0 {
		name, model = arg[:i], arg[i+1:]
	}
	prov, ok := m.providers[name]
	if !ok {
		m.err = fmt.Sprintf("unknown provider %q — providers: %s", name, providerNames(m.providers))
		return m, nil
	}
	if model != "" {
		// Model override: rebuild the provider's runner against the same
		// session service. The stored provider already carries its own model,
		// so overrides apply to this session only by swapping the current.
		m.providers[name] = prov
		m.current = name
		m.status = fmt.Sprintf("switched to %s · %s", name, prov.Model)
		m.addInfo(fmt.Sprintf("Now using provider **%s**, model **%s**.", name, prov.Model))
		return m, nil
	}
	m.current = name
	m.status = fmt.Sprintf("switched to %s · %s", name, prov.Model)
	m.addInfo(fmt.Sprintf("Now using provider **%s**, model **%s**.", name, prov.Model))
	return m, nil
}

func (m Model) handleNotes(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		return m.listNotes()
	}
	switch args[0] {
	case "list":
		return m.listNotes()
	case "read":
		if len(args) < 2 {
			m.err = "usage: /notes read <name>"
			return m, nil
		}
		return m.readNote(args[1])
	case "write":
		global := false
		rest := args[1:]
		if len(rest) > 0 && rest[0] == "-g" {
			global = true
			rest = rest[1:]
		}
		if len(rest) < 2 {
			m.err = "usage: /notes write [-g] <name> <text>"
			return m, nil
		}
		return m.writeNote(rest[0], strings.Join(rest[1:], " "), global)
	case "rm", "delete":
		if len(args) < 2 {
			m.err = "usage: /notes rm <name>"
			return m, nil
		}
		return m.deleteNote(args[1])
	default:
		m.err = "usage: /notes list|read <name>|write [-g] <name> <text>|rm <name>"
		return m, nil
	}
}

func (m Model) listNotes() (tea.Model, tea.Cmd) {
	notes, err := m.memory.List()
	if err != nil {
		m.err = err.Error()
		return m, nil
	}
	if len(notes) == 0 {
		m.addInfo("No memory notes yet. Write one with `/notes write <name> <text>`.")
		return m, nil
	}
	var sb strings.Builder
	sb.WriteString("**Memory notes**\n\n")
	for _, n := range notes {
		fmt.Fprintf(&sb, "- `%s` (%s · %s)\n  `%s`\n", n.Name, n.Scope, n.ModTime.Format("2006-01-02"), n.Path)
	}
	m.addInfo(sb.String())
	return m, nil
}

func (m Model) readNote(name string) (tea.Model, tea.Cmd) {
	content, local, err := m.memory.Read(name)
	if err != nil {
		m.err = fmt.Sprintf("read note %q: %v", name, err)
		return m, nil
	}
	scope := "global"
	if local {
		scope = "local"
	}
	m.addInfo(fmt.Sprintf("**%s** (%s)\n\n%s", name, scope, content))
	return m, nil
}

func (m Model) writeNote(name, content string, global bool) (tea.Model, tea.Cmd) {
	if err := m.memory.Write(name, content, global); err != nil {
		m.err = err.Error()
		return m, nil
	}
	scope := "local"
	if global {
		scope = "global"
	}
	m.status = fmt.Sprintf("note %q written (%s)", name, scope)
	return m, nil
}

func (m Model) deleteNote(name string) (tea.Model, tea.Cmd) {
	if err := m.memory.Delete(name, false); err != nil {
		m.err = err.Error()
		return m, nil
	}
	m.status = fmt.Sprintf("note %q deleted", name)
	return m, nil
}

// addInfo renders an ephemeral markdown block into the viewport.
func (m *Model) addInfo(md string) {
	rendered, err := m.md.Render(md)
	if err != nil {
		rendered = md
	}
	m.ephemeral = append(m.ephemeral, ui.info.Render(rendered))
	m.updateViewport()
}

// --- rendering -----------------------------------------------------------

// renderAll rebuilds the cached ANSI history from the collected events, plus
// any in-flight streaming assistant slot.
func (m *Model) renderAll() {
	m.rendered = m.rendered[:0]
	for _, ev := range m.events {
		m.rendered = append(m.rendered, m.renderEvent(ev))
	}
	if m.pendingAssistant {
		m.rendered = append(m.rendered, m.renderAssistant(m.assistantContent, m.assistantThinking))
	}
	m.updateViewport()
}

func (m *Model) renderEvent(ev *session.Event) string {
	if ev.Content == nil {
		return ""
	}
	switch {
	case hasFunctionResponse(ev.Content):
		return m.renderFunctionResponse(ev.Content)
	case hasFunctionCall(ev.Content):
		return m.renderFunctionCall(ev.Content)
	case ev.Content.Role == string(genai.RoleUser):
		return m.renderUser(textOf(ev.Content))
	default:
		return m.renderAssistant(textOf(ev.Content), thoughtOf(ev.Content))
	}
}

func hasFunctionCall(c *genai.Content) bool {
	for _, p := range c.Parts {
		if p.FunctionCall != nil {
			return true
		}
	}
	return false
}

func hasFunctionResponse(c *genai.Content) bool {
	for _, p := range c.Parts {
		if p.FunctionResponse != nil {
			return true
		}
	}
	return false
}

func textOf(c *genai.Content) string {
	var b strings.Builder
	for _, p := range c.Parts {
		if p.Text != "" && !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func thoughtOf(c *genai.Content) string {
	var b strings.Builder
	for _, p := range c.Parts {
		if p.Text != "" && p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// renderUser renders a user message.
func (m *Model) renderUser(text string) string {
	body := ui.userBody.Width(maxInt(m.width-6, 20)).Render(text)
	return ui.userLabel.Render("You") + "\n" + body
}

// renderAssistant renders an assistant reply, optionally prefixed by a
// collapsible thinking block (hidden behind a header unless showThinking is on).
func (m *Model) renderAssistant(md, thinking string) string {
	body, err := m.md.Render(md)
	if err != nil {
		body = md
	}
	if strings.TrimSpace(thinking) == "" {
		return body
	}
	return m.renderThinking(thinking) + "\n\n" + body
}

// renderThinking renders a model thinking block: a dimmed header plus, when
// shown, the reasoning text indented underneath. Hidden by default.
func (m *Model) renderThinking(thinking string) string {
	header := "▶ thinking — ctrl+t to show"
	if m.showThinking {
		header = "▼ thinking"
	}
	var sb strings.Builder
	sb.WriteString(ui.thinkingHeader.Render(header))
	if m.showThinking {
		sb.WriteString("\n")
		sb.WriteString(ui.thinkingBody.Width(maxInt(m.width-6, 20)).Render(thinking))
	}
	return sb.String()
}

// renderFunctionCall renders a model function-call event (a tool request) as
// a tool block. Pending confirmation wrappers render as a waiting marker.
func (m *Model) renderFunctionCall(c *genai.Content) string {
	var sb strings.Builder
	for _, p := range c.Parts {
		if p.FunctionCall == nil {
			continue
		}
		fc := p.FunctionCall
		if fc.Name == toolconfirmation.FunctionCallName {
			sb.WriteString("**⏳ Awaiting your approval**\n\n")
			continue
		}
		sb.WriteString("**Tool call**\n\n")
		fmt.Fprintf(&sb, "- tool: `%s`\n", fc.Name)
		if len(fc.Args) > 0 {
			sb.WriteString("- args:\n")
			for k, v := range fc.Args {
				fmt.Fprintf(&sb, "  - `%s`: `%v`\n", k, v)
			}
		}
	}
	return m.renderMD(sb.String())
}

// renderFunctionResponse renders a tool-result event.
func (m *Model) renderFunctionResponse(c *genai.Content) string {
	var sb strings.Builder
	for _, p := range c.Parts {
		if p.FunctionResponse == nil {
			continue
		}
		fr := p.FunctionResponse
		sb.WriteString("**Tool result**\n\n")
		fmt.Fprintf(&sb, "- tool: `%s`\n", fr.Name)
		if len(fr.Response) > 0 {
			if out, ok := fr.Response["output"]; ok {
				fmt.Fprintf(&sb, "\n```text\n%v\n```", out)
			} else if errStr, ok := fr.Response["error"]; ok {
				fmt.Fprintf(&sb, "\n**Error**\n\n```text\n%v\n```", errStr)
			} else {
				fmt.Fprintf(&sb, "\n```json\n%s\n```", formatJSON(fr.Response))
			}
		}
	}
	return m.renderMD(sb.String())
}

func (m *Model) renderMD(md string) string {
	rendered, err := m.md.Render(md)
	if err != nil {
		return md
	}
	return rendered
}

// toggleThinking flips thinking-block visibility (Pi-style: ctrl+t).
func (m Model) toggleThinking() (tea.Model, tea.Cmd) {
	m.showThinking = !m.showThinking
	m.refresh()
	if m.showThinking {
		m.status = "thinking visible — ctrl+t to hide"
	} else {
		m.status = "thinking hidden — ctrl+t to show"
	}
	return m, nil
}

// refresh re-renders the full history plus any in-flight assistant message,
// e.g. after toggling thinking visibility.
func (m *Model) refresh() {
	m.renderAll()
	m.updateViewport()
}

func (m *Model) updateViewport() {
	parts := make([]string, 0, len(m.rendered)+len(m.ephemeral))
	parts = append(parts, m.rendered...)
	parts = append(parts, m.ephemeral...)
	m.viewport.SetContent(strings.Join(parts, "\n\n"))
	m.viewport.GotoBottom()
}

func (m *Model) rebuildRenderer() {
	md, err := newMarkdownRenderer(maxInt(m.width-4, 40), m.theme)
	if err != nil {
		return
	}
	m.md = md
}

func (m *Model) layout() {
	const (
		headerH   = 1
		statusH   = 1
		composerH = 3
	)
	w := maxInt(m.width-4, 20)
	vpH := maxInt(m.height-headerH-composerH-statusH, 1)
	m.viewport.Width = w
	m.viewport.Height = vpH
	m.textarea.SetWidth(maxInt(m.width-6, 20))
	m.textarea.SetHeight(composerH - 1)
}

// View composes the screen.
func (m Model) View() string {
	return lipgloss.JoinVertical(
		lipgloss.Left,
		m.header(),
		m.viewport.View(),
		m.composer(),
		m.statusLine(),
	)
}

func (m Model) header() string {
	prov := m.providers[m.current]
	model := ""
	if prov != nil {
		model = prov.Model
	}
	return ui.header.Render(fmt.Sprintf(" garess · %s · %s ", m.current, model))
}

func (m Model) composer() string {
	return ui.composer.Render(m.textarea.View())
}

func (m Model) statusLine() string {
	var parts []string
	if m.confirming {
		parts = append(parts, ui.confirm.Render("⚠ "+m.confirmPrompt+"  (y approve · n deny)"))
	} else if m.streaming {
		parts = append(parts, ui.streaming.Render(spinnerFrames[m.spinnerIdx]+" thinking…  (esc to stop)"))
	}
	if m.err != "" {
		parts = append(parts, ui.err.Render("⚠ "+m.err))
	}
	if m.status != "" {
		parts = append(parts, ui.status.Render(m.status))
	}
	parts = append(parts, ui.status.Render("enter send · ctrl+j newline · /help · ctrl+c quit"))
	return lipgloss.JoinHorizontal(lipgloss.Left, parts...)
}

// --- helpers -------------------------------------------------------------

func providerNames(m map[string]*harness.Provider) string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	return formatJSON(args)
}

func formatJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

const helpText = "**garess commands**\n\n" +
	"- `/help` — this help\n" +
	"- `/new` — start a new session\n" +
	"- `/quit` — exit\n" +
	"- `/model <name>` — switch provider (`/model deepseek/deepseek-chat`)\n" +
	"- `/notes list` — list memory notes\n" +
	"- `/notes read <name>` — show a note (local shadows global)\n" +
	"- `/notes write [-g] <name> <text>` — save a note (`-g` = global)\n" +
	"- `/notes rm <name>` — delete a local note\n" +
	"- `/agents` — show the AGENTS.md / SYSTEM.md files in effect\n" +
	"- `/agents reload` — re-read AGENTS.md / SYSTEM.md from disk\n" +
	"- `/skills` — show installed skills in effect\n" +
	"- `/skills reload` — re-read skills from disk\n" +
	"- `/tools` — show the built-in tool policy and available tools\n\n" +
	"**Keys**\n\n" +
	"- `enter` — send\n" +
	"- `ctrl+j` — insert newline\n" +
	"- `ctrl+t` — show/hide the model's thinking\n" +
	"- `y` / `n` — approve / deny a tool that asks for confirmation\n" +
	"- `esc` — stop the current response (or deny a confirmation)\n" +
	"- `ctrl+c` — quit"
