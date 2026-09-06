package tui

import (
	"strings"
)

// lineZone tags a conversation line with its visual treatment. Railed zones
// (user, assistant) are drawn with a colored left rail in view(); zonePlain
// lines — tool/thinking internals, summaries, ephemera, the compression
// indicator — are drawn as-is. Zones live on the LINES (not whole messages)
// so a rail stays continuous across chunk boundaries while streaming and so
// mixed-zone live tails (thinking above a response) render correctly.
type lineZone uint8

const (
	zonePlain lineZone = iota
	zoneUser
	zoneAssistant
)

func (z lineZone) railed() bool { return z == zoneUser || z == zoneAssistant }

// taggedPart is content plus the zone its lines belong to. Stable parts and
// the pieces of the live tail carry one so view() can paint rails per line.
type taggedPart struct {
	text string
	zone lineZone
}

// convView is a bottom-anchored, append-only line viewer for the conversation.
//
// bubbles/viewport was the original widget, but its SetContent re-splits and
// re-measures the WHOLE conversation on every call — O(session) per 50ms
// render tick — and its View() re-applies per-line lipgloss padding on every
// frame, which together pegged a Raspberry Pi 1 core. convView keeps frozen
// stable lines separate from a small live tail: stable content is appended
// once (O(delta)) and the tail is replaced in place (O(tail)); rendering is a
// plain join of the visible window (O(height)). Rails are painted as a cheap
// per-line ANSI prefix on visible railed lines (precomputed prefix strings —
// no width measurement, no lipgloss on the hot path).
type convView struct {
	lines     []string   // frozen completed content
	zones     []lineZone // parallel to lines
	live      []string   // live streaming tail (replaced every tick)
	liveZones []lineZone // parallel to live
	yOff      int        // lines scrolled up from the bottom (0 = at bottom)
	height    int
}

func newConvView(height int) *convView {
	return &convView{height: height}
}

// reset discards all content (next append starts a full rebuild).
func (v *convView) reset() {
	v.lines = nil
	v.zones = nil
	v.live = nil
	v.liveZones = nil
	v.yOff = 0
}

// appendStable adds a piece of completed content to the frozen region with no
// rail (plain zone). Zone-tagged content goes through appendStableZ.
func (v *convView) appendStable(s string) {
	v.appendStableZ(s, zonePlain)
}

// appendStableZ adds completed content tagged with a zone.
func (v *convView) appendStableZ(s string, z lineZone) {
	v.appendTo(&v.lines, &v.zones, s, z)
}

// setLive replaces the live tail with a single plain piece (replaced in
// place). Zone-tagged tails go through setLiveZ.
func (v *convView) setLive(s string) {
	v.setLiveZ(s, zonePlain)
}

// setLiveZ replaces the live tail with one zone-tagged piece in place.
func (v *convView) setLiveZ(s string, z lineZone) {
	v.live = v.live[:0]
	v.liveZones = v.liveZones[:0]
	v.appendTo(&v.live, &v.liveZones, s, z)
	v.clamp()
}

// clearLive drops the live tail.
func (v *convView) clearLive() {
	v.live = v.live[:0]
	v.liveZones = v.liveZones[:0]
}

func (v *convView) appendTo(dst *[]string, zs *[]lineZone, s string, z lineZone) {
	if s == "" {
		return
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	*dst = append(*dst, lines...)
	for range lines {
		*zs = append(*zs, z)
	}
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

// zoneAt returns the zone of the i-th line across stable + live.
func (v *convView) zoneAt(i int) lineZone {
	if i < len(v.lines) {
		return v.zones[i]
	}
	return v.liveZones[i-len(v.lines)]
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
// streaming case) this is a plain line join with no per-line styling; railed
// zones get a precomputed ANSI rail prefix (a cheap string concat per line,
// no width measurement). Short content is padded vertically so the composer
// stays anchored at the bottom.
func (v *convView) view() string {
	n := v.lineCount()
	if v.height <= 0 || n == 0 {
		return ""
	}
	end := n - v.yOff
	start := max(0, end-v.height)
	window := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		line := v.lineAt(i)
		// Paint the rail on railed zones, but skip truly blank lines so a
		// message's rail starts and ends at its own content (message
		// separators stay clean).
		if p := railPrefix(v.zoneAt(i)); p != "" && line != "" {
			line = p + line
		}
		window = append(window, line)
	}
	joined := strings.Join(window, "\n")
	if len(window) >= v.height {
		return joined
	}
	// Content is shorter than the viewport: pad with plain newlines so the
	// composer stays anchored at the bottom. Do NOT use
	// lipgloss.NewStyle().Height(v.height).Render(...) here — a Height style
	// makes lipgloss run its horizontal re-align pass (alignTextHorizontal),
	// which measures the ANSI width of EVERY line and re-pads them all, every
	// frame. That is O(frame) per View and ~100x a plain join; on a Pi 1 it
	// cost tens of ms per frame whenever the conversation was shorter than the
	// terminal (GARESS_STATS `conv` bucket, e.g. 32-42ms at 180x45). Blank
	// padding lines are equivalent on screen.
	return joined + strings.Repeat("\n", v.height-len(window))
}
