package tui

import (
	"strings"
	"testing"
)

// TestUpdateViewportPreservesScroll verifies that streaming new content does
// not snap the view back to the bottom while the user has scrolled up, and
// that being at the bottom keeps it pinned there.
func TestUpdateViewportPreservesScroll(t *testing.T) {
	md, err := newMarkdownRenderer(80, "dark")
	if err != nil {
		t.Fatal(err)
	}
	m := &Model{
		md:                md,
		conv:              newConvView(5),
		assistantChunks:   newStreamChunker(),
		assistantThinking: newStreamChunkerWith(func(s string) string { return ui.thinkingBody.Render(s) }),
	}
	for i := 0; i < 10; i++ {
		m.rendered = append(m.rendered, "line")
	}
	m.updateViewport()
	m.conv.gotoTop()
	if m.conv.yOff == 0 {
		t.Fatal("expected to be scrolled up after gotoTop")
	}

	// Streaming a new batch must NOT snap back to the bottom.
	m.assistantActive = true
	m.assistantChunks.append(md, "streaming\n\ntail\n\n")
	m.assistantChunks.render(md)
	m.updateViewport()
	if m.conv.yOff == 0 {
		t.Error("updateViewport snapped back to bottom while scrolled up")
	}

	// At the bottom, new content stays pinned to the bottom.
	m.conv.gotoBottom()
	m.assistantChunks.append(md, "more\n\n")
	m.assistantChunks.render(md)
	m.updateViewport()
	if m.conv.yOff != 0 {
		t.Errorf("at bottom but yOff = %d", m.conv.yOff)
	}

	// Scrollback keys clamp within the new content bounds.
	m.scroll("home")
	if m.conv.yOff != m.conv.maxScroll() {
		t.Errorf("home yOff = %d, want %d", m.conv.yOff, m.conv.maxScroll())
	}
	m.scroll("end")
	if m.conv.yOff != 0 {
		t.Errorf("end yOff = %d, want 0", m.conv.yOff)
	}
	// At the bottom the newest streamed content is visible.
	if !strings.Contains(m.conv.view(), "more") {
		t.Errorf("view missing newest content: %q", m.conv.view())
	}
}
