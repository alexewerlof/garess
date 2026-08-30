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
	statBucketCount
)

var statNames = [statBucketCount]string{"flush", "viewport", "view", "adk"}

// tuiStats is a CPU-time breakdown of the hot rendering paths. It is a
// pointer field on Model (shared across Bubble Tea's value copies). Enable
// with GARESS_STATS=1; the reporter writes to stderr so the TUI (stdout)
// is never corrupted.
type tuiStats struct {
	mu sync.Mutex
	ns [statBucketCount]int64 // accumulated nanoseconds
	n  [statBucketCount]int64 // call counts
}

func newTUIStats() *tuiStats { return &tuiStats{} }

func statsEnabled() bool { return os.Getenv("GARESS_STATS") != "" }

func (s *tuiStats) add(b statBucket, d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ns[b] += int64(d)
	s.n[b]++
	s.mu.Unlock()
}

// reportLoop prints the per-bucket CPU time and call count for the last
// window to stderr, e.g.:
//
//	tui-stats: flush=1.2s(42) viewport=3.4s(42) view=2.1s(180) adk=0.9s(900)
//
// Run as a goroutine.
func (s *tuiStats) reportLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var prevNS, prevN [statBucketCount]int64
	for range t.C {
		s.mu.Lock()
		ns, n := s.ns, s.n
		s.mu.Unlock()
		var parts []string
		for i := 0; i < int(statBucketCount); i++ {
			d := time.Duration(ns[i] - prevNS[i]).Round(time.Millisecond)
			c := n[i] - prevN[i]
			parts = append(parts, fmt.Sprintf("%s=%s(%d)", statNames[i], d, c))
		}
		prevNS, prevN = ns, n
		fmt.Fprintf(os.Stderr, "tui-stats: %s\n", strings.Join(parts, " "))
	}
}
