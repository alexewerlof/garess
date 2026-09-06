package tui

// Tests for the empty-state hero screen and the slash-command palette.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"garess/internal/chat"
	"garess/internal/harness"
	"garess/internal/memory"
	"garess/internal/tools"
)

// newUserEvent builds a user display event.
func newUserEvent(s string) *session.Event {
	return &session.Event{LLMResponse: model.LLMResponse{
		Content: genai.NewContentFromText(s, genai.RoleUser),
	}}
}

// heroModel builds a full idle Model with an empty conversation (as New does).
func heroModel(t *testing.T) *Model {
	t.Helper()
	dir := t.TempDir()
	svc := chat.NewService(dir+"/sessions", 100)
	mem := memory.New(dir+"/local", dir+"/global")
	sessID := chat.NewSessionID()
	preamble := harness.NewPreamble()
	prov := testProvider(t, svc, tools.Policy{}, &scriptedModel{name: "m"}, preamble)
	m, err := New(map[string]*harness.Provider{"fake": prov}, "fake", "dark", "local", sessID, mem, preamble, t.TempDir(), 100, 30, Options{Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHeroEmptyState(t *testing.T) {
	m := heroModel(t)
	if !m.hero() {
		t.Fatal("empty model should be in hero state")
	}
	view := m.heroBlock()
	if !strings.Contains(view, "minimal AI harness") {
		t.Errorf("hero missing tagline: %q", view)
	}
	if !strings.Contains(view, "Ask garess anything") {
		t.Errorf("hero missing editor placeholder: %q", view)
	}
	if !strings.Contains(view, "██") {
		t.Errorf("hero missing pixel logo: %q", view)
	}
	// The hero block fills exactly the rows above the status bar.
	if n := len(strings.Split(view, "\n")); n != 29 {
		t.Errorf("hero rows = %d, want 29 (height 30 - status)", n)
	}
	// The status line shows provider · model on the left and version on the right.
	status := m.statusLine()
	if !strings.Contains(status, "fake · m") {
		t.Errorf("status missing model: %q", status)
	}
	if !strings.Contains(status, "1.2.3") {
		t.Errorf("status missing version: %q", status)
	}

	// Once a conversation exists the hero is gone.
	m.events = []*session.Event{newUserEvent("hello")}
	m.renderAll()
	if m.hero() {
		t.Error("model with a conversation should not be in hero state")
	}
	if view := m.View(); strings.Contains(view, "minimal AI harness") {
		t.Error("conversation view should not include the hero tagline")
	}
}

// paletteModel builds a bare Model whose textarea holds v (enough for the
// palette logic, which reads the composer and a couple of flags).
func paletteModel(v string) Model {
	ta := textarea.New()
	ta.SetValue(v)
	md, err := newMarkdownRenderer(80, "dark")
	if err != nil {
		panic(err)
	}
	return Model{
		textarea:          ta,
		md:                md,
		conv:              newConvView(20),
		assistantChunks:   newStreamChunker(),
		assistantThinking: newStreamChunker(),
		paletteShow:       true,
	}
}

func TestPaletteOpenAndFilter(t *testing.T) {
	if m := paletteModel("hello"); m.paletteOpen() {
		t.Error("plain text must not open the palette")
	}
	if m := paletteModel("/"); !m.paletteOpen() {
		t.Error("leading slash should open the palette")
	}
	if m := paletteModel("/new"); !m.paletteOpen() {
		t.Error("slash command should keep the palette open")
	}
	// Multi-line values (past a newline) never open it.
	m := paletteModel("/notes write x y")
	m.textarea.InsertString("\n")
	if m.paletteOpen() {
		t.Error("multi-line composer must not open the palette")
	}

	if got := paletteModel("/no").paletteRows(); len(got) != 1 || got[0].name != "/notes" {
		t.Errorf("filter '/no' rows = %+v, want [/notes]", got)
	}
	if got := paletteModel("/").paletteRows(); len(got) < 5 {
		t.Errorf("filter '/' rows = %d, want all commands", len(got))
	}
}

func TestPaletteVisibility(t *testing.T) {
	// Partial word with matches → visible.
	if !paletteModel("/no").paletteVisible() {
		t.Error("'/no' should show the palette")
	}
	// Exact command name → nothing to choose (Enter runs it) → hidden.
	if paletteModel("/new").paletteVisible() {
		t.Error("exact '/new' should hide the palette")
	}
	// After esc (paletteShow=false) the list stays hidden until the next edit.
	m := paletteModel("/no")
	m.paletteShow = false
	if m.paletteVisible() {
		t.Error("esc-dismissed palette should stay hidden")
	}
	// No matches → hidden.
	if paletteModel("/xyzzy").paletteVisible() {
		t.Error("no matching commands should hide the palette")
	}
}

func TestPaletteComplete(t *testing.T) {
	m := paletteModel("/no")
	m.paletteSel = 0
	got, _ := m.paletteComplete()
	if nm, ok := got.(Model); !ok {
		t.Fatalf("paletteComplete returned %T, want Model", got)
	} else if v := nm.textarea.Value(); v != "/notes" {
		t.Errorf("completion value = %q, want /notes", v)
	}
	// Text after the word is preserved (arguments).
	m2 := paletteModel("/no read x")
	m2.paletteSel = 0
	got2, _ := m2.paletteComplete()
	if nm2, ok := got2.(Model); !ok {
		t.Fatalf("paletteComplete returned %T, want Model", got2)
	} else if v := nm2.textarea.Value(); v != "/notes read x" {
		t.Errorf("completion with args = %q, want /notes read x", v)
	}
}
