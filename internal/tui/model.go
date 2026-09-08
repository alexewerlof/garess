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

	"github.com/charmbracelet/bubbles/cursor"
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
	"garess/internal/compress"
	"garess/internal/config"
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
	skillsSources []skills.Skill
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
	generationPending bool             // a model generation is pending (show the live Thinking slot)
	spinnerIdx        int
	streamFailed      bool
	stats             *tuiStats // GARESS_STATS=1 CPU-time breakdown (nil = disabled)

	// Context usage tracking + compression (see context.go).
	windows         map[string]windowInfo // resolved context window per provider
	ctxWindow       int                   // active context window (tokens)
	ctxApprox       bool                  // window came from the fallback default (≈)
	ctxEst          int                   // estimated current usage
	ctxLastPrompt   int                   // last exact server prompt_tokens
	ctxUsageThisRun bool                  // a model event carried exact usage this run
	ctxNoAutoAfter  int                   // events watermark before the next auto-compress
	autoCompress    bool
	autoCompressPct int
	compressing     bool
	compressIsAuto  bool // wording: "auto-compressing" vs "compressing"
	compressCancel  context.CancelFunc
	pendingSend     *genai.Content // user message queued behind a pre-send auto-compress
	pendingText     string         // original text (restored if compression is cancelled)
	summaryEventID  string         // latest compaction summary event (styled distinctly)

	// Type-ahead while busy: prompts queued (Enter) during a streaming run, in
	// submission order (FIFO). finishStreaming pops the front and auto-sends it
	// on a clean finish; on cancel/failure the queue is handed back to the
	// composer instead of firing runs the user may not want. The queue renders
	// as "Pending" blocks pinned above the composer (pendingRows).
	queued []queuedPrompt

	// HITL confirmation mode (ADK tool confirmation round trip).
	confirming        bool
	confirmPrompt     string
	confirmWrapperIDs []string

	showThinking bool // ctrl+t toggles thinking blocks between hidden and visible

	err    string
	status string

	// Modern chrome / empty state.
	version string // app version (bottom bar); "" hides it

	// Slash-command palette ("/" at the start of the composer).
	paletteShow bool // esc dismisses the palette until the next edit
	paletteSel  int  // index into paletteRows()

	// Right session rail + resume (/sessions). sessionSvc is the concrete
	// chat service behind Options.SessionService; nil hides the rail and the
	// picker. sessions is the cached recent-session list (nil = not loaded).
	sessionSvc   *chat.Service
	sessions     []chat.RecentSession
	sessionsErr  string // last list-load error ("" = none)
	sessionsSel  int    // highlighted entry in the rail / picker
	sessionsShow bool   // /sessions picker is open (conv live slot)
	railFocused  bool   // arrow keys drive the right rail list

	// Sub-agent delegation (run_subagent): subCh carries the live status
	// stream from the harness sink; subs holds per-run display records;
	// subOpen expands/collapses their blocks (ctrl+o).
	subCh   <-chan harness.SubAgentStatus
	subs    []*subAgentRecord
	subOpen bool
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
// AGENTS.md files. width/height may be 0 until the first resize event. opts is
// optional: with no Options the defaults apply (auto-compress on at 80%, no
// configured context windows); pass Options to tune or disable compression.
func New(providers map[string]*harness.Provider, current, theme, userID, sessionID string, mem *memory.Store, preamble *harness.Preamble, workDir string, width, height int, opts ...Options) (*Model, error) {
	o := Options{}
	if len(opts) > 0 {
		o = opts[0]
	}
	autoCompress := true
	if len(opts) > 0 {
		autoCompress = o.AutoCompress
	}
	autoPct := o.AutoCompressPct
	if autoPct <= 0 {
		autoPct = config.DefaultAutoCompressThreshold
	}
	windows := resolveWindows(providers, o)
	cur := windows[current]

	// The session service drives the right rail and /sessions resume; it is
	// type-asserted to the concrete chat service (like the compression flow).
	var sessionSvc *chat.Service
	if s, ok := o.SessionService.(*chat.Service); ok {
		sessionSvc = s
	}
	// The markdown renderer wraps at the content width (terminal width minus
	// the right rail when it is active), so wrapped lines never run under the
	// rail column.
	contentW := width
	if width >= railMinWidth && sessionSvc != nil {
		contentW = width - railWidth
	}
	md, err := newMarkdownRenderer(maxInt(contentW-4, 40), theme)
	if err != nil {
		return nil, err
	}

	resolveUI(theme) // pick the dark/light style set before any render
	ta := textarea.New()
	ta.Prompt = "" // no prompt glyph inside the editor panel
	ta.Placeholder = "Ask garess anything…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	styleTextarea(&ta, theme) // placeholder blends into the panel (no block bg)
	ta.Focus()                // focus must be set before Init (Init runs on a value copy)

	m := &Model{
		providers:       providers,
		current:         current,
		theme:           theme,
		userID:          userID,
		sessionID:       sessionID,
		sessionSvc:      sessionSvc,
		memory:          mem,
		preamble:        preamble,
		workDir:         workDir,
		width:           width,
		height:          height,
		version:         o.Version,
		textarea:        ta,
		md:              md,
		paletteShow:     true,
		streamBuffer:    &strings.Builder{}, // pointer: the Model is copied by Bubble Tea on every Update
		thinkingBuffer:  &strings.Builder{}, // pointer: see streamBuffer
		assistantChunks: newStreamChunker(), // pointer: see streamBuffer
		assistantThinking: newStreamChunkerWith(func(s string) string {
			return ui.thinkingBody.Render(s)
		}), // pointer: see streamBuffer
		windows:         windows,
		ctxWindow:       cur.size,
		ctxApprox:       cur.approx,
		autoCompress:    autoCompress,
		autoCompressPct: autoPct,
		subCh:           o.SubAgentEvents,
	}
	if statsEnabled() {
		m.stats = newTUIStats()
		go m.stats.reportLoop(5 * time.Second)
	}
	// The composer starts empty: hide the block cursor (see
	// reconcileComposerCursor) so no reverse box sits over the placeholder.
	m.reconcileComposerCursor()
	m.loadAgents()
	m.loadSkills()
	m.conv = newConvView(1)
	m.layout()
	m.renderAll()
	return m, nil
}

// Init starts the cursor blink, the status spinner and the async load of the
// recent-session list (for the right rail and /sessions).
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		textarea.Blink,
		tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinnerTickMsg{} }),
	}
	if cmd := m.loadSessions(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.subCh != nil {
		cmds = append(cmds, waitSubAgent(m.subCh))
	}
	return tea.Batch(cmds...)
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
		if m.streaming || m.compressing {
			if m.compressing || (m.streaming && m.generationPending) || m.subRunning() {
				// Animate in-conversation indicators: the compression
				// progress, the live "Thinking" placeholder while we wait
				// for the first deltas of a model generation, and the live
				// sub-agent row while it runs.
				m.updateViewport()
			}
			return m, tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinnerTickMsg{} })
		}
		return m, nil

	case adkEventMsg:
		return m.handleADK(msg)

	case compressMsg:
		return m.handleCompressResult(msg)

	case sessionsMsg:
		return m.handleSessions(msg)

	case subAgentMsg:
		return m.handleSubAgent(msg.st)

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
	m.trackUsage(ev)

	// Track whether a model generation is still pending so the live
	// "Thinking" slot appears at the right times across the tool loop: every
	// tool result is followed by another model call; a model output or a tool
	// request ends the current generation.
	switch {
	case hasFunctionResponse(ev.Content):
		m.generationPending = true
	case hasFunctionCall(ev.Content), ev.Content.Role == string(genai.RoleModel):
		m.generationPending = false
	}

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
		// The composer stays editable while the harness is busy so the user
		// can compose the next prompt mid-turn. Control keys keep their
		// streaming meaning; everything else edits the composer.
		switch msg.String() {
		case "esc", "ctrl+c":
			m.cancelStream()
			return m, nil
		case "pgup", "pgdown", "home", "end":
			m.scroll(msg.String())
			return m, nil
		case "up", "down":
			// Scrollback stays available while streaming; arrows fall through
			// to the composer when there is no overflow (same rule as idle),
			// so the cursor can move within a multi-line draft.
			if m.conv.maxScroll() > 0 {
				m.scroll(msg.String())
				return m, nil
			}
		case "ctrl+t":
			return m.toggleThinking()
		case "ctrl+o":
			return m.toggleSubagents()
		case "ctrl+j":
			// Enter queues (see below), so ctrl+j is the newline key for a
			// multi-line draft — same as the idle composer.
			m.textarea.InsertString("\n")
			return m, m.reconcileComposerCursor()
		case "enter":
			// Type-ahead: Enter queues the drafted prompt; it is sent when
			// this run finishes (see finishStreaming). Commands are not
			// dispatched mid-run — command text stays in the composer.
			return m.queueWhileBusy()
		}
		return m.editComposer(msg)
	}

	// While an auto/manual compression is running, input is gated (esc can
	// cancel it, scroll keys keep working).
	if m.compressing {
		switch msg.String() {
		case "esc", "ctrl+c":
			m.cancelCompression()
		case "up", "down", "pgup", "pgdown", "home", "end":
			m.scroll(msg.String())
			return m, nil
		case "ctrl+t":
			return m.toggleThinking()
		}
		return m, nil
	}

	// Right session rail focus (tab cycles into it): arrows move the selection,
	// enter resumes it, esc/tab hand focus back to the composer, any other key
	// exits the rail and is processed normally below.
	if m.railFocused && m.railActive() {
		switch msg.String() {
		case "up":
			m.moveSessionsSel(-1)
			m.updateViewport()
			return m, nil
		case "down":
			m.moveSessionsSel(+1)
			m.updateViewport()
			return m, nil
		case "home", "pgup":
			m.sessionsSel = 0
			m.updateViewport()
			return m, nil
		case "end", "pgdown":
			if n := len(m.sessionEntries()); n > 0 {
				m.sessionsSel = n - 1
			}
			m.updateViewport()
			return m, nil
		case "enter":
			return m.resumeSelectedSession()
		case "tab", "esc":
			m.exitRailFocus()
			return m, nil
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+t":
			return m.toggleThinking()
		default:
			// Typing hands focus back to the composer; the key is processed
			// by the normal idle path below.
			m.exitRailFocus()
		}
	}

	// /sessions picker (open via the /sessions command): arrows move the
	// selection, enter resumes it, esc dismisses it, and any other key closes
	// the picker and edits the composer.
	if m.sessionsOpen() {
		switch msg.String() {
		case "up":
			m.moveSessionsSel(-1)
			m.updateViewport()
			return m, nil
		case "down":
			m.moveSessionsSel(+1)
			m.updateViewport()
			return m, nil
		case "home", "pgup":
			m.sessionsSel = 0
			m.updateViewport()
			return m, nil
		case "end", "pgdown":
			if n := len(m.sessionEntries()); n > 0 {
				m.sessionsSel = n - 1
			}
			m.updateViewport()
			return m, nil
		case "enter":
			return m.resumeSelectedSession()
		case "tab":
			if m.railActive() {
				// Move the picker into the rail's list (desktop terminals).
				m.sessionsShow = false
				return m.enterRailFocus()
			}
			m.sessionsShow = false
			m.updateViewport()
			return m, nil
		case "esc":
			m.sessionsShow = false
			m.updateViewport()
			return m, nil
		case "ctrl+c":
			return m, tea.Quit
		default:
			// Typing dismisses the picker and edits the composer.
			m.sessionsShow = false
		}
	}

	// Slash-command palette ("/" at the start of the composer): arrows move
	// the selection, tab/enter complete the highlighted command, esc dismisses
	// the list (the typed text is kept). Other keys fall through to editing.
	if m.paletteVisible() {
		switch msg.String() {
		case "up":
			n := len(m.paletteRows())
			m.paletteSel = (m.paletteSel - 1 + n) % n
			m.updateViewport()
			return m, nil
		case "down":
			n := len(m.paletteRows())
			m.paletteSel = (m.paletteSel + 1) % n
			m.updateViewport()
			return m, nil
		case "tab", "enter":
			return m.paletteComplete()
		case "esc":
			m.paletteShow = false
			m.updateViewport()
			return m, nil
		case "ctrl+c":
			return m, tea.Quit
		}
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
		return m, m.reconcileComposerCursor()
	case "ctrl+t":
		return m.toggleThinking()
	case "ctrl+o":
		return m.toggleSubagents()
	case "tab":
		// Tab cycles focus to the right session rail when there is something
		// to resume (desktop terminals only — the rail is inactive on narrow
		// ones like the Pi's 118 columns).
		if m.railActive() && len(m.sessionEntries()) > 0 {
			return m.enterRailFocus()
		}
		return m, nil
	case "esc":
		return m, nil
	default:
		return m.editComposer(msg)
	}
}

// editComposer passes a non-control key to the textarea so the composer edits
// normally. The slash palette only re-arms while idle (it is hidden during
// streaming/compressing), so the list is not refreshed on busy edits.
func (m Model) editComposer(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	prev := m.textarea.Value()
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	if !m.streaming && !m.confirming && !m.compressing &&
		(strings.HasPrefix(prev, "/") || strings.HasPrefix(m.textarea.Value(), "/")) {
		m.paletteShow = true
		m.clampPalette()
		m.updateViewport()
	}
	return m, batchCmds(cmd, m.reconcileComposerCursor())
}

// queuedPrompt is one user prompt waiting for its turn (typed and Entered
// while a run was busy): the text the user drafted plus the user content the
// runner sends when the prompt is popped.
type queuedPrompt struct {
	content *genai.Content
	text    string
}

const (
	// maxQueuedPrompts caps the type-ahead queue so a backlog cannot grow
	// without bound.
	maxQueuedPrompts = 8
	// pendingMaxShown caps how many queued prompts are expanded as individual
	// blocks in the pending region; older ones collapse into a "+N more" row.
	pendingMaxShown = 3
)

// queueWhileBusy is Enter while a run is in flight: the prompt drafted in the
// composer is appended to the type-ahead queue and auto-sent, one per finished
// turn, when the current run finishes (see finishStreaming). Command lines are
// not queued — commands are never dispatched mid-turn, so the draft is left in
// the composer for after. A full queue keeps the draft in the composer too.
func (m Model) queueWhileBusy() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.textarea.Value())
	if text == "" || strings.HasPrefix(text, "/") || len(m.queued) >= maxQueuedPrompts {
		return m, nil
	}
	m.textarea.Reset()
	m.reconcileComposerCursor() // empty again: drop the block cursor
	m.queued = append(m.queued, queuedPrompt{
		content: genai.NewContentFromText(text, genai.RoleUser),
		text:    text,
	})
	// The pending region grew, so the conversation viewport shrinks to match.
	m.layout()
	m.conv.clamp()
	m.updateViewport()
	return m, nil
}

// send submits the composer content as a user message.
func (m Model) send() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.textarea.Value())
	if text == "" {
		return m, nil
	}
	m.textarea.Reset()
	m.reconcileComposerCursor() // empty again: drop the block cursor
	m.ephemeral = nil
	if strings.HasPrefix(text, "/") {
		return dispatchCommand(m, text)
	}
	content := genai.NewContentFromText(text, genai.RoleUser)
	// Auto-compress behind the scenes when the context is already at/over the
	// threshold as the user sends — a backstop, since the post-turn check
	// normally keeps it below. The prompt is queued and sent once compression
	// finishes. The pending message itself is not compressible, so its size
	// does not drive the trigger.
	if m.autoCompress && m.ctxEst >= m.thresholdTokens() {
		return m.startCompression(true, "", content, text)
	}
	return m.startStream(content)
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
	m.generationPending = true // a new model generation is expected
	m.cancelled = false
	m.streamFailed = false
	m.ctxUsageThisRun = false
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
		// Visible text has started: the thinking phase is over (any streamed
		// thoughts freeze as a stable block above the response).
		m.generationPending = false
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
	m.generationPending = false
	m.cancel = nil
	m.adkCh = nil
	m.streamBuffer.Reset()
	m.thinkingBuffer.Reset()
	// A turn persisted events to the current session: refresh the rail/picker
	// list so its preview, order and timestamps stay current.
	refresh := m.loadSessions()
	if m.confirming {
		m.updateViewport()
		return m, refresh
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
	// Servers that cannot report usage (no stream_options.include_usage
	// support): fall back to a local estimate so the readout still reflects
	// conversation growth after every turn.
	if !m.cancelled && !m.streamFailed && !m.ctxUsageThisRun {
		m.ctxEst = m.estimatedPromptTokens()
		m.ctxLastPrompt = 0
	}

	// Type-ahead flush: the front queued prompt (Entered while this run was
	// busy) is auto-sent now that the turn is over — riding any post-turn
	// auto-compress so it fires after compression finishes. Remaining queued
	// prompts stay pending and send one per finished turn. If the run was
	// cancelled or failed, the whole queue is handed back to the composer
	// instead of firing runs the user may not want.
	if len(m.queued) > 0 {
		if m.cancelled || m.streamFailed {
			m.restoreQueuedToComposer()
			m.layout() // the pending region shrank to nothing
			m.conv.clamp()
			m.updateViewport()
			return m, batchCmds(refresh, m.reconcileComposerCursor())
		}
		front := m.queued[0]
		m.queued = m.queued[1:]
		m.layout() // the pending region shrank by the popped prompt
		m.conv.clamp()
		if m.autoCompress && m.ctxEst >= m.thresholdTokens() &&
			len(m.events) >= m.ctxNoAutoAfter {
			mm, cmd := m.startCompression(true, "", front.content, front.text)
			return mm, batchCmds(refresh, cmd)
		}
		mm, cmd := m.startStream(front.content)
		return mm, batchCmds(cmd, refresh)
	}

	// Auto-compress check after EVERY completed turn: a turn's tool outputs
	// can be massive, so re-evaluate here (right after the run persisted
	// them), not only when the user types the next prompt.
	if m.autoCompress && !m.cancelled && !m.streamFailed &&
		m.ctxEst >= m.thresholdTokens() && len(m.events) >= m.ctxNoAutoAfter {
		mm, cmd := m.startCompression(true, "", nil, "")
		return mm, batchCmds(refresh, cmd)
	}
	return m, refresh
}

// restoreQueuedToComposer returns every queued prompt to the composer (in
// order, one per line) and clears the queue. Used when a run is cancelled or
// fails: nothing auto-fires after an interrupt, and the user keeps full
// control of their drafted text. The single-prompt case behaves exactly like
// the original type-ahead restore.
func (m *Model) restoreQueuedToComposer() {
	if len(m.queued) == 0 {
		return
	}
	texts := make([]string, 0, len(m.queued))
	for _, q := range m.queued {
		texts = append(texts, q.text)
	}
	m.queued = nil
	m.textarea.SetValue(strings.Join(texts, "\n"))
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
			fmt.Fprintf(&sb, "- deny: %s\n", strings.Join(policy.Deny, ", "))
		}
		if len(policy.Ask) > 0 {
			fmt.Fprintf(&sb, "- ask: %s\n", strings.Join(policy.Ask, ", "))
		}
		if len(policy.Allow) > 0 {
			fmt.Fprintf(&sb, "- allow: %s\n", strings.Join(policy.Allow, ", "))
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
	m.generationPending = false
	m.summaryEventID = ""
	m.ctxEst = 0
	m.ctxLastPrompt = 0
	m.ctxNoAutoAfter = 0
	m.ctxUsageThisRun = false
	m.compressIsAuto = false
	// Prompts queued behind a busy run belong to the previous session
	// (commands only run idle, so this is normally already flushed).
	m.queued = nil
	// The picker and rail focus describe the previous session; close them.
	m.sessionsShow = false
	m.railFocused = false
	// Sub-agent blocks belong to the previous session's events.
	m.clearSubagents()
	m.layout() // the pending region (if any) changed with the queue
	m.resetView()
	m.status = "new session started"
	m.updateViewport()
	return m, m.loadSessions()
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
		m.applyWindow(name)
		m.status = fmt.Sprintf("switched to %s · %s", name, prov.Model)
		m.addInfo(fmt.Sprintf("Now using provider **%s**, model **%s**.", name, prov.Model))
		return m, nil
	}
	m.current = name
	m.applyWindow(name)
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

// renderEvent renders one completed display event, followed by a trailing
// blank line so consecutive messages breathe (the blank line carries no rail —
// see convView.view).
func (m *Model) renderEvent(ev *session.Event) string {
	s := m.renderEventInner(ev)
	if s == "" {
		return ""
	}
	return s + "\n"
}

func (m *Model) renderEventInner(ev *session.Event) string {
	if ev.Content == nil {
		return ""
	}
	if ev.ID != "" && ev.ID == m.summaryEventID {
		return m.renderSummary(ev)
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

// renderSummary renders the compaction summary event — the head of the
// compacted conversation — as a distinct block rather than a user message.
func (m *Model) renderSummary(ev *session.Event) string {
	body := strings.TrimSpace(strings.TrimPrefix(textOf(ev.Content), compress.SummaryHeader))
	md, err := m.md.Render(body)
	if err != nil {
		md = body
	}
	return ui.summaryHeader.Render("● Earlier conversation compressed") + "\n\n" + md
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

// renderUser renders a user message: a "You" label plus the text. The
// conversation view paints the whole message with a blue left rail.
func (m *Model) renderUser(text string) string {
	body := ui.userBody.Width(maxInt(m.contentWidth()-4, 20)).Render(text)
	return ui.userLabel.Render("You") + "\n" + body
}

// renderAssistant renders an assistant reply, optionally prefixed by a
// collapsible thinking block (hidden behind a header unless showThinking is
// on). The conversation view paints the message with a rose left rail.
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
	header := "Thinking"
	if m.showThinking {
		header = "▼ Thinking"
	}
	var sb strings.Builder
	sb.WriteString(ui.thinkingHeader.Render(header))
	if m.showThinking {
		sb.WriteString("\n")
		sb.WriteString(ui.thinkingBody.Width(maxInt(m.contentWidth()-4, 20)).Render(thinking))
	}
	return sb.String()
}

// renderFunctionCall renders a model function-call event (a tool request) as
// a compact dim internal block (no rail). Pending confirmation wrappers
// render as an actionable waiting marker instead.
func (m *Model) renderFunctionCall(c *genai.Content) string {
	var sb strings.Builder
	for _, p := range c.Parts {
		if p.FunctionCall == nil {
			continue
		}
		fc := p.FunctionCall
		if fc.Name == toolconfirmation.FunctionCallName {
			sb.WriteString(ui.confirm.Render("⏳ awaiting your approval"))
			sb.WriteString("\n")
			continue
		}
		sb.WriteString(ui.toolHeader.Render("⚙ " + fc.Name))
		if len(fc.Args) > 0 {
			sb.WriteString("\n")
			sb.WriteString(ui.toolBody.Render(clipDisplay(formatJSON(fc.Args), maxInt(m.contentWidth()-8, 40), toolDisplayChars)))
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// renderFunctionResponse renders a tool-result event as a compact dim
// internal block (no rail). The body is clipped for display — the model saw
// the full output.
func (m *Model) renderFunctionResponse(c *genai.Content) string {
	var sb strings.Builder
	for _, p := range c.Parts {
		if p.FunctionResponse == nil {
			continue
		}
		fr := p.FunctionResponse
		sb.WriteString(ui.toolHeader.Render("↳ " + fr.Name))
		body := ""
		if out, ok := fr.Response["output"]; ok {
			body = fmt.Sprintf("%v", out)
		} else if errStr, ok := fr.Response["error"]; ok {
			body = "error: " + fmt.Sprintf("%v", errStr)
		} else {
			body = formatJSON(fr.Response)
		}
		if body != "" {
			sb.WriteString("\n")
			sb.WriteString(ui.toolBody.Render(clipDisplay(body, maxInt(m.contentWidth()-8, 40), toolDisplayChars)))
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// toolDisplayChars caps a tool block's body in the conversation view (huge
// results stay visible in the transcript and to the model, but don't flood
// the display).
const toolDisplayChars = 6000

// clipDisplay truncates a block to at most totalMax runes and each line to
// lineMax runes, so dim internal tool blocks never overflow the terminal.
func clipDisplay(s string, lineMax, totalMax int) string {
	body := clipRunes(s, totalMax)
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if r := []rune(l); len(r) > lineMax {
			lines[i] = string(r[:lineMax]) + "…"
		}
	}
	return strings.Join(lines, "\n")
}

// clipRunes truncates s to at most n runes (append "…" when cut).
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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

// zoneForEvent returns the rail zone for a completed display event: user
// messages and assistant replies get rails; tool calls/results, summaries and
// anything else are plain (internal, dim).
func (m *Model) zoneForEvent(ev *session.Event) lineZone {
	if ev == nil || ev.Content == nil {
		return zonePlain
	}
	if ev.ID != "" && ev.ID == m.summaryEventID {
		return zonePlain
	}
	switch {
	case hasFunctionCall(ev.Content), hasFunctionResponse(ev.Content):
		return zonePlain
	case ev.Content.Role == string(genai.RoleUser):
		return zoneUser
	default:
		return zoneAssistant
	}
}

// stableParts lists every piece of completed (frozen) content in display
// order with its zone: completed events, the frozen thinking block (if any),
// finalized streaming chunks, and ephemeral blocks.
func (m *Model) stableParts() []taggedPart {
	n := len(m.rendered) + len(m.ephemeral) + len(m.assistantChunks.chunks)
	if m.thinkingBlock != "" {
		n++
	}
	parts := make([]taggedPart, 0, n+len(m.subs))
	// Finished top-level sub-agent runs anchor their block to the ⚙ run_subagent
	// function call that started them (keyed by its call id).
	byCall := make(map[string]*subAgentRecord, len(m.subs))
	for _, r := range m.subs {
		if !r.running {
			byCall[r.callID] = r
		}
	}
	for i, r := range m.rendered {
		var ev *session.Event
		if i < len(m.events) {
			ev = m.events[i]
		}
		parts = append(parts, taggedPart{text: r, zone: m.zoneForEvent(ev)})
		if ev != nil {
			if id := subCallID(ev); id != "" {
				if rec := byCall[id]; rec != nil {
					parts = append(parts, taggedPart{text: m.subBlock(rec) + "\n", zone: zonePlain})
				}
			}
		}
	}
	if m.thinkingBlock != "" {
		parts = append(parts, taggedPart{text: m.thinkingBlock, zone: zoneAssistant})
	}
	for _, c := range m.assistantChunks.chunks {
		parts = append(parts, taggedPart{text: c, zone: zoneAssistant})
	}
	for _, e := range m.ephemeral {
		parts = append(parts, taggedPart{text: e, zone: zonePlain})
	}
	return parts
}

// thinkingIndicator renders the live in-conversation "Thinking" slot shown
// while a model generation is pending: the real thinking block once thoughts
// are streaming, otherwise an animated placeholder right under the last
// content. ctrl+t expands it (showThinking) to stream the thoughts live.
func (m *Model) thinkingIndicator() string {
	if m.assistantThinking != nil && m.assistantThinking.raw.Len() > 0 {
		return m.renderThinkingBlock()
	}
	title := "Thinking"
	if m.showThinking {
		title = "▼ Thinking"
	}
	return ui.thinkingHeader.Render(title + " " + spinnerFrames[m.spinnerIdx] + "…")
}

// renderThinkingBlock renders the streamed thinking (a dimmed header plus,
// when shown, the chunked reasoning body). Used both for the live tail and
// for the frozen stable block once the response text starts.
func (m *Model) renderThinkingBlock() string {
	if m.assistantThinking == nil || m.assistantThinking.raw.Len() == 0 {
		return ""
	}
	header := "Thinking"
	if m.showThinking {
		header = "▼ Thinking"
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
// re-split the whole conversation on every 50ms tick. The live slot also
// carries the slash-command palette when it is open (idle only).
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
		m.conv.appendStableZ(stable[i].text, stable[i].zone) // O(delta): only new parts are appended
	}
	m.viewCommitted = len(stable)
	switch {
	case m.compressing:
		m.conv.setLive(m.compressionIndicator())
	case m.streaming && m.generationPending:
		// A model generation is pending: show the live in-conversation
		// "Thinking" slot under the last content (opencode/openrouter style).
		m.conv.setLiveZ(m.thinkingIndicator(), zoneAssistant)
	case m.assistantActive:
		m.conv.setLiveZ(m.streamTail(), zoneAssistant)
	case m.subRunning():
		// A top-level sub-agent is running: its live row (agent + current
		// tool) owns the live slot until the run finishes.
		m.conv.setLive(m.subAgentLive())
	case m.sessionsOpen():
		// /sessions picker: the recent-session list in the live slot.
		m.conv.setLive(m.sessionsBlock())
	case m.paletteVisible():
		m.conv.setLive(m.paletteBlock())
	default:
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
	// Wrap at the content width (terminal width minus the right rail when it
	// is active) so markdown lines never run underneath the rail column.
	md, err := newMarkdownRenderer(maxInt(m.contentWidth()-4, 40), m.theme)
	if err != nil {
		return
	}
	m.md = md
}

// statusBarH / composerH are the fixed row counts of the bottom chrome: the
// status line and the editor panel (1 top + 1 bottom padding + 2 textarea
// rows). layout() splits the remaining rows between the conversation and the
// pinned pending region.
const (
	statusBarH = 1
	composerH  = 4
)

func (m *Model) layout() {
	contentW := m.contentWidth()
	pendingH := len(m.pendingRows(m.pendingMaxRows()))
	vpH := maxInt(m.height-statusBarH-composerH-pendingH, 1)
	m.conv.height = vpH
	m.textarea.SetWidth(maxInt(contentW-8, 20))
	m.textarea.SetHeight(composerH - 2)
}

// contentWidth returns the width available for content (the full terminal
// width minus the right session rail when it is active — see railActive).
func (m Model) contentWidth() int {
	if m.railActive() {
		return maxInt(m.width-m.railWidth(), 40)
	}
	return m.width
}

// View composes the screen. Bubble Tea calls this after every message, so it
// is a hot path worth measuring with GARESS_STATS=1.
func (m Model) View() string {
	if m.stats == nil {
		return m.frame()
	}
	start := time.Now()
	defer func() { m.stats.add(statView, time.Since(start)) }()
	// Per-component timing (tui-stats-view) — on a slow CPU this shows whether
	// View() is dominated by the composer, the conversation join, etc.
	if m.hero() {
		t0 := time.Now()
		hero := m.heroBlock()
		m.stats.addViewPart(viewPartConv, time.Since(t0))
		t0 = time.Now()
		st := m.statusLine()
		m.stats.addViewPart(viewPartStatus, time.Since(t0))
		return m.wrapFrame(strings.Join([]string{hero, st}, "\n"))
	}
	t0 := time.Now()
	cv := m.conv.view()
	m.stats.addViewPart(viewPartConv, time.Since(t0))
	t0 = time.Now()
	// The pending region sits between the conversation (whose live slot is the
	// "current activity") and the composer — see frame.
	pending := m.pendingBlock()
	comp := m.composer()
	m.stats.addViewPart(viewPartComposer, time.Since(t0))
	t0 = time.Now()
	st := m.statusLine()
	m.stats.addViewPart(viewPartStatus, time.Since(t0))
	return m.wrapFrame(m.joinFrame(m.frameParts(cv, pending, comp, st)))
}

// frame is the non-instrumented View() path.
func (m Model) frame() string {
	if m.hero() {
		return m.wrapFrame(strings.Join([]string{m.heroBlock(), m.statusLine()}, "\n"))
	}
	return m.wrapFrame(m.joinFrame(m.frameParts(m.conv.view(), m.pendingBlock(), m.composer(), m.statusLine())))
}

// frameParts stacks the fixed regions of the chat view, top to bottom:
// conversation (history + live "current activity" slot), the pinned pending
// prompts, the composer and the status line. Empty regions are omitted so they
// take no rows.
func (m Model) frameParts(cv, pending, comp, st string) []string {
	parts := []string{cv}
	if pending != "" {
		parts = append(parts, pending)
	}
	parts = append(parts, comp, st)
	return parts
}

// joinFrame stacks frame parts. Left-aligned frame stack. Do NOT use
// lipgloss.JoinVertical here: it splits every block, re-measures the ANSI
// display width of EVERY line and re-pads the whole frame to a common width on
// every call — O(frame) per keystroke (~580µs of the ~600µs View on a desktop;
// ~90ms/frame on a Pi 1 per GARESS_STATS, scaling with conversation size).
// Nothing in this layout needs a shared width (all blocks are left-aligned and
// the terminal erases to end-of-line when lines are repainted), so trailing
// padding is invisible. strings.Join reproduces JoinVertical's line structure
// exactly (an empty block still contributes one blank line, matching
// strings.Split("", "\n")) without any width math.
func (m Model) joinFrame(parts []string) string {
	return strings.Join(parts, "\n")
}

// --- pending prompts (type-ahead queue) ----------------------------------

// pendingMaxRows bounds the pinned pending region so the conversation keeps at
// least one visible row (the composer + status line always win the bottom of
// the terminal). 0 means "draw nothing" on very short terminals.
func (m Model) pendingMaxRows() int {
	return maxInt(m.height-(statusBarH+composerH+1), 0)
}

// pendingBlock renders the queued prompts pinned between the conversation
// (whose live slot is the current activity) and the composer, oldest (next to
// be sent) first. Returns "" when nothing is queued. The row count here must
// agree with layout() — both call pendingRows with pendingMaxRows.
func (m Model) pendingBlock() string {
	rows := m.pendingRows(m.pendingMaxRows())
	if len(rows) == 0 {
		return ""
	}
	return strings.Join(rows, "\n")
}

// pendingRows renders up to maxRows rows of queued prompts. Each prompt is a
// dim "Pending" block on the user rail (it is the user's message, just not
// sent yet), wrapped to the content width. At most pendingMaxShown prompts are
// expanded; the rest collapse into a "+N more" row.
func (m Model) pendingRows(maxRows int) []string {
	q := m.queued
	if len(q) == 0 || maxRows <= 0 {
		return nil
	}
	bodyW := maxInt(m.contentWidth()-6, 10) // rail(2) + body indent(2) + margin(2)
	countRow := func(n int) []string {
		return []string{ui.userRail + ui.pendingBody.Render(fmt.Sprintf("%d pending", n))}
	}
	// A single block taller than maxRows (huge prompt on a short terminal)
	// collapses to a one-line count so the region never exceeds maxRows.
	if len(m.pendingPromptRows(q[0].text, bodyW)) > maxRows {
		return countRow(len(q))
	}
	var rows []string
	shown := 0
	for i := 0; i < len(q) && i < pendingMaxShown; i++ {
		block := m.pendingPromptRows(q[i].text, bodyW)
		sep := 0
		if len(rows) > 0 {
			sep = 1 // blank line between pending blocks
		}
		if len(rows)+sep+len(block) > maxRows {
			break // the next block would push past maxRows; stop at a boundary
		}
		if sep > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, block...)
		shown = i + 1
	}
	if shown < len(q) {
		more := fmt.Sprintf("+ %d more pending", len(q)-shown)
		if len(rows)+2 <= maxRows {
			rows = append(rows, "")
			rows = append(rows, ui.userRail+ui.pendingBody.Render(more))
		}
	}
	return rows
}

// pendingPromptRows renders one queued prompt as a pending block: a "Pending"
// label row plus the wrapped prompt text, each row on the user rail.
func (m Model) pendingPromptRows(text string, bodyW int) []string {
	rows := []string{ui.userRail + ui.pendingLabel.Render("Pending")}
	for _, l := range wrapPlain(text, bodyW) {
		rows = append(rows, ui.userRail+ui.pendingBody.Render(l))
	}
	return rows
}

// wrapPlain wraps ANSI-free text to at most width display columns, preserving
// existing newlines and hard-breaking words longer than a line. Blank lines in
// the source are dropped (a compact preview is enough for the pending region).
func wrapPlain(s string, width int) []string {
	if width <= 0 {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []string{s}
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		var cur []string
		curW := 0
		flush := func() {
			if len(cur) > 0 {
				out = append(out, strings.Join(cur, " "))
				cur = nil
				curW = 0
			}
		}
		for _, w := range strings.Fields(para) {
			wl := lipgloss.Width(w)
			if wl > width {
				// A single word longer than the line: hard-break it.
				flush()
				for runes := []rune(w); len(runes) > 0; {
					n := min(width, len(runes))
					out = append(out, string(runes[:n]))
					runes = runes[n:]
				}
				continue
			}
			if curW > 0 && curW+1+wl > width {
				flush()
			}
			if len(cur) > 0 {
				curW += 1 + wl
			} else {
				curW = wl
			}
			cur = append(cur, w)
		}
		flush()
	}
	return out
}

// composer renders the editor panel for the conversation view: the textarea
// inside a full-width panel with a slightly lighter background and the user
// left rail (same as user messages).
func (m Model) composer() string {
	return m.editorBox(m.contentWidth())
}

// editorBox wraps the textarea view in the editor panel style, stretched to
// the given width so the panel background spans the whole row. Width is
// applied per call (the model is value-copied); the panel is a small fixed
// block, so the one-time lipgloss width pass is bounded and cheap.
func (m Model) editorBox(w int) string {
	if w <= 0 {
		return ui.composer.Render(m.composerBody())
	}
	// The prompt box wears the same left rail as user messages ("You"): the
	// rail prefix occupies the composerRail cells, so the panel renders at
	// w-composerInner and every row is prefixed with the rail (painted over
	// the panel background). Typed text keeps its column because the panel's
	// own left padding is gone.
	inner := maxInt(w-ui.composerInner, 20)
	panel := ui.composer.Width(inner).Render(m.composerBody())
	lines := strings.Split(panel, "\n")
	for i, l := range lines {
		lines[i] = ui.composerRail + l
	}
	return strings.Join(lines, "\n")
}

// composerBody is the content rendered inside the editor panel: the live
// textarea view once there is text; otherwise the placeholder is drawn here
// directly. bubbles' own placeholder rendering leaves its internal viewport
// padding without a background, which shows as a black band to the right of
// the placeholder text (the panel background can't repaint those cells — the
// viewport output carries its own style resets). These hand-drawn rows carry
// the panel background end to end, so the empty composer is uniform.
func (m Model) composerBody() string {
	if m.textarea.Value() != "" {
		return m.textarea.View()
	}
	// Two content rows, matching the textarea's height: the placeholder text
	// and a blank row. Both are drawn with the panel background (the panel
	// stretches them to the full width).
	return ui.placeholderBody.Render(m.textarea.Placeholder) + "\n" +
		ui.placeholderBody.Render(" ")
}

// styleTextarea makes the composer textarea blend into the editor panel. The
// placeholder is a dim foreground and no cursor-line block background is
// painted. Crucially, the PANEL background is baked into every textarea
// content style: the textarea emits style resets around its typed text, and
// those would otherwise drop the typed rows back onto the terminal's default
// (darker) background instead of the panel's.
func styleTextarea(ta *textarea.Model, theme string) {
	c := darkColors
	if theme == "light" {
		c = lightColors
	}
	panelBG := lipgloss.Color(c.panelBG)
	focused, blurred := textarea.DefaultStyles()
	ph := lipgloss.NewStyle().Foreground(lipgloss.Color(c.faint)).Background(panelBG)
	lineBG := lipgloss.NewStyle().Background(panelBG)
	focused.Text = focused.Text.Background(panelBG)
	focused.Prompt = focused.Prompt.Background(panelBG)
	focused.Placeholder = ph
	focused.CursorLine = lineBG
	focused.EndOfBuffer = focused.EndOfBuffer.Background(panelBG)
	blurred.Text = blurred.Text.Background(panelBG)
	blurred.Prompt = blurred.Prompt.Background(panelBG)
	blurred.Placeholder = ph
	blurred.CursorLine = lineBG
	blurred.EndOfBuffer = blurred.EndOfBuffer.Background(panelBG)
	ta.FocusedStyle = focused
	ta.BlurredStyle = blurred
}

// reconcileComposerCursor matches the textarea's cursor to the composer
// state. While the composer is EMPTY the block cursor is hidden: bubbles
// renders that cursor OVER the first placeholder character, and its
// reverse-video box reads as a black block next to the placeholder text. As
// soon as there is text the blinking block cursor returns (SetMode(CursorBlink)
// flips it visible and returns the command that (re)starts the blink cycle).
func (m *Model) reconcileComposerCursor() tea.Cmd {
	c := &m.textarea.Cursor
	if m.textarea.Value() == "" {
		if c.Mode() != cursor.CursorHide {
			c.SetMode(cursor.CursorHide)
		}
		return nil
	}
	if c.Mode() != cursor.CursorBlink {
		return c.SetMode(cursor.CursorBlink)
	}
	return nil
}

// modelLabel is the "provider · model" text for the bottom bar.
func (m Model) modelLabel() string {
	prov := m.providers[m.current]
	if prov == nil {
		return m.current
	}
	return m.current + " · " + prov.Model
}

// statusLine renders the bottom bar: the busy state / error / provider·model
// on the left, context usage and the app version right-aligned.
func (m Model) statusLine() string {
	var line string
	switch {
	case m.confirming:
		line = ui.confirm.Render("⚠ " + m.confirmPrompt + "  (y approve · n deny)")
	case m.streaming:
		verb := m.streamVerb() + "  (esc to stop)"
		if n := len(m.queued); n > 0 {
			verb += fmt.Sprintf(" · %d queued", n)
		}
		line = ui.streaming.Render(spinnerFrames[m.spinnerIdx] + " " + verb)
	case m.err != "":
		line = ui.err.Render("⚠ " + m.err)
	case m.status != "":
		line = ui.status.Render(m.status)
	default:
		line = ui.meta.Render(m.modelLabel())
	}
	// Context usage is always visible, right-aligned on the status line.
	var right []string
	if r := m.contextReadout(); r != "" {
		right = append(right, ui.info.Render(r))
	}
	if m.version != "" {
		right = append(right, ui.status.Render(m.version))
	}
	if len(right) == 0 {
		return line
	}
	seg := strings.Join(right, "  ")
	if m.width > 0 {
		if pad := m.contentWidth() - lipgloss.Width(line) - lipgloss.Width(seg) - 1; pad > 0 {
			line += strings.Repeat(" ", pad)
		}
	} else {
		line += "  "
	}
	return line + seg
}

// streamVerb labels the streaming phase on the bottom bar: thinking while a
// model generation is pending, responding once visible text streams, and a
// generic working state while tools run between generations.
func (m Model) streamVerb() string {
	switch {
	case m.generationPending:
		return "thinking…"
	case m.assistantActive:
		return "responding…"
	default:
		return "working…"
	}
}

// --- right session rail (Phase E) ----------------------------------------

// railMinWidth is the terminal width at which the right session rail appears.
// Below it (e.g. the Pi's 118 columns) the rail is absent entirely, so its
// per-frame width math never runs on the Pi.
const railMinWidth = 140

// railWidth is the width reserved for the right session rail.
const railWidth = 34

// railActive reports whether the right session rail is drawn: it needs a wide
// enough terminal (desktop — never on the Pi's 118 columns) AND a session
// service to list. The rail's per-frame width math only runs while active.
func (m Model) railActive() bool {
	return m.width >= railMinWidth && m.sessionSvc != nil
}

// railWidth returns the reserved rail width (only meaningful when railActive).
func (m Model) railWidth() int { return railWidth }

// --- hero (empty-state launch screen) ------------------------------------

const (
	heroTagline = "minimal AI harness for the terminal"
	heroHint    = "enter send · ctrl+j newline · type / for commands · ctrl+t thinking"
)

// hero reports whether the empty-state launch screen is showing: no
// conversation content and nothing busy in flight.
func (m Model) hero() bool {
	return len(m.rendered) == 0 && len(m.ephemeral) == 0 &&
		!m.assistantActive && !m.streaming && !m.compressing && !m.confirming
}

// heroBlock renders the launch screen — pixel logo, tagline, slash palette
// (when open), the editor panel and a hint line, vertically centered on the
// terminal's own (dark) background. It returns exactly the rows above the
// bottom status bar.
func (m Model) heroBlock() string {
	if m.height <= 1 {
		return "" // no rows yet (before the first window size); bubble tea re-renders on resize
	}
	avail := m.height - 1
	// Center within the content column (the right rail occupies the rest when
	// it is active, so the hero never runs under it).
	width := m.contentWidth()
	if width <= 0 {
		width = 100
	}
	contentW := min(maxInt(width-8, 40), 110)
	leftPad := maxInt((width-contentW)/2, 0)

	center := func(s string) string {
		pad := maxInt((width-lipgloss.Width(s))/2, 0)
		return strings.Repeat(" ", pad) + s
	}

	var content []string
	if width >= logoWidth() {
		for _, l := range logoLines() {
			content = append(content, center(ui.logo.Render(l)))
		}
		content = append(content, "")
	}
	content = append(content, center(ui.tagline.Render(heroTagline)))
	content = append(content, "")

	if m.paletteVisible() {
		for _, r := range m.paletteList(paletteMaxRows, contentW) {
			content = append(content, center(r))
		}
		content = append(content, "")
	}
	padLine := strings.Repeat(" ", leftPad)
	if m.sessionsOpen() {
		for _, r := range m.sessionsRows(6, contentW) {
			content = append(content, padLine+r)
		}
		content = append(content, "")
	}
	for _, l := range strings.Split(strings.TrimSuffix(m.editorBox(contentW), "\n"), "\n") {
		content = append(content, padLine+l)
	}
	content = append(content, "")
	content = append(content, center(ui.hint.Render(heroHint)))

	// Never let the block overflow the screen: drop least-important top rows
	// (logo, tagline, palette) first, then the hint, keeping the editor.
	for len(content) > avail && len(content) > 6 {
		content = content[1:]
	}
	for len(content) > avail {
		content = content[:len(content)-1]
	}
	out := make([]string, 0, avail)
	for i := 0; i < (avail-len(content))/2; i++ {
		out = append(out, "")
	}
	out = append(out, content...)
	for len(out) < avail {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

// --- slash-command palette ------------------------------------------------

// paletteMaxRows caps the slash-command list so the conversation stays
// visible while it is open.
const paletteMaxRows = 8

// paletteOpen reports whether the composer holds a single-line value starting
// with "/" (the palette trigger). The palette only interacts while idle.
func (m Model) paletteOpen() bool {
	if m.streaming || m.confirming || m.compressing {
		return false
	}
	v := m.textarea.Value()
	if v == "" || strings.Contains(v, "\n") {
		return false
	}
	return v[0] == '/'
}

// paletteWord is the text after "/" up to the first space ("no" in "/no").
func (m Model) paletteWord() string {
	v := strings.TrimPrefix(m.textarea.Value(), "/")
	if i := strings.IndexAny(v, " \t"); i >= 0 {
		v = v[:i]
	}
	return v
}

// paletteRows returns the commands matching the typed prefix.
func (m Model) paletteRows() []cmdSpec {
	return filterCommands(m.paletteWord())
}

// paletteVisible reports whether the palette list should be drawn: idle, "/"
// typed, at least one match, and the typed word is not already a complete
// command name (nothing left to choose — Enter runs it directly).
func (m Model) paletteVisible() bool {
	if !m.paletteShow || !m.paletteOpen() {
		return false
	}
	rows := m.paletteRows()
	if len(rows) == 0 {
		return false
	}
	for _, c := range rows {
		if strings.TrimPrefix(c.name, "/") == m.paletteWord() {
			return false
		}
	}
	return true
}

// clampPalette keeps the selection within the filtered command list.
func (m *Model) clampPalette() {
	if n := len(m.paletteRows()); n > 0 {
		m.paletteSel %= n
		if m.paletteSel < 0 {
			m.paletteSel = 0
		}
	}
}

// paletteList renders up to maxRows palette rows (name left, description
// after, selected row highlighted), width-padded to maxW.
func (m Model) paletteList(maxRows, maxW int) []string {
	rows := m.paletteRows()
	n := len(rows)
	if n == 0 {
		return nil
	}
	if maxRows > n {
		maxRows = n
	}
	start := 0
	if m.paletteSel >= maxRows {
		start = m.paletteSel - maxRows + 1
	}
	nameW := 0
	for _, c := range rows {
		if w := len(strings.TrimPrefix(c.name, "/")); w > nameW {
			nameW = w
		}
	}
	out := make([]string, 0, maxRows)
	for i := start; i < start+maxRows; i++ {
		c := rows[i]
		name := c.name
		row := name + strings.Repeat(" ", nameW+2-len(strings.TrimPrefix(c.name, "/"))) + c.desc
		if maxW > 0 {
			if pad := maxW - lipgloss.Width(row); pad > 0 {
				row += strings.Repeat(" ", pad)
			}
		}
		if i == m.paletteSel {
			out = append(out, ui.palSel.Render(row))
		} else {
			out = append(out, ui.palItem.Render(row[:len(name)])+ui.palDesc.Render(row[len(name):]))
		}
	}
	return out
}

// paletteBlock renders the palette list for the conversation view's live slot.
func (m Model) paletteBlock() string {
	maxRows := min(paletteMaxRows, maxInt(m.conv.height-1, 1))
	lines := m.paletteList(maxRows, m.contentWidth())
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// paletteComplete fills in the selected command name, keeping any text after
// the word (arguments), and moves the cursor to the end.
func (m Model) paletteComplete() (tea.Model, tea.Cmd) {
	rows := m.paletteRows()
	if len(rows) == 0 {
		return m, nil
	}
	sel := rows[m.paletteSel%len(rows)]
	name := strings.TrimPrefix(sel.name, "/")
	v := m.textarea.Value()
	rest := ""
	if i := strings.IndexAny(v, " \t"); i >= 0 {
		rest = v[i:]
	}
	m.textarea.SetValue("/" + name + rest)
	m.textarea.CursorEnd()
	m.paletteShow = true
	m.clampPalette()
	m.updateViewport()
	return m, m.reconcileComposerCursor()
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
