package tui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// statBucket identifies a hot TUI path whose CPU time is accumulated when
// GARESS_STATS=1. Accumulation happens on the single Bubble Tea loop
// goroutine; a reporter goroutine drains it periodically.
type statBucket int

const (
	statFlush    statBucket = iota // flushStream: deltas -> chunk finalize + tail glamour render
	statViewport                   // updateViewport: append stable parts + replace live tail
	statView                       // View(): full frame composition (Bubble Tea calls it after every message)
	statADK                        // handleADK: pumping runner events
	statInput                      // handleKey: keystroke handling (textarea, scroll, commands)
	statBucketCount
)

var statNames = [statBucketCount]string{"flush", "viewport", "view", "adk", "input"}

// viewPart identifies the sub-components of View() (reported separately so we
// can see which one dominates on a slow CPU when the plain `view` bucket says
// the whole frame is expensive).
type viewPart int

const (
	viewPartHeader viewPart = iota
	viewPartConv
	viewPartComposer
	viewPartStatus
	viewPartCount
)

var viewPartNames = [viewPartCount]string{"hdr", "conv", "comp", "status"}

// tuiStats is a CPU-time breakdown of the hot paths. It is a pointer field on
// Model (shared across Bubble Tea's value copies). Enable with GARESS_STATS=1;
// the reporter writes to stderr so the TUI (stdout) is never corrupted.
type tuiStats struct {
	mu sync.Mutex
	ns [statBucketCount]int64 // accumulated nanoseconds
	n  [statBucketCount]int64 // call counts
	mx [statBucketCount]int64 // slowest single call (ns) — typing latency is a max/p95 problem, not an average one

	vpNS [viewPartCount]int64 // View() sub-component accumulators (window totals)
	vpN  [viewPartCount]int64
}

func newTUIStats() *tuiStats { return &tuiStats{} }

func statsEnabled() bool { return os.Getenv("GARESS_STATS") != "" }

func (s *tuiStats) add(b statBucket, d time.Duration) {
	if s == nil {
		return
	}
	ns := int64(d)
	s.mu.Lock()
	s.ns[b] += ns
	s.n[b]++
	if ns > s.mx[b] {
		s.mx[b] = ns
	}
	s.mu.Unlock()
}

func (s *tuiStats) addViewPart(p viewPart, d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.vpNS[p] += int64(d)
	s.vpN[p]++
	s.mu.Unlock()
}

// reportLoop prints the per-bucket CPU time and call count for the last
// window to stderr, e.g.:
//
//	tui-stats: flush=1.2s(42) viewport=3.4s(42) view=2.1s(180) adk=0.9s(900) input=0.5s(3000) max[view]=12.3ms
//	tui-stats-view: hdr=0.1s(180) conv=0.3s(180) comp=1.4s(180) status=0.05s(180)
//
// Run as a goroutine.
func (s *tuiStats) reportLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var prevNS, prevN [statBucketCount]int64
	var prevVPNS, prevVPN [viewPartCount]int64
	for range t.C {
		s.mu.Lock()
		ns, n, mx := s.ns, s.n, s.mx
		vpNS, vpN := s.vpNS, s.vpN
		s.mu.Unlock()
		var parts []string
		for i := 0; i < int(statBucketCount); i++ {
			d := time.Duration(ns[i] - prevNS[i]).Round(time.Millisecond)
			c := n[i] - prevN[i]
			parts = append(parts, fmt.Sprintf("%s=%s(%d)", statNames[i], d, c))
		}
		// Slowest single call per bucket since startup (max over the run so
		// far) — catches the occasional 100ms+ key/frame even when the window
		// average hides it.
		var mxParts []string
		for i := 0; i < int(statBucketCount); i++ {
			if m := time.Duration(mx[i]); m > 0 {
				mxParts = append(mxParts, fmt.Sprintf("%s=%s", statNames[i], m.Round(time.Microsecond)))
			}
		}
		// View() sub-component breakdown for the window — shows which part of
		// View() is expensive on a slow CPU.
		var vpParts []string
		for i := 0; i < int(viewPartCount); i++ {
			d := time.Duration(vpNS[i] - prevVPNS[i]).Round(time.Millisecond)
			c := vpN[i] - prevVPN[i]
			vpParts = append(vpParts, fmt.Sprintf("%s=%s(%d)", viewPartNames[i], d, c))
		}
		prevNS, prevN = ns, n
		prevVPNS, prevVPN = vpNS, vpN
		fmt.Fprintf(os.Stderr, "tui-stats: %s | max %s\n", strings.Join(parts, " "), strings.Join(mxParts, " "))
		fmt.Fprintf(os.Stderr, "tui-stats-view: %s\n", strings.Join(vpParts, " "))
	}
}
