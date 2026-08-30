package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
)

// markdownDoc builds a synthetic agent response of at least minTokens words:
// prose paragraphs, headings, lists and code fences — the mix a model actually
// emits. Used by the rendering benchmarks below.
func markdownDoc(minTokens int) string {
	var b strings.Builder
	words := 0
	for i := 0; words < minTokens; i++ {
		switch i % 5 {
		case 0:
			fmt.Fprintf(&b, "## Section %d\n\n", i)
			words += 3
		case 1:
			b.WriteString("```go\nfunc example() error {\n\treturn nil\n}\n```\n\n")
			words += 10
		case 2:
			b.WriteString("- item one\n- item two\n- item three\n\n")
			words += 8
		default:
			b.WriteString("The quick brown fox jumps over the lazy dog and keeps running through the dense forest toward the distant village.\n\n")
			words += 19
		}
	}
	return b.String()
}

// BenchmarkGlamourRender measures the cost of rendering a whole message in one
// shot — the reason streaming is chunked (assistantChunkCap): only the small
// tail chunk is re-rendered per batch, not the whole growing document.
func BenchmarkGlamourRender(b *testing.B) {
	r, err := newMarkdownRenderer(80, "dark")
	if err != nil {
		b.Fatal(err)
	}
	for _, n := range []int{500, 1000, 2000, 4000, 8000} {
		doc := markdownDoc(n)
		b.Run(fmt.Sprintf("words=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := r.Render(doc); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkStreamingTick measures the per-tick render cost of the streaming
// path (flushStream → chunker append + tail render) for messages of growing
// size. Unlike the old full re-render, this cost stays bounded by
// assistantChunkCap regardless of how large the message gets.
func BenchmarkStreamingTick(b *testing.B) {
	md, _ := newMarkdownRenderer(80, "dark")
	for _, n := range []int{500, 2000, 8000} {
		b.Run(fmt.Sprintf("words=%d", n), func(b *testing.B) {
			sc := newStreamChunker()
			sc.append(md, markdownDoc(n)) // pre-consume: chunking cost is one-off
			sc.render(md)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sc.append(md, " more streamed words\n\n")
				sc.render(md)
			}
		})
	}
}

// BenchmarkViewportSetContent is a historical reference: the cost of the
// abandoned bubbles/viewport.SetContent (O(session) re-split per call) that
// convView replaced. Kept to justify the design and guard against
// reintroducing it — the sizes show how it grows with conversation length.
func BenchmarkViewportSetContent(b *testing.B) {
	r, _ := newMarkdownRenderer(80, "dark")
	for _, turns := range []int{1, 4, 12} { // conversation length in assistant replies
		var parts []string
		for i := 0; i < turns; i++ {
			out, _ := r.Render(markdownDoc(2000))
			parts = append(parts, out)
		}
		joined := strings.Join(parts, "\n\n")
		vp := viewport.New(76, 30)
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				vp.SetContent(joined)
				vp.GotoBottom()
			}
		})
	}
}

// benchModel builds a Model whose viewport is populated with `turns` completed
// assistant replies of `words` words plus a live streaming slot — the steady
// state where the TUI spends most of its CPU.
func benchModel(tb testing.TB, words, turns int) *Model {
	tb.Helper()
	const width, height = 100, 30
	md, err := newMarkdownRenderer(width-4, "dark")
	if err != nil {
		tb.Fatal(err)
	}
	m := &Model{
		width:             width,
		height:            height,
		conv:              newConvView(height - 4),
		textarea:          textarea.New(),
		md:                md,
		streamBuffer:      &strings.Builder{},
		thinkingBuffer:    &strings.Builder{},
		assistantChunks:   newStreamChunker(),
		assistantThinking: newStreamChunkerWith(func(s string) string { return ui.thinkingBody.Render(s) }),
	}
	for i := 0; i < turns; i++ {
		out, err := md.Render(markdownDoc(words))
		if err != nil {
			tb.Fatal(err)
		}
		m.rendered = append(m.rendered, out)
	}
	m.assistantActive = true
	m.assistantChunks.append(md, markdownDoc(words))
	m.assistantChunks.render(md)
	m.updateViewport()
	return m
}

// BenchmarkView measures the cost of one View() call — Bubble Tea invokes it
// after EVERY message (every streamed delta), so on a slow CPU it can add up
// to a lot of the total load even when nothing else is expensive.
func BenchmarkView(b *testing.B) {
	for _, turns := range []int{4, 12} {
		m := benchModel(b, 2000, turns)
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = m.View()
			}
		})
	}
}

// BenchmarkViewStreaming measures one streaming frame: the conversation tail
// re-rendered (updateViewport) plus the full View() compose — the
// steady-state per-batch cost while a response streams.
func BenchmarkViewStreaming(b *testing.B) {
	for _, turns := range []int{4, 12} {
		m := benchModel(b, 2000, turns)
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				m.updateViewport() // streaming tick: replace the live tail
				_ = m.View()
			}
		})
	}
}

// BenchmarkConvViewTick measures the NEW per-tick viewport update cost
// (updateViewport with convView): appending finalized chunks and replacing the
// live tail — O(delta), independent of conversation size.
func BenchmarkConvViewTick(b *testing.B) {
	md, _ := newMarkdownRenderer(80, "dark")
	for _, turns := range []int{1, 4, 12} {
		cv := newConvView(30)
		for range turns {
			out, _ := md.Render(markdownDoc(2000))
			cv.appendStable(out)
		}
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cv.setLive("streamed tail text\n\n") // small bounded tail
			}
		})
	}
}

// BenchmarkConvViewView measures the NEW per-frame View() rendering cost — a
// plain join of the visible window (O(height)), not O(session).
func BenchmarkConvViewView(b *testing.B) {
	md, _ := newMarkdownRenderer(80, "dark")
	for _, turns := range []int{1, 4, 12} {
		cv := newConvView(30)
		for range turns {
			out, _ := md.Render(markdownDoc(2000))
			cv.appendStable(out)
		}
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = cv.view()
			}
		})
	}
}
