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
	// streamBatchInterval is how long the pump coalesces streamed deltas before
	// delivering them as one message, so Bubble Tea composes one frame per
	// batch instead of one per token (matches the old 50ms display cadence).
	streamBatchInterval = 50 * time.Millisecond
	// streamBatchCap bounds a single batch under a fast token burst.
	streamBatchCap = 64

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

	conv          *convView // append-only conversation line viewer
	viewCommitted int       // stable parts already appended to conv
	textarea      textarea.Model
	md            *markdownRenderer

	// events is the display history (parallel to rendered), fed by the runner.
	events    []*session.Event
	rendered  []string
	ephemeral []string // transient info blocks (command output)

	streaming         bool
	cancelled         bool
	cancel            context.CancelFunc
	adkCh             <-chan adkEventMsg
	streamBuffer      *strings.Builder // raw text deltas awaiting the next pump batch
	thinkingBuffer    *strings.Builder // raw thinking deltas (hidden by default)
	assistantActive   bool             // an assistant reply is being streamed
	assistantChunks   *streamChunker   // streaming text: completed chunks cached, tail re-rendered
	assistantThinking *streamChunker   // streaming thinking (chunked, rendered only when shown)
	thinkingBlock     string           // frozen thinking block (stable, above the response)
	thinkingFrozen    bool             // thinking finalized once visible text starts
	spinnerIdx        int
	streamFailed      bool
	stats             *tuiStats // GARESS_STATS=1 CPU-time breakdown (nil = disabled)

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
	adkEventMsg    struct {
		evt   adkEvent
		batch []adkEvent // coalesced streaming deltas from the pump
		done  bool
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
		providers:       providers,
		current:         current,
		theme:           theme,
		userID:          userID,
		sessionID:       sessionID,
		memory:          mem,
		preamble:        preamble,
		workDir:         workDir,
		width:           width,
		height:          height,
		textarea:        ta,
		md:              md,
		streamBuffer:    &strings.Builder{}, // pointer: the Model is copied by Bubble Tea on every Update
		thinkingBuffer:  &strings.Builder{}, // pointer: see streamBuffer
		assistantChunks: newStreamChunker(), // pointer: see streamBuffer
		assistantThinking: newStreamChunkerWith(func(s string) string {
			return ui.thinkingBody.Render(s)
		}), // pointer: see streamBuffer
	}
	if statsEnabled() {
		m.stats = newTUIStats()
		go m.stats.reportLoop(5 * time.Second)
	}
	m.loadAgents()
	m.loadSkills()
	m.conv = newConvView(1)
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
		if m.assistantActive {
			// Re-render the streaming slot at the new wrap width.
			m.assistantChunks.rechunk(m.md)
			m.resetView()
			m.updateViewport()
		}
		return m, nil

	case spinnerTickMsg:
		m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
		if m.streaming {
			return m, tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinnerTickMsg{} })
		}
		return m, nil

	case adkEventMsg:
		return m.handleADK(msg)

	case tea.KeyMsg:
		if m.stats != nil {
			// Keystroke handling cost (textarea, scroll, commands). Together
			// with statView this is OUR per-key CPU — Bubble Tea's renderer
			// quantization (~16ms @ 60fps) is on top and is not measured here.
			start := time.Now()
			mm, cmd := m.handleKey(msg)
			m.stats.add(statInput, time.Since(start))
			return mm, cmd
		}
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
	if m.stats != nil {
		start := time.Now()
		defer func() { m.stats.add(statADK, time.Since(start)) }()
	}
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
	// Coalesced streaming deltas from the pump: buffer and flush them now —
	// one frame per batch, on a fixed cadence, instead of once per token.
	if msg.batch != nil {
		m.bufferDeltas(msg.batch)
		m.flushStream()
		return m, waitForADK(m.adkCh)
	}
	ev := msg.evt.ev
	if ev == nil || ev.Content == nil {
		return m, waitForADK(m.adkCh)
	}

	if ev.Partial {
		// Single delta fallback (the pump normally batches these).
		m.bufferDeltas([]adkEvent{{ev: ev}})
		m.flushStream()
		return m, waitForADK(m.adkCh)
	}

	// Completed event (user echo, model response, tool call/result, or a
	// confirmation request). The final model event supersedes any streamed
	// partials, so clear the in-flight buffers. The event is rendered once and
	// appended to the cache — completed history is never re-rendered.
	m.assistantActive = false
	m.assistantChunks.reset()
	m.assistantThinking.reset()
	m.thinkingBlock = ""
	m.thinkingFrozen = false
	m.streamBuffer.Reset()
	m.thinkingBuffer.Reset()

	m.events = append(m.events, ev)
	m.rendered = append(m.rendered, m.renderEvent(ev))

	if isConfirmationRequest(ev) {
		m.enterConfirmation(ev)
	}
	// The final event superseded the streaming chunks; discard the view cache
	// so the next update is a clean full rebuild.
	m.resetView()
	m.updateViewport()
	return m, waitForADK(m.adkCh)
}

// waitForADK fetches the next pumped message (a batch of deltas, a single
// completed event, or the end-of-stream marker) as a Cmd.
func waitForADK(ch <-chan adkEventMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return adkEventMsg{done: true}
		}
		return msg
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
			m.scroll(msg.String())
			return m, nil
		case "ctrl+t":
			return m.toggleThinking()
		}
		return m, nil
	}

	// Scrollback stays available after streaming. Page keys always scroll;
	// arrows scroll too when there is overflow, otherwise they edit the
	// composer.
	switch msg.String() {
	case "pgup", "pgdown", "home", "end":
		m.scroll(msg.String())
		return m, nil
	case "up", "down":
		if m.conv.maxScroll() > 0 {
			m.scroll(msg.String())
			return m, nil
		}
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
	m.assistantActive = false
	m.assistantChunks.reset()
	m.assistantThinking.reset()
	m.thinkingBlock = ""
	m.thinkingFrozen = false
	m.streamBuffer.Reset()
	m.thinkingBuffer.Reset()
	// A new response streams at the bottom; show it even if the user had
	// scrolled up during the previous turn.
	m.conv.gotoBottom()

	ch := make(chan adkEventMsg, 1)
	go func() {
		defer close(ch)
		// Feeder: pull events from the runner iterator without blocking the
		// HTTP read on Bubble Tea's pace (this is the "fill a buffer" half).
		raw := make(chan adkEvent, 16)
		go func() {
			defer close(raw)
			it := prov.Runner.Run(ctx, m.userID, m.sessionID, content,
				agent.RunConfig{StreamingMode: agent.StreamingModeSSE},
				runner.WithYieldUserMessage())
			for ev, err := range it {
				select {
				case raw <- adkEvent{ev: ev, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
		// Coalescer: deliver streaming deltas as batches on a fixed cadence;
		// completed events and errors go through immediately.
		ticker := time.NewTicker(streamBatchInterval)
		defer ticker.Stop()
		var batch []adkEvent
		send := func(msg adkEventMsg) bool {
			select {
			case ch <- msg:
				return true
			case <-ctx.Done():
				return false
			}
		}
		flush := func() bool {
			if len(batch) == 0 {
				return true
			}
			b := batch
			batch = nil
			return send(adkEventMsg{batch: b})
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ae, ok := <-raw:
				if !ok {
					flush() // drain any remaining deltas before signalling done
					return
				}
				if ae.err != nil {
					flush()
					send(adkEventMsg{evt: adkEvent{err: ae.err}})
					return
				}
				if ae.ev == nil || !ae.ev.Partial {
					flush()
					if ae.ev != nil && !send(adkEventMsg{evt: adkEvent{ev: ae.ev}}) {
						return
					}
					continue
				}
				batch = append(batch, adkEvent{ev: ae.ev})
				if len(batch) >= streamBatchCap {
					flush()
				}
			case <-ticker.C:
				flush() // bounded latency even if the model pauses mid-batch
			}
		}
	}()
	m.adkCh = ch
	m.updateViewport()
	return m, waitForADK(ch)
}

// bufferDeltas appends streamed text deltas (from a coalesced pump batch) to
// the pending buffers, keeping thought and visible text separate.
func (m *Model) bufferDeltas(evs []adkEvent) {
	for _, ae := range evs {
		ev := ae.ev
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.Thought {
				m.thinkingBuffer.WriteString(p.Text)
			} else if p.Text != "" {
				m.streamBuffer.WriteString(p.Text)
			}
		}
	}
}

// flushStream moves buffered streaming deltas into the live assistant slot.
// It renders only the small tail chunk each batch: completed chunks and the
// whole session history are cached, so per-batch cost is O(chunk), not
// O(session) — which is what made rendering slow as sessions grew.
func (m *Model) flushStream() {
	if m.stats != nil {
		start := time.Now()
		defer func() { m.stats.add(statFlush, time.Since(start)) }()
	}
	content := m.streamBuffer.String()
	m.streamBuffer.Reset()
	thinking := m.thinkingBuffer.String()
	m.thinkingBuffer.Reset()
	if content == "" && thinking == "" {
		return
	}
	m.assistantActive = true
	if content != "" {
		m.assistantChunks.append(m.md, content)
		m.assistantChunks.render(m.md)
	}
	if thinking != "" {
		m.assistantThinking.append(m.md, thinking)
		m.assistantThinking.render(m.md)
	}
	// Once the model emits visible text, freeze the thinking block as a stable
	// part ABOVE the response, so newly finalized paragraphs stay visible
	// instead of being pushed out of view by the tall live thinking block (the
	// "every paragraph wipes the previous text" bug).
	if content != "" && !m.thinkingFrozen && m.assistantThinking.raw.Len() > 0 {
		m.freezeThinking()
	}
	m.updateViewport()
}

// finishStreaming finalizes the current run. If the run ended because a tool
// requested confirmation, the TUI stays in confirmation mode waiting for the
// user's y/n.
func (m *Model) finishStreaming() (tea.Model, tea.Cmd) {
	m.streaming = false
	m.cancel = nil
	m.adkCh = nil
	m.streamBuffer.Reset()
	m.thinkingBuffer.Reset()
	if m.confirming {
		m.updateViewport()
		return m, nil
	}
	if !m.cancelled && !m.streamFailed {
		// Normal completion: the final event was already rendered once by
		// handleADK; the streaming slot was cleared there too.
		m.renderAll()
	} else {
		// Cancelled/failed: keep the streamed text on screen by finalizing
		// the in-flight tail chunk.
		m.assistantChunks.render(m.md)
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
	m.assistantActive = false
	m.assistantChunks.reset()
	m.assistantThinking.reset()
	m.thinkingBlock = ""
	m.thinkingFrozen = false
	m.resetView()
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

// renderAll rebuilds the cached ANSI history from the collected events. It is
// only used on rare full refreshes (resize, /new, thinking toggle); the hot
// streaming path appends completed events and chunks incrementally instead.
func (m *Model) renderAll() {
	m.rendered = m.rendered[:0]
	for _, ev := range m.events {
		m.rendered = append(m.rendered, m.renderEvent(ev))
	}
	m.resetView()
	m.updateViewport()
}

// assistantChunkCap bounds the re-rendered streaming tail chunk: each batch
// only re-renders up to this many chars, keeping the single-threaded Bubble
// Tea loop responsive on slow (RPi 1) hardware regardless of message size.
const assistantChunkCap = 600

// streamChunker incrementally renders a growing stream: completed chunks are
// rendered once and cached; only the small tail chunk is re-rendered per
// batch. Per-batch cost is O(chunk), not O(message). Text chunks use glamour
// (markdown); the thinking streamer uses a lipgloss renderer instead.
type streamChunker struct {
	chunks      []string            // rendered ANSI of completed chunks
	partial     *strings.Builder    // raw text of the in-flight tail chunk
	partialR    string              // ANSI render of the tail chunk
	raw         *strings.Builder    // full raw text, for one-shot re-chunks (resize)
	renderChunk func(string) string // optional chunk renderer (defaults to glamour)
}

func newStreamChunker() *streamChunker {
	return &streamChunker{
		partial: &strings.Builder{},
		raw:     &strings.Builder{},
	}
}

// newStreamChunkerWith uses a custom chunk renderer instead of glamour, e.g.
// for thinking text which is styled with a lipgloss block.
func newStreamChunkerWith(render func(string) string) *streamChunker {
	c := newStreamChunker()
	c.renderChunk = render
	return c
}

func (c *streamChunker) reset() {
	c.chunks = nil
	c.partial.Reset()
	c.partialR = ""
	c.raw.Reset()
}

func (c *streamChunker) append(md *markdownRenderer, text string) {
	c.raw.WriteString(text)
	c.appendRaw(md, text)
}

func (c *streamChunker) appendRaw(md *markdownRenderer, text string) {
	c.partial.WriteString(text)
	raw := c.partial.String()
	for {
		n := assistantChunkBoundary(raw, assistantChunkCap)
		if n <= 0 {
			break
		}
		chunk := raw[:n]
		c.chunks = append(c.chunks, c.renderOne(md, chunk))
		raw = raw[n:]
	}
	c.partial.Reset()
	c.partial.WriteString(raw)
}

// render re-renders the tail chunk — the only per-batch render work.
func (c *streamChunker) render(md *markdownRenderer) {
	c.partialR = c.renderOne(md, c.partial.String())
}

// rechunk re-renders the whole stream from raw text, e.g. after the terminal
// is resized and the renderer's wrap width changed.
func (c *streamChunker) rechunk(md *markdownRenderer) {
	raw := c.raw.String()
	c.chunks = nil
	c.partial.Reset()
	c.partialR = ""
	c.appendRaw(md, raw)
	c.render(md)
}

// renderOne renders one chunk with the custom renderer, or glamour by default.
func (c *streamChunker) renderOne(md *markdownRenderer, s string) string {
	if c.renderChunk != nil {
		return c.renderChunk(s)
	}
	out, err := md.Render(s)
	if err != nil {
		return strings.TrimRight(s, "\n")
	}
	return out
}

// assistantChunkBoundary returns the byte index just past the longest stable
// prefix of s (bounded by cap), or -1 when no stable boundary exists yet. A
// stable prefix ends at a blank line outside a code fence — a safe place to
// finalize a markdown block — or at the hard chunk cap.
func assistantChunkBoundary(s string, cap int) int {
	if s == "" {
		return -1
	}
	limit := len(s)
	if limit > cap {
		limit = cap
	}
	best := -1
	inFence := false
	idx := 0
	for idx < limit {
		nl := strings.IndexByte(s[idx:limit], '\n')
		if nl < 0 {
			break
		}
		lineEnd := idx + nl
		trimmed := strings.TrimSpace(s[idx:lineEnd])
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		} else if !inFence && trimmed == "" {
			best = lineEnd + 1
		}
		idx = lineEnd + 1
	}
	if best > 0 {
		return best
	}
	if len(s) > cap {
		return limit
	}
	return -1
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
	if m.thinkingFrozen && m.assistantThinking != nil && m.assistantThinking.raw.Len() > 0 {
		// The thinking was already frozen into a stable block above the
		// response; re-render it for the new visibility state.
		m.thinkingBlock = m.renderThinkingBlock()
		m.resetView()
		m.updateViewport()
	} else {
		m.refresh()
	}
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

// stableParts lists every piece of completed (frozen) content in display
// order: completed events, the frozen thinking block (if any), finalized
// streaming chunks, and ephemeral blocks.
func (m *Model) stableParts() []string {
	n := len(m.rendered) + len(m.ephemeral) + len(m.assistantChunks.chunks)
	if m.thinkingBlock != "" {
		n++
	}
	parts := make([]string, 0, n)
	parts = append(parts, m.rendered...)
	if m.thinkingBlock != "" {
		parts = append(parts, m.thinkingBlock)
	}
	parts = append(parts, m.assistantChunks.chunks...)
	parts = append(parts, m.ephemeral...)
	return parts
}

// renderThinkingBlock renders the streamed thinking (a dimmed header plus,
// when shown, the chunked reasoning body). Used both for the live tail and
// for the frozen stable block once the response text starts.
func (m *Model) renderThinkingBlock() string {
	if m.assistantThinking == nil || m.assistantThinking.raw.Len() == 0 {
		return ""
	}
	header := "▶ thinking — ctrl+t to show"
	if m.showThinking {
		header = "▼ thinking"
	}
	var sb strings.Builder
	sb.WriteString(ui.thinkingHeader.Render(header))
	if m.showThinking {
		body := append(append([]string{}, m.assistantThinking.chunks...), m.assistantThinking.partialR)
		sb.WriteString("\n")
		sb.WriteString(strings.Join(body, "\n\n"))
	}
	return sb.String()
}

// freezeThinking moves the streamed thinking into a stable block that sits
// above the response text, then drops it from the live tail. The view is
// rebuilt once so the block lands in the correct display order.
func (m *Model) freezeThinking() {
	m.thinkingBlock = m.renderThinkingBlock()
	m.thinkingFrozen = true
	m.resetView()
}

// streamTail is the live (bounded) part of the streaming assistant slot: the
// thinking block (until frozen) plus the current tail chunk render. Thinking
// is chunked too, so only its small tail is re-rendered per batch — re-
// rendering all of it (as before) made ctrl+t quadratic while a model reasoned.
func (m *Model) streamTail() string {
	var parts []string
	if !m.thinkingFrozen {
		if tb := m.renderThinkingBlock(); tb != "" {
			parts = append(parts, tb)
		}
	}
	if m.assistantChunks.partialR != "" {
		parts = append(parts, m.assistantChunks.partialR)
	}
	return strings.Join(parts, "\n\n")
}

// resetView discards the view line cache so the next updateViewport performs a
// full rebuild. Called on rare full refreshes (resize, /new, thinking toggle,
// stream completion) where the stable content changed shape.
func (m *Model) resetView() {
	m.viewCommitted = 0
	m.conv.reset()
}

// updateViewport keeps the line viewer in sync with the model. Completed
// content is appended once (O(delta)); the live streaming tail is replaced in
// place each tick (O(tail)). This replaced bubbles/viewport.SetContent, which
// re-split the whole conversation on every 50ms tick.
func (m *Model) updateViewport() {
	if m.stats != nil {
		start := time.Now()
		defer func() { m.stats.add(statViewport, time.Since(start)) }()
	}
	stable := m.stableParts()
	if m.viewCommitted > len(stable) {
		// Stable content shrank (chunks replaced by the final event, /new, ...).
		m.viewCommitted = 0
		m.conv.reset()
	}
	for i := m.viewCommitted; i < len(stable); i++ {
		m.conv.appendStable(stable[i]) // O(delta): only new parts are appended
	}
	m.viewCommitted = len(stable)
	if m.assistantActive {
		m.conv.setLive(m.streamTail())
	} else {
		m.conv.clearLive()
	}
	// setLive/clamp keep the user's scroll position; when at the bottom
	// (yOff == 0) new content naturally keeps the view pinned to the bottom.
	// No forced gotoBottom here, so scrollback survives streaming.
}

// scroll handles the streaming scrollback keys (up/down/pgup/pgdown/home/end).
func (m *Model) scroll(k string) {
	switch k {
	case "up":
		m.conv.scrollUp(1)
	case "down":
		m.conv.scrollDown(1)
	case "pgup":
		m.conv.pageUp()
	case "pgdown":
		m.conv.pageDown()
	case "home":
		m.conv.gotoTop()
	case "end":
		m.conv.gotoBottom()
	}
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
	vpH := maxInt(m.height-headerH-composerH-statusH, 1)
	m.conv.height = vpH
	m.textarea.SetWidth(maxInt(m.width-6, 20))
	m.textarea.SetHeight(composerH - 1)
}

// View composes the screen. Bubble Tea calls this after every message, so it
// is a hot path worth measuring with GARESS_STATS=1.
func (m Model) View() string {
	if m.stats == nil {
		return strings.Join([]string{m.header(), m.conv.view(), m.composer(), m.statusLine()}, "\n")
	}
	start := time.Now()
	defer func() { m.stats.add(statView, time.Since(start)) }()
	// Per-component timing (tui-stats-view) — on a slow CPU this shows whether
	// View() is dominated by the composer, the conversation join, etc.
	t0 := time.Now()
	hdr := m.header()
	m.stats.addViewPart(viewPartHeader, time.Since(t0))
	t0 = time.Now()
	cv := m.conv.view()
	m.stats.addViewPart(viewPartConv, time.Since(t0))
	t0 = time.Now()
	comp := m.composer()
	m.stats.addViewPart(viewPartComposer, time.Since(t0))
	t0 = time.Now()
	st := m.statusLine()
	m.stats.addViewPart(viewPartStatus, time.Since(t0))
	// Left-aligned frame stack. Do NOT use lipgloss.JoinVertical here: it
	// splits every block, re-measures the ANSI display width of EVERY line
	// and re-pads the whole frame to a common width on every call — O(frame)
	// per keystroke (~580µs of the ~600µs View on a desktop; ~90ms/frame on a
	// Pi 1 per GARESS_STATS, scaling with conversation size). Nothing in this
	// layout needs a shared width (all blocks are left-aligned and the
	// terminal erases to end-of-line when lines are repainted), so trailing
	// padding is invisible. strings.Join reproduces JoinVertical's line
	// structure exactly (an empty block still contributes one blank line,
	// matching strings.Split("", "\n")) without any width math.
	return strings.Join([]string{hdr, cv, comp, st}, "\n")
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
