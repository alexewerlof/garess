package tui

// Typing-latency probe.
//
// Question: pressing a key while typing a prompt takes a perceptible
// (~100-200ms) time to appear on screen when garess runs over SSH on a slow
// machine (Raspberry Pi 1). This file measures where that latency comes from.
//
// It is gated behind GARESS_KEYPROBE=1 so normal `go test ./...` stays fast:
//
//	# host (sanity check)
//	GARESS_KEYPROBE=1 go test ./internal/tui -run TestKeyLatencyProbe -v
//
//	# Raspberry Pi 1 (cross-compile on the host, run on the Pi)
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go test -c -o /tmp/tui-keyprobe ./internal/tui
//	scp /tmp/tui-keyprobe pi:~/
//	ssh pi 'GARESS_KEYPROBE=1 ./tui-keyprobe -test.run TestKeyLatencyProbe -test.v'
//
// Two measurements per scenario:
//
//	Probe A (raw loop): the cost of OUR code for one keystroke when idle —
//	  Model.Update (handleKey -> textarea) and Model.View (full frame compose),
//	  plus the frame byte size and how many lines actually change per key.
//	  This is the CPU the Bubble Tea loop spends per key BEFORE anything is
//	  written to the terminal.
//
//	Probe B (end-to-end): a real tea.Program driven with Program.Send; we time
//	  key -> frame bytes written to the terminal by the renderer. This includes
//	  Bubble Tea's internal 60fps renderer quantization (a key's frame waits for
//	  the next renderer tick, 0-16.6ms) and the renderer's line diffing. It is
//	  the device-side floor of what the user perceives (SSH RTT is on top).
//
// Scenarios vary the conversation size (empty/short/long) and the terminal
// size to see whether per-key cost scales with history or with the visible
// window.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

const probeKeys = 60 // measured keystrokes per scenario/probe
const probeWarm = 5  // warmup keystrokes (GC, textarea state, caches)

// keyprobeEnabled reports whether the (slow, interactive) latency probe should
// run. Normal test runs skip it.
func keyprobeEnabled() bool { return os.Getenv("GARESS_KEYPROBE") != "" }

type latencyScenario struct {
	name   string
	words  int // per assistant reply
	turns  int // completed assistant replies in the conversation
	width  int
	height int
}

var latencyScenarios = []latencyScenario{
	{"empty-100x30", 0, 0, 100, 30},
	{"short-100x30", 500, 2, 100, 30},
	{"long-100x30", 2000, 8, 100, 30},
	{"long-160x46", 2000, 8, 160, 46},
}

// probeKeyset returns the runes we "type", one KeyMsg per character (exactly
// what a human typer produces).
//
// NOTE: the e2e probe measures latency by watching for the renderer to write
// the changed frame to the terminal. Bubble Tea skips writing frames that are
// byte-identical to the previous one, and a trailing SPACE can be one of
// those (the cursor is rendered as an unstyled space when it is in its hidden
// blink phase, so "Explain" + cursor and "Explain " + cursor render the same
// bytes). A space key therefore produces no terminal output and cannot be
// latency-measured via bytes — so the keyset here is deliberately space-free;
// every key visibly changes the composer line and always produces a frame.
func probeKeyset(n int) []tea.KeyMsg {
	const phrase = "Explain-the-difference-between-a-closure-and-a-higher-order-function-and-give-a-short-Go-example-of-each-also-mention-when-you-would-prefer-one-over-the-other-including-tradeoffs-around-allocation-readability-and-testability. "
	out := make([]tea.KeyMsg, 0, n)
	for i := 0; i < n; i++ {
		r := rune(phrase[i%len(phrase)])
		out = append(out, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return out
}

// idleTypingModel builds a real idle Model (composer focused, not streaming)
// whose conversation contains `turns` glamour-rendered assistant replies of
// `words` words each. Cursor blink is disabled so no stray blink frames pollute
// the end-to-end byte/latency accounting.
func idleTypingModel(tb testing.TB, sc latencyScenario) *Model {
	tb.Helper()
	md, err := newMarkdownRenderer(sc.width-4, "dark")
	if err != nil {
		tb.Fatal(err)
	}
	ta := textarea.New()
	ta.Placeholder = "Message garess…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.Cursor.SetMode(cursor.CursorStatic)
	ta.Focus()
	m := &Model{
		width:             sc.width,
		height:            sc.height,
		textarea:          ta,
		md:                md,
		conv:              newConvView(sc.height - 4),
		streamBuffer:      &strings.Builder{},
		thinkingBuffer:    &strings.Builder{},
		assistantChunks:   newStreamChunker(),
		assistantThinking: newStreamChunkerWith(func(s string) string { return ui.thinkingBody.Render(s) }),
	}
	m.layout()
	for i := 0; i < sc.turns; i++ {
		out, err := md.Render(markdownDoc(sc.words))
		if err != nil {
			tb.Fatal(err)
		}
		m.rendered = append(m.rendered, out)
	}
	m.updateViewport()
	return m
}

// stats summarizes a set of per-key measurements.
type latencyStats struct {
	avg, p50, p95, max time.Duration
}

func summarize(ds []time.Duration) latencyStats {
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p := func(q float64) time.Duration {
		i := int(q * float64(len(sorted)-1))
		return sorted[i]
	}
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return latencyStats{
		avg: sum / time.Duration(len(ds)),
		p50: p(0.5), p95: p(0.95),
		max: sorted[len(sorted)-1],
	}
}

func fmtDur(s latencyStats) string {
	return fmt.Sprintf("avg=%s p50=%s p95=%s max=%s", s.avg.Round(time.Microsecond), s.p50.Round(time.Microsecond), s.p95.Round(time.Microsecond), s.max.Round(time.Microsecond))
}

// countFrameDelta returns the number of differing lines between two rendered
// frames and the total byte length of the differing lines — a proxy for what
// Bubble Tea's renderer actually ships to the terminal per keystroke.
func countFrameDelta(prev, cur string) (lines int, bytes int) {
	if prev == "" {
		return 0, 0
	}
	a, b := strings.Split(prev, "\n"), strings.Split(cur, "\n")
	n := max(len(a), len(b))
	for i := 0; i < n; i++ {
		var la, lb string
		if i < len(a) {
			la = a[i]
		}
		if i < len(b) {
			lb = b[i]
		}
		if la != lb {
			lines++
			bytes += len(lb)
		}
	}
	return lines, bytes
}

// probeRaw measures our per-keystroke Update + View cost on a single Model,
// the way Bubble Tea invokes them (minus the renderer). Also reports frame
// size and how much of the frame actually changes per key.
func probeRaw(tb testing.TB, sc latencyScenario, keys []tea.KeyMsg) (update, view latencyStats, frameBytes, chgLines, chgBytes int) {
	tb.Helper()
	cur := *idleTypingModel(tb, sc)

	var updates, views []time.Duration
	prev := ""
	var totalFrame, totalChgLines, totalChgBytes int
	measured := 0

	step := func(k tea.KeyMsg, count bool) {
		t0 := time.Now()
		next, _ := cur.Update(k)
		tUpd := time.Since(t0)
		// Bubble Tea renders the model Update returned, not the old one.
		view := next.(Model).View()
		tView := time.Since(t0) - tUpd
		cur = next.(Model)
		if count {
			updates = append(updates, tUpd)
			views = append(views, tView)
			totalFrame += len(view)
			l, b := countFrameDelta(prev, view)
			totalChgLines += l
			totalChgBytes += b
			measured++
		}
		prev = view
	}

	for _, k := range keys[:min(probeWarm, len(keys))] {
		step(k, false)
	}
	for _, k := range keys[probeWarm:] {
		step(k, true)
	}
	return summarize(updates), summarize(views),
		totalFrame / measured, totalChgLines / measured, totalChgBytes / measured
}

// signalWriter counts bytes written and notifies on every Write. Used as the
// tea.Program output so we can observe when a rendered frame actually lands on
// the "terminal" (i.e. hits the pty/SSH in real use).
type signalWriter struct {
	mu     sync.Mutex
	n      int64
	notify chan struct{}
}

func newSignalWriter() *signalWriter { return &signalWriter{notify: make(chan struct{}, 1)} }

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.n += int64(len(p))
	w.mu.Unlock()
	select {
	case w.notify <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *signalWriter) count() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// waitAbove blocks until more than water bytes have been written or timeout
// elapses.
func (w *signalWriter) waitAbove(water int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if w.count() > water {
			return true
		}
		select {
		case <-w.notify:
		case <-time.After(time.Millisecond):
		}
	}
	return w.count() > water
}

// probeE2E runs a real tea.Program (alt screen, like cmd/garess) with the
// conversation preloaded and measures, per injected keystroke, the time until
// the renderer writes the new frame to the output — the device-side
// keystroke->terminal floor, Bubble Tea 60fps render quantization included.
func probeE2E(tb testing.TB, sc latencyScenario, keys []tea.KeyMsg) (e2e latencyStats, bytesPerKey, fullFrame int) {
	tb.Helper()
	w := newSignalWriter()
	prog := tea.NewProgram(idleTypingModel(tb, sc), tea.WithInput(blockingReader{}), tea.WithOutput(w), tea.WithAltScreen())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = prog.Run()
	}()

	// Let the first frame (and alt-screen entry) land before measuring. The
	// alt-screen entry is only ~35B; wait for the full first view flush and let
	// the renderer settle, otherwise keys sent during startup can be dropped
	// (bubbletea's event loop is not in steady state yet).
	if !w.waitAbove(200, 5*time.Second) {
		prog.Quit()
		<-done
		tb.Fatalf("%s: e2e: initial frame never rendered (bytes=%d runErr=%v)", sc.name, w.count(), runErr)
	}
	time.Sleep(150 * time.Millisecond)
	fullFrame = int(w.count())

	var ds []time.Duration
	var totalBytes int
	measured := 0
	step := func(k tea.KeyMsg, count bool) {
		water := w.count()
		t0 := time.Now()
		prog.Send(k)
		if !w.waitAbove(water, 5*time.Second) {
			prog.Quit()
			<-done
			tb.Fatalf("%s: e2e: key frame never rendered (bytes=%d runErr=%v)", sc.name, w.count(), runErr)
		}
		if count {
			ds = append(ds, time.Since(t0))
			totalBytes += int(w.count() - water)
			measured++
		}
	}
	for _, k := range keys[:probeWarm] {
		step(k, false)
	}
	for _, k := range keys[probeWarm:] {
		step(k, true)
	}
	prog.Quit()
	<-done
	return summarize(ds), totalBytes / measured, fullFrame
}

// TestKeyLatencyProbe prints a per-scenario typing-latency report. It runs
// only when GARESS_KEYPROBE=1 (see file header for cross-compile instructions).
func TestKeyLatencyProbe(t *testing.T) {
	if !keyprobeEnabled() {
		t.Skip("set GARESS_KEYPROBE=1 to run the typing-latency probe")
	}
	keys := probeKeyset(probeWarm + probeKeys)

	var b strings.Builder
	fmt.Fprintf(&b, "key-latency probe: %d warmup + %d measured keys per scenario\n", probeWarm, probeKeys)
	fmt.Fprintf(&b, "units: per-key Update/View CPU (our code), e2e key->terminal write (our code + bubbletea renderer), bytes\n\n")

	for _, sc := range latencyScenarios {
		upd, view, frameBytes, chgLines, chgBytes := probeRaw(t, sc, keys)
		e2e, bytesPerKey, fullFrame := probeE2E(t, sc, keys)

		fmt.Fprintf(&b, "=== %s (conv %d replies x %dw, term %dx%d) ===\n", sc.name, sc.turns, sc.words, sc.width, sc.height)
		fmt.Fprintf(&b, "  update (key handling, our code): %s\n", fmtDur(upd))
		fmt.Fprintf(&b, "  view   (frame compose, our code): %s\n", fmtDur(view))
		fmt.Fprintf(&b, "  frame size: %dB, changed lines/key: %d, changed bytes/key: %dB\n", frameBytes, chgLines, chgBytes)
		fmt.Fprintf(&b, "  e2e key->terminal (incl 60fps render): %s\n", fmtDur(e2e))
		fmt.Fprintf(&b, "  renderer bytes/key: %dB (full first frame: %dB)\n\n", bytesPerKey, fullFrame)
	}

	// Print the report (visible with -test.v; captured in the paste log).
	fmt.Print(b.String())
}
