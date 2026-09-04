// Context usage tracking and compression orchestration for the TUI.
//
// The indicator and the auto-compress decision run on estimates: there is no
// offline tokenizer, so usage is approximated (≈1 token per 4 chars) and
// anchored to the exact usage.prompt_tokens the server reports on every model
// event. The context window comes from config (Options.ContextWindows), else
// a fallback default (marked approximate).
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/genai"

	tea "github.com/charmbracelet/bubbletea"

	"google.golang.org/adk/v2/session"

	"garess/internal/chat"
	"garess/internal/compress"
	"garess/internal/config"
	"garess/internal/harness"
)

// Options carries context/compression configuration into New. The zero value
// means auto-compression is disabled; pass no Options at all for the defaults
// (auto-compress on at 80%, window fallback 128k).
type Options struct {
	// AutoCompress enables automatic context compression after a turn and
	// before a prompt that would push past the threshold.
	AutoCompress bool
	// AutoCompressPct is the threshold percentage of the context window.
	// <= 0 uses config.DefaultAutoCompressThreshold.
	AutoCompressPct int
	// ContextWindows maps provider name to its configured context window in
	// tokens. 0/unset providers fall back to DefaultContextWindow (marked ≈).
	ContextWindows map[string]int
	// DefaultContextWindow overrides the fallback window for providers that
	// have no configured window.
	DefaultContextWindow int
}

// windowInfo is the resolved context window for one provider.
type windowInfo struct {
	size   int
	approx bool
}

// resolveWindows computes each provider's context window: configured values
// are exact; anything else falls back to the (approximate) default.
func resolveWindows(providers map[string]*harness.Provider, o Options) map[string]windowInfo {
	out := make(map[string]windowInfo, len(providers))
	fallback := o.DefaultContextWindow
	if fallback <= 0 {
		fallback = config.DefaultContextWindow
	}
	for name := range providers {
		w := o.ContextWindows[name]
		if w > 0 {
			out[name] = windowInfo{size: w}
		} else {
			out[name] = windowInfo{size: fallback, approx: true}
		}
	}
	return out
}

// compressMsg carries the result of an async compression to Update.
type compressMsg struct {
	summary      *session.Event // the persisted summary event (nil on error)
	tail         []*session.Event
	covered      int // estimated tokens of the summarized (covered) events
	coveredCount int // number of events summarized
	digest       int // estimated tokens of the new summary event text
	auto         bool
	err          error
}

// applyWindow switches the active context window to provider's resolved one.
func (m *Model) applyWindow(name string) {
	w, ok := m.windows[name]
	if !ok {
		w = windowInfo{size: config.DefaultContextWindow, approx: true}
	}
	m.ctxWindow = w.size
	m.ctxApprox = w.approx
	m.ctxEst = 0
	m.ctxLastPrompt = 0
	m.ctxUsageThisRun = false
}

// thresholdTokens returns the estimated usage that triggers auto-compression.
func (m Model) thresholdTokens() int {
	if m.ctxWindow <= 0 {
		// No known window: never auto-trigger (max int for this platform —
		// must not hard-code 1<<62, which overflows 32-bit int on armv6).
		return int(^uint(0) >> 1)
	}
	pct := m.autoCompressPct
	if pct <= 0 {
		pct = config.DefaultAutoCompressThreshold
	}
	return m.ctxWindow * pct / 100
}

// trackUsage anchors the usage estimate to a completed model event's exact
// server-reported prompt token count.
func (m *Model) trackUsage(ev *session.Event) {
	if ev == nil || ev.LLMResponse.UsageMetadata == nil {
		return
	}
	if n := ev.LLMResponse.UsageMetadata.PromptTokenCount; n > 0 {
		m.ctxLastPrompt = int(n)
		m.ctxEst = int(n)
		m.ctxUsageThisRun = true
	}
}

// estimateEventTokens approximates one event's context contribution: its text
// plus the JSON of tool calls/results, at ≈1 token per 4 characters.
func estimateEventTokens(ev *session.Event) int {
	if ev == nil || ev.Content == nil {
		return 0
	}
	var b strings.Builder
	for _, p := range ev.Content.Parts {
		if p == nil || p.Thought {
			continue
		}
		switch {
		case p.Text != "":
			b.WriteString(p.Text)
		case p.FunctionCall != nil:
			if j, err := json.Marshal(p.FunctionCall.Args); err == nil {
				b.Write(j)
			}
		case p.FunctionResponse != nil:
			if j, err := json.Marshal(p.FunctionResponse.Response); err == nil {
				b.Write(j)
			}
		}
	}
	return compress.EstimateTokens(b.String())
}

func estimateEvents(events []*session.Event) int {
	n := 0
	for _, ev := range events {
		n += estimateEventTokens(ev)
	}
	return n
}

// startCompression kicks off an asynchronous context compression. auto marks
// the automatic (turn-end / pre-send) path for feedback wording; instruction
// is the user's optional /compress guidance. When pending is non-nil the
// user's message is sent (startStream) as soon as compression succeeds; if
// there is nothing worth compressing the pending message is sent right away.
func (m Model) startCompression(auto bool, instruction string, pending *genai.Content, pendingText string) (tea.Model, tea.Cmd) {
	cutoff, ok := compress.PickCutoff(m.events)
	prov := m.providers[m.current]
	svc, svcOK := chatServiceOf(prov)

	if !ok || cutoff < 0 || prov == nil || prov.LLMModel == nil || !svcOK {
		if pending != nil {
			m.pendingSend = nil
			return m.startStream(pending)
		}
		if !ok || cutoff < 0 {
			m.status = "nothing to compress yet"
		} else {
			m.err = "context compression needs the configured model and session service"
		}
		return m, nil
	}

	covered := make([]*session.Event, cutoff+1)
	copy(covered, m.events[:cutoff+1])
	tail := make([]*session.Event, len(m.events)-cutoff-1)
	copy(tail, m.events[cutoff+1:])

	ctx, cancel := context.WithCancel(context.Background())
	m.compressing = true
	m.compressIsAuto = auto
	m.compressCancel = cancel
	m.pendingSend = pending
	m.pendingText = pendingText
	m.status = ""
	// Show the in-conversation compression indicator immediately (spinner +
	// text), and reveal it if the user had scrolled up.
	m.conv.gotoBottom()
	m.updateViewport()

	ch := make(chan compressMsg, 1)
	go func() {
		defer close(ch)
		res := runCompression(ctx, prov, svc, m.userID, m.sessionID, covered, tail, instruction)
		res.auto = auto
		select {
		case ch <- res:
		case <-ctx.Done():
		}
	}()
	return m, func() tea.Msg {
		res, ok := <-ch
		if !ok {
			return compressMsg{err: context.Canceled}
		}
		return res
	}
}

// chatServiceOf returns the concrete chat service behind a provider's session
// service, if any.
func chatServiceOf(prov *harness.Provider) (*chat.Service, bool) {
	if prov == nil || prov.SessionService == nil {
		return nil, false
	}
	svc, ok := prov.SessionService.(*chat.Service)
	return svc, ok
}

// runCompression runs the summarizer and persists the summary + compaction
// marker. It runs on a goroutine and touches no Bubble Tea model state — all
// inputs are snapshots.
func runCompression(ctx context.Context, prov *harness.Provider, svc *chat.Service, userID, sessionID string, covered, tail []*session.Event, instruction string) compressMsg {
	res := compressMsg{tail: tail, covered: estimateEvents(covered), coveredCount: len(covered)}
	transcript := compress.Transcript(covered, compress.DefaultMaxTranscriptChars)
	system, user := compress.SummaryPrompt(instruction, transcript)
	digest, err := compress.Summarize(ctx, prov.LLMModel, system, user, compress.DefaultMaxSummaryTokens)
	if err != nil {
		res.err = err
		return res
	}
	summaryText := compress.FormatSummary(digest)
	coveredThrough := covered[len(covered)-1]
	sumEv, err := svc.Compact(ctx, &chat.CompactRequest{
		AppName:               harness.AppName,
		UserID:                userID,
		SessionID:             sessionID,
		SummaryText:           summaryText,
		CoveredThroughEventID: coveredThrough.ID,
		CoveredCount:          len(covered),
	})
	if err != nil {
		res.err = err
		return res
	}
	res.summary = sumEv
	res.digest = compress.EstimateTokens(summaryText)
	return res
}

// handleCompressResult applies an async compression result: it rebuilds the
// display to the compacted view ([summary] + tail), updates the usage
// indicator, shows before/after feedback and, when a message was queued
// before an auto-compression, sends it.
func (m Model) handleCompressResult(msg compressMsg) (tea.Model, tea.Cmd) {
	m.compressing = false
	m.compressIsAuto = false
	if m.compressCancel != nil {
		m.compressCancel()
		m.compressCancel = nil
	}
	if msg.err != nil {
		if errors.Is(msg.err, context.Canceled) {
			if m.pendingText != "" {
				m.textarea.SetValue(m.pendingText)
			}
			m.status = "context compression cancelled"
		} else {
			m.err = fmt.Sprintf("context compression failed: %v", msg.err)
		}
		m.pendingSend = nil
		m.pendingText = ""
		m.updateViewport()
		return m, nil
	}
	if msg.summary == nil {
		m.err = "context compression produced no summary"
		m.pendingSend = nil
		m.pendingText = ""
		return m, nil
	}

	m.summaryEventID = msg.summary.ID
	m.events = append([]*session.Event{msg.summary}, msg.tail...)

	// Before/after are TOTAL context estimates when a usage anchor exists
	// (m.ctxEst is still the pre-compression value here); otherwise they
	// describe just the summarized region. Replacing the covered events with
	// the digest shrinks the total by (covered - digest).
	before := m.ctxEst
	switch {
	case before > 0:
		m.ctxEst = before - (msg.covered - msg.digest)
	case msg.covered > 0:
		before = msg.covered
		m.ctxEst = msg.digest
	default:
		m.ctxEst = 0
	}
	if m.ctxEst < 0 {
		m.ctxEst = 0
	}
	saved := before - m.ctxEst
	if saved < 0 {
		saved = 0
	}
	m.ctxLastPrompt = 0 // an estimate again until the next model call re-anchors
	if msg.auto {
		// Cooldown: don't re-run an auto-compression until a few new events
		// have accumulated (prevents thrash when the system prompt + tools
		// alone sit near the threshold).
		m.ctxNoAutoAfter = len(m.events) + 2
	}

	label := "Context compressed"
	if msg.auto {
		label = "Auto-compressed context"
	}
	noun := "messages"
	if msg.coveredCount == 1 {
		noun = "message"
	}
	card := fmt.Sprintf("**%s** — %d earlier %s summarized into one digest.\n\n", label, msg.coveredCount, noun)
	card += fmt.Sprintf("Context: %s → %s tokens (freed %s)", fmtCount(before), fmtCount(m.ctxEst), fmtCount(saved))
	if m.ctxWindow > 0 {
		card += fmt.Sprintf(" · usage %s → %s of %s", fmtPct(before, m.ctxWindow), fmtPct(m.ctxEst, m.ctxWindow), fmtTokens(m.ctxWindow))
	}
	m.renderAll()
	m.addInfo(card)

	pending := m.pendingSend
	m.pendingSend = nil
	m.pendingText = ""
	if pending != nil {
		return m.startStream(pending)
	}
	m.updateViewport()
	return m, nil
}

// cancelCompression aborts an in-flight compression (esc). The pending text
// (if any) is restored when the cancelled result arrives.
func (m *Model) cancelCompression() {
	if m.compressCancel != nil {
		m.compressCancel()
	}
}

// compressionIndicator is the in-conversation live block shown while a
// compression runs, styled like the tool/thinking blocks so the user can see
// something is happening (the status bar mirrors it).
func (m Model) compressionIndicator() string {
	verb := "Compressing context"
	if m.compressIsAuto {
		verb = "Auto-compressing context"
	}
	return ui.streaming.Render(spinnerFrames[m.spinnerIdx] + " " + verb + "…  (esc to stop)")
}

// estimatedPromptTokens approximates the next prompt when the server reports
// no usage (it cannot honor stream_options.include_usage): the conversation
// events plus the instruction preamble. Tool schemas and identity text are
// not counted, so this under-reports and the readout marks it with ≈.
func (m Model) estimatedPromptTokens() int {
	total := estimateEvents(m.events)
	if m.preamble != nil {
		if t, err := m.preamble.Get(); err == nil && t != "" {
			total += compress.EstimateTokens(t)
		}
	}
	return total
}

// contextReadout renders the always-visible status-bar context readout, e.g.
// "ctx 12% · 30k/262k used · 232k free", or "" when no window is known yet.
// Values are exact when anchored to server usage; estimates get an ≈ prefix.
func (m Model) contextReadout() string {
	if m.ctxWindow <= 0 {
		return ""
	}
	approx := m.ctxApprox
	if m.ctxEst > 0 && m.ctxLastPrompt <= 0 {
		approx = true // estimate, not anchored to server usage
	}
	mark := ""
	if approx {
		mark = "≈"
	}
	used := m.ctxEst
	free := m.ctxWindow - used
	if free < 0 {
		free = 0
	}
	return fmt.Sprintf("ctx %s%s · %s/%s used · %s free",
		mark, fmtPct(used, m.ctxWindow), fmtTokens(used), fmtTokens(m.ctxWindow), fmtTokens(free))
}

// fmtPct renders part/whole as a percentage, with one decimal below 10% so
// small values on large windows stay readable (e.g. "0.4%").
func fmtPct(part, whole int) string {
	if whole <= 0 {
		return "–"
	}
	p := float64(part) * 100 / float64(whole)
	if p >= 10 {
		return fmt.Sprintf("%d%%", int(p+0.5))
	}
	return fmt.Sprintf("%.1f%%", p)
}

// fmtCount renders an exact integer with thousands separators ("1,024").
func fmtCount(n int) string {
	if n < 0 {
		return "-" + fmtCount(-n)
	}
	digits := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func fmtTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	return fmt.Sprintf("%d", n)
}
