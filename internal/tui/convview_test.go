package tui

import (
	"strings"
	"testing"
)

func TestConvViewAppendStableAndLive(t *testing.T) {
	cv := newConvView(3)

	cv.appendStable("line1\nline2")
	if len(cv.lines) != 2 {
		t.Fatalf("stable lines = %d, want 2", len(cv.lines))
	}
	if got := strings.Join(cv.lines, "|"); got != "line1|line2" {
		t.Fatalf("lines = %q", got)
	}

	// The live tail is replaced in place; the stable prefix is untouched.
	cv.setLive("tailA\nmoreA")
	if cv.lineCount() != 4 {
		t.Fatalf("lineCount after setLive = %d, want 4", cv.lineCount())
	}
	cv.setLive("tailB")
	if cv.lineCount() != 3 {
		t.Fatalf("lineCount after second setLive = %d, want 3", cv.lineCount())
	}
	if got := joinAll(cv); got != "line1|line2|tailB" {
		t.Fatalf("lines = %q, want line1|line2|tailB", got)
	}

	// New stable content still appends before the live tail.
	cv.appendStable("line3")
	cv.setLive("tailC")
	if got := joinAll(cv); got != "line1|line2|line3|tailC" {
		t.Fatalf("lines = %q", got)
	}

	cv.clearLive()
	if got := joinAll(cv); got != "line1|line2|line3" {
		t.Fatalf("after clearLive = %q", got)
	}
}

// joinAll renders all lines (stable + live) in display order for assertions.
func joinAll(cv *convView) string {
	parts := make([]string, 0, cv.lineCount())
	for i := 0; i < cv.lineCount(); i++ {
		parts = append(parts, cv.lineAt(i))
	}
	return strings.Join(parts, "|")
}

func TestConvViewScrolling(t *testing.T) {
	cv := newConvView(3)
	cv.appendStable("l0\nl1\nl2\nl3\nl4")
	// 5 lines, height 3 → max scroll 2.
	if cv.maxScroll() != 2 {
		t.Fatalf("maxScroll = %d, want 2", cv.maxScroll())
	}
	cv.gotoTop()
	if cv.yOff != 2 {
		t.Fatalf("gotoTop yOff = %d, want 2", cv.yOff)
	}
	cv.scrollDown(1)
	if cv.yOff != 1 {
		t.Fatalf("scrollDown yOff = %d, want 1", cv.yOff)
	}
	cv.scrollDown(5)
	if cv.yOff != 0 {
		t.Fatalf("scrollDown clamp yOff = %d, want 0", cv.yOff)
	}
	cv.gotoTop()
	cv.scrollUp(5)
	if cv.yOff != 2 {
		t.Fatalf("scrollUp clamp yOff = %d, want 2", cv.yOff)
	}
	// New content keeps the offset clamped to the new max.
	cv.scrollDown(1)
	cv.appendStable("l5\nl6")
	if cv.maxScroll() != 4 {
		t.Fatalf("maxScroll after append = %d, want 4", cv.maxScroll())
	}
	if cv.yOff > cv.maxScroll() {
		t.Fatalf("yOff %d exceeds maxScroll %d", cv.yOff, cv.maxScroll())
	}
}

func TestConvViewViewWindow(t *testing.T) {
	cv := newConvView(3)
	cv.appendStable("l0\nl1\nl2\nl3\nl4")
	// Bottom: last 3 lines.
	if got := cv.view(); got != "l2\nl3\nl4" {
		t.Fatalf("bottom view = %q, want %q", got, "l2\nl3\nl4")
	}
	cv.gotoTop()
	if got := cv.view(); got != "l0\nl1\nl2" {
		t.Fatalf("top view = %q, want %q", got, "l0\nl1\nl2")
	}
}

func TestConvViewViewPadsWhenShort(t *testing.T) {
	cv := newConvView(3)
	cv.appendStable("hello")
	out := cv.view()
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("short view has %d lines, want 3 (padded to height)", len(lines))
	}
	if lines[0] != "hello" {
		t.Fatalf("first line = %q", lines[0])
	}
}

func TestConvViewRails(t *testing.T) {
	cv := newConvView(20)
	// User + assistant zones get a rail; the blank separator line inside a
	// railed block must NOT (so each message reads as its own railed block);
	// plain zones never get one.
	cv.appendStableZ("user one\n\nuser two", zoneUser)
	cv.appendStableZ("assistant reply", zoneAssistant)
	cv.appendStable("plain block")
	out := cv.view()
	lines := strings.Split(out, "\n")
	if len(lines) < 5 {
		t.Fatalf("view lines = %d (%q)", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], ui.userRail) {
		t.Errorf("user line 1 missing rail: %q", lines[0])
	}
	if lines[1] != "" {
		t.Errorf("blank line should stay clean, got %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], ui.userRail) {
		t.Errorf("user line 2 missing rail: %q", lines[2])
	}
	if !strings.HasPrefix(lines[3], ui.assistantRail) {
		t.Errorf("assistant line missing rail: %q", lines[3])
	}
	if strings.HasPrefix(lines[4], railChar) {
		t.Errorf("plain block must not be railed: %q", lines[4])
	}
}

func TestConvViewSetLiveZone(t *testing.T) {
	cv := newConvView(10)
	cv.setLiveZ("streaming tail", zoneAssistant)
	if len(cv.live) != 1 || len(cv.liveZones) != 1 {
		t.Fatalf("live lines/zones = %d/%d", len(cv.live), len(cv.liveZones))
	}
	if cv.zoneAt(0) != zoneAssistant {
		t.Errorf("live zone = %v, want assistant", cv.zoneAt(0))
	}
	cv.clearLive()
	if len(cv.live) != 0 || len(cv.liveZones) != 0 {
		t.Errorf("clearLive left live=%d zones=%d", len(cv.live), len(cv.liveZones))
	}
}
