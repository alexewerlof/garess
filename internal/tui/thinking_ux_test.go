package tui

// Tests for the live, in-conversation "Thinking" slot (opencode/openrouter
// style): a "Thinking" indicator appears under the last content while a model
// generation is pending, and ctrl+t expands it to stream the thoughts.

import (
	"strings"
	"testing"
)

func thinkingTestModel() *Model {
	md, err := newMarkdownRenderer(80, "dark")
	if err != nil {
		panic(err)
	}
	return &Model{
		md:                md,
		conv:              newConvView(30),
		streamBuffer:      &strings.Builder{},
		thinkingBuffer:    &strings.Builder{},
		assistantChunks:   newStreamChunker(),
		assistantThinking: newStreamChunkerWith(func(s string) string { return ui.thinkingBody.Render(s) }),
	}
}

func TestThinkingLivePlaceholderWhileGenerationPending(t *testing.T) {
	m := thinkingTestModel()
	m.streaming = true
	m.generationPending = true // run started, no deltas yet
	m.updateViewport()

	view := joinAll(m.conv)
	if !strings.Contains(view, "Thinking") {
		t.Errorf("live Thinking placeholder missing while generation pending: %q", view)
	}
	// The placeholder animates with the spinner.
	before := joinAll(m.conv)
	m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
	m.updateViewport()
	if joinAll(m.conv) == before {
		t.Error("Thinking placeholder should animate with the spinner")
	}

	// Once the run ends there is nothing live left.
	m.streaming = false
	m.generationPending = false
	m.updateViewport()
	if got := joinAll(m.conv); got != "" {
		t.Errorf("live slot should clear after the run, got %q", got)
	}
}

func TestThinkingLiveExpandsWithStreamedThoughts(t *testing.T) {
	m := thinkingTestModel()
	m.streaming = true
	m.generationPending = true

	// Thinking streams: the generation is still pending (no visible text).
	m.thinkingBuffer.WriteString("the secret reasoning\n\n")
	m.flushStream()
	if !m.generationPending {
		t.Error("streaming thoughts alone should keep the generation pending")
	}
	if !m.assistantActive {
		t.Error("streaming thoughts should mark the assistant active")
	}

	// Collapsed by default: header only, no reasoning.
	m.showThinking = false
	m.updateViewport()
	view := joinAll(m.conv)
	if !strings.Contains(view, "Thinking") {
		t.Errorf("collapsed Thinking header missing: %q", view)
	}
	if strings.Contains(view, "secret reasoning") {
		t.Errorf("collapsed Thinking must not leak the reasoning: %q", view)
	}

	// ctrl+t (showThinking) expands the title and streams the thoughts live.
	m.showThinking = true
	m.updateViewport()
	view = joinAll(m.conv)
	if !strings.Contains(view, "▼ Thinking") {
		t.Errorf("expanded Thinking title missing: %q", view)
	}
	if !strings.Contains(view, "secret reasoning") {
		t.Errorf("expanded Thinking should stream the reasoning: %q", view)
	}
}

func TestThinkingPendingEndsWhenTextStarts(t *testing.T) {
	m := thinkingTestModel()
	m.streaming = true
	m.generationPending = true

	m.thinkingBuffer.WriteString("thoughts first\n\n")
	m.streamBuffer.WriteString("First paragraph\n\nSecond paragraph\n\n")
	m.flushStream()

	if m.generationPending {
		t.Error("visible text should end the thinking phase")
	}
	if !m.thinkingFrozen {
		t.Error("thinking should freeze once visible text starts")
	}
}
