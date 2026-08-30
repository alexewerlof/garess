package tui

import (
	"strings"
	"testing"
)

func TestAssistantChunkBoundary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		cap  int
		want int
	}{
		{"empty", "", 1200, -1},
		{"short no newline", "hello", 1200, -1},
		{"one blank line", "a\n\nb", 1200, 3},
		{"last blank line wins", "a\n\nb\n\nc", 1200, 6},
		{"cap on long paragraph", strings.Repeat("word ", 1000), 1200, 1200},
		{"blank lines inside fence kept", "```go\nf(){\n\n}\n```\n\nrest", 1200, 19},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := assistantChunkBoundary(tc.in, tc.cap); got != tc.want {
				t.Errorf("assistantChunkBoundary(%q, %d) = %d, want %d", tc.in, tc.cap, got, tc.want)
			}
		})
	}
}

func TestStreamChunkerFinalizesChunks(t *testing.T) {
	md, err := newMarkdownRenderer(80, "dark")
	if err != nil {
		t.Fatal(err)
	}
	sc := newStreamChunker()
	sc.append(md, "## Hello\n\nWorld\n\n")
	sc.append(md, "more text\n\n")
	sc.render(md)

	if len(sc.chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(sc.chunks))
	}
	joined := strings.Join(sc.chunks, "\n\n")
	// ANSI escape codes are interleaved between words, so check per-word
	// substrings rather than whole phrases.
	for _, want := range []string{"Hello", "World", "more", "text"} {
		if !strings.Contains(joined, want) {
			t.Errorf("chunks missing %q: %q", want, joined)
		}
	}
	if sc.partial.Len() != 0 {
		t.Errorf("partial not empty after finalization: %q", sc.partial.String())
	}
	if sc.partialR != "" {
		t.Errorf("partialR not empty after finalization: %q", sc.partialR)
	}
}

func TestStreamChunkerKeepsTailLive(t *testing.T) {
	md, err := newMarkdownRenderer(80, "dark")
	if err != nil {
		t.Fatal(err)
	}
	sc := newStreamChunker()
	sc.append(md, "first\n\nsec")
	sc.render(md)

	if len(sc.chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(sc.chunks))
	}
	if !strings.Contains(sc.chunks[0], "first") {
		t.Errorf("completed chunk missing 'first': %q", sc.chunks[0])
	}
	if sc.partial.String() != "sec" {
		t.Errorf("tail = %q, want %q", sc.partial.String(), "sec")
	}
	if !strings.Contains(sc.partialR, "sec") {
		t.Errorf("rendered tail missing 'sec': %q", sc.partialR)
	}

	// More text arrives; the tail grows and eventually finalizes. The final
	// boundary finalizes the longest stable prefix, so "second\n\nthird\n\n"
	// becomes one capped chunk (2 total), leaving no live tail.
	sc.append(md, "ond\n\nthird\n\n")
	sc.render(md)
	if len(sc.chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(sc.chunks))
	}
	if sc.partial.Len() != 0 {
		t.Errorf("partial not empty at end: %q", sc.partial.String())
	}
}

func TestViewIncludesStreamingSlot(t *testing.T) {
	md, err := newMarkdownRenderer(80, "dark")
	if err != nil {
		t.Fatal(err)
	}
	m := &Model{
		md:                md,
		conv:              newConvView(30),
		assistantChunks:   newStreamChunker(),
		assistantThinking: newStreamChunkerWith(func(s string) string { return ui.thinkingBody.Render(s) }),
		rendered:          []string{"[user]"},
	}
	m.assistantActive = true
	m.assistantChunks.append(md, "one\n\ntwo")
	m.assistantChunks.render(md)
	m.updateViewport()

	view := joinAll(m.conv)
	if !strings.Contains(view, "[user]") {
		t.Errorf("view missing completed events: %q", view)
	}
	if !strings.Contains(view, "one") || !strings.Contains(view, "two") {
		t.Errorf("view missing streamed text: %q", view)
	}

	// Hidden thinking still renders its header, not the body.
	m.assistantThinking.append(m.md, "secret reasoning\n\n")
	m.assistantThinking.render(m.md)
	m.showThinking = false
	m.updateViewport()
	view = joinAll(m.conv)
	if !strings.Contains(view, "thinking") {
		t.Errorf("hidden thinking header missing: %q", view)
	}
	if strings.Contains(view, "secret reasoning") {
		t.Errorf("hidden thinking body leaked: %q", view)
	}

	m.showThinking = true
	m.updateViewport()
	view = joinAll(m.conv)
	if !strings.Contains(view, "secret reasoning") {
		t.Errorf("shown thinking body missing: %q", view)
	}
}

// TestThinkingFreezesAboveResponse verifies that once the model emits visible
// text, the streamed thinking is frozen as a stable block ABOVE the response
// chunks (so finalized paragraphs stay visible) and drops out of the live tail.
func TestThinkingFreezesAboveResponse(t *testing.T) {
	md, err := newMarkdownRenderer(80, "dark")
	if err != nil {
		t.Fatal(err)
	}
	m := &Model{
		md:                md,
		conv:              newConvView(30),
		streamBuffer:      &strings.Builder{},
		thinkingBuffer:    &strings.Builder{},
		assistantChunks:   newStreamChunker(),
		assistantThinking: newStreamChunkerWith(func(s string) string { return ui.thinkingBody.Render(s) }),
	}
	m.showThinking = true
	m.thinkingBuffer.WriteString("the reasoning\n\n")
	m.streamBuffer.WriteString("First paragraph\n\nSecond paragraph\n\n")
	m.flushStream()

	if !m.thinkingFrozen {
		t.Fatal("thinking not frozen once visible text arrived")
	}
	if m.thinkingBlock == "" {
		t.Fatal("frozen thinking block is empty")
	}

	// Stable lines: the thinking block must appear before the response chunks.
	joined := strings.Join(m.conv.lines, "\n")
	ti := strings.Index(joined, "the reasoning")
	fi := strings.Index(joined, "First")
	si := strings.Index(joined, "Second")
	if ti < 0 || fi < 0 || si < 0 {
		t.Fatalf("content missing: thinking=%d first=%d second=%d in %q", ti, fi, si, joined)
	}
	if !(ti < fi && fi < si) {
		t.Errorf("display order wrong: thinking(%d) before first(%d) before second(%d)", ti, fi, si)
	}

	// The live tail no longer carries the frozen thinking.
	if strings.Contains(m.streamTail(), "the reasoning") {
		t.Error("streamTail still includes frozen thinking")
	}
}
