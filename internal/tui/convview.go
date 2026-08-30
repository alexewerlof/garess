package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// convView is a bottom-anchored, append-only line viewer for the conversation.
//
// bubbles/viewport was the original widget, but its SetContent re-splits and
// re-measures the WHOLE conversation on every call — O(session) per 50ms
// render tick — and its View() re-applies per-line lipgloss padding on every
// frame, which together pegged a Raspberry Pi 1 core. convView keeps frozen
// stable lines separate from a small live tail: stable content is appended
// once (O(delta)) and the tail is replaced in place (O(tail)); rendering is a
// plain join of the visible window (O(height)).
type convView struct {
	lines  []string // frozen completed content
	live   []string // live streaming tail (replaced every tick)
	yOff   int      // lines scrolled up from the bottom (0 = at bottom)
	height int
}

func newConvView(height int) *convView {
	return &convView{height: height}
}

// reset discards all content (next append starts a full rebuild).
func (v *convView) reset() {
	v.lines = nil
	v.live = nil
	v.yOff = 0
}

// appendStable adds a piece of completed content to the frozen region.
func (v *convView) appendStable(s string) {
	v.appendTo(&v.lines, s)
}

// setLive replaces the live tail (the streaming assistant slot) in place.
func (v *convView) setLive(s string) {
	v.live = v.live[:0]
	v.appendTo(&v.live, s)
	v.clamp()
}

// clearLive drops the live tail.
func (v *convView) clearLive() { v.live = v.live[:0] }

func (v *convView) appendTo(dst *[]string, s string) {
	if s == "" {
		return
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	*dst = append(*dst, strings.Split(s, "\n")...)
}

// lineCount returns the total number of lines (stable + live).
func (v *convView) lineCount() int { return len(v.lines) + len(v.live) }

// lineAt returns the i-th line across stable + live.
func (v *convView) lineAt(i int) string {
	if i < len(v.lines) {
		return v.lines[i]
	}
	return v.live[i-len(v.lines)]
}

// --- scrolling (bottom-anchored) ----------------------------------------

func (v *convView) maxScroll() int { return max(0, v.lineCount()-v.height) }

func (v *convView) clamp() {
	if v.yOff > v.maxScroll() {
		v.yOff = v.maxScroll()
	}
}

func (v *convView) gotoBottom() { v.yOff = 0 }

func (v *convView) gotoTop() { v.yOff = v.maxScroll() }

func (v *convView) scrollUp(n int)   { v.yOff = min(v.yOff+n, v.maxScroll()) }
func (v *convView) scrollDown(n int) { v.yOff = max(0, v.yOff-n) }

func (v *convView) pageUp()   { v.scrollUp(max(v.height-1, 1)) }
func (v *convView) pageDown() { v.scrollDown(max(v.height-1, 1)) }

// view renders the visible window. When content fills the viewport (the common
// streaming case) this is a plain line join with no per-line styling; short
// content is padded vertically so the composer stays anchored at the bottom.
func (v *convView) view() string {
	n := v.lineCount()
	if v.height <= 0 || n == 0 {
		return ""
	}
	end := n - v.yOff
	start := max(0, end-v.height)
	window := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		window = append(window, v.lineAt(i))
	}
	if len(window) >= v.height {
		return strings.Join(window, "\n")
	}
	return lipgloss.NewStyle().Height(v.height).Render(strings.Join(window, "\n"))
}
