package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestComposerCarriesUserRail: the prompt box wears the same left rail as
// user messages — every one of its rows is prefixed with the composer rail,
// and the panel still spans exactly the requested width.
func TestComposerCarriesUserRail(t *testing.T) {
	m := idleTypingModel(t, latencyScenario{name: "composer", width: 120, height: 30})
	box := m.editorBox(120)

	rows := strings.Split(box, "\n")
	if len(rows) != 4 { // 1 top pad + 2 textarea rows + 1 bottom pad
		t.Fatalf("composer rows = %d, want 4", len(rows))
	}
	for i, row := range rows {
		if !strings.HasPrefix(row, ui.composerRail) {
			t.Errorf("row %d does not start with the user rail", i)
		}
		if got := lipgloss.Width(row); got != 120 {
			t.Errorf("row %d width = %d, want 120 (rail + panel)", i, got)
		}
	}
}

// TestComposerTextareaMatchesPanelBackground: the textarea content styles must
// carry the panel background. The textarea emits style resets around its
// typed text; without baking the panel color in, those rows would fall back
// to the terminal's default (darker) background instead of the panel's.
func TestComposerTextareaMatchesPanelBackground(t *testing.T) {
	c := darkColors
	check := func(label string, style lipgloss.Style) {
		t.Helper()
		if got := style.GetBackground(); got != lipgloss.Color(c.panelBG) {
			t.Errorf("%s background = %q, want panel %q", label, got, c.panelBG)
		}
	}

	var ta textarea.Model
	styleTextarea(&ta, "dark")
	check("focused.Text", ta.FocusedStyle.Text)
	check("focused.CursorLine", ta.FocusedStyle.CursorLine)
	check("focused.Placeholder", ta.FocusedStyle.Placeholder)
	check("focused.EndOfBuffer", ta.FocusedStyle.EndOfBuffer)
	check("blurred.Text", ta.BlurredStyle.Text)
	check("blurred.CursorLine", ta.BlurredStyle.CursorLine)
}

// TestComposerCursorFollowsEmptiness: while the composer is empty the block
// cursor is hidden (bubbles would otherwise draw its reverse box over the
// first placeholder character — the "black text" next to the placeholder).
// Typing the first character restores the blinking block cursor; clearing the
// text hides it again.
func TestComposerCursorFollowsEmptiness(t *testing.T) {
	m := idleTypingModel(t, latencyScenario{name: "cursor", width: 120, height: 30})
	styleTextarea(&m.textarea, "dark")
	m.textarea.Focus()
	if m.textarea.Value() != "" {
		t.Fatalf("test composer not empty: %q", m.textarea.Value())
	}

	_ = m.reconcileComposerCursor()
	if got := m.textarea.Cursor.Mode(); got != cursor.CursorHide {
		t.Fatalf("empty composer cursor mode = %v, want CursorHide", got)
	}

	m.textarea.SetValue("h")
	cmd := m.reconcileComposerCursor()
	if got := m.textarea.Cursor.Mode(); got != cursor.CursorBlink {
		t.Fatalf("typed composer cursor mode = %v, want CursorBlink", got)
	}
	if cmd == nil {
		t.Error("empty->text transition should return the blink-start command")
	}

	m.textarea.SetValue("")
	_ = m.reconcileComposerCursor()
	if got := m.textarea.Cursor.Mode(); got != cursor.CursorHide {
		t.Fatalf("re-cleared composer cursor mode = %v, want CursorHide", got)
	}
}

// TestComposerRowsNeverFallBackToTerminalBackground renders the composer with
// color forced and walks every row's ANSI state to assert that NO visible
// cell is left on the terminal's default background (the "black box" after
// the placeholder). Covers BOTH the empty (placeholder) and typed states. The
// empty state must additionally contain no reverse-video cursor block. This
// forces the color profile and restores it (tests run sequentially).
func TestComposerRowsNeverFallBackToTerminalBackground(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		resolveUI("dark") // rebuild the precomputed style strings colorless
	}()
	resolveUI("dark")

	build := func(value string) *Model {
		m := idleTypingModel(t, latencyScenario{name: "bg", width: 120, height: 30})
		styleTextarea(&m.textarea, "dark")
		m.textarea.Focus()
		if value != "" {
			m.textarea.SetValue(value)
		}
		m.reconcileComposerCursor()
		return m
	}
	panel, _ := strconv.Atoi(darkColors.panelBG)

	for _, tc := range []struct{ label, value string }{
		{"empty (placeholder)", ""},
		{"typed", "hi there"},
	} {
		rows := strings.Split(build(tc.value).editorBox(120), "\n")
		if len(rows) != 4 {
			t.Fatalf("%s: composer rows = %d, want 4", tc.label, len(rows))
		}
		for i, row := range rows {
			if got := lipgloss.Width(row); got != 120 {
				t.Errorf("%s row %d width = %d, want 120", tc.label, i, got)
			}
			if !cellsOnBackground(row, panel) {
				t.Errorf("%s row %d has cells on the terminal default background:\n%q", tc.label, i, row)
			}
			if tc.value == "" && strings.Contains(row, "\x1b[7m") {
				t.Errorf("%s row %d still paints a reverse-video cursor block", tc.label, i)
			}
		}
	}
}

// cellsOnBackground walks one rendered row's SGR state and reports whether
// every visible cell is painted on the given 256-color background index.
// Requires a color-enabled render (lipgloss is colorless under Ascii).
func cellsOnBackground(row string, panel int) bool {
	bg := -1 // -1 = default background
	i := 0
	for i < len(row) {
		c := row[i]
		if c == 0x1b {
			if i+1 < len(row) && row[i+1] == '[' {
				j := i + 2
				for j < len(row) && row[j] != 'm' {
					j++
				}
				if j >= len(row) {
					return true
				}
				params := strings.Split(row[i+2:j], ";")
				i = j + 1
				for k := 0; k < len(params); k++ {
					switch params[k] {
					case "0":
						bg = -1
					case "48": // 48;5;<n> 256-color background
						if k+2 < len(params) && params[k+1] == "5" {
							if n, err := strconv.Atoi(params[k+2]); err == nil {
								bg = n
							}
							k += 2
						}
					}
				}
				continue
			}
			// Non-CSI escape: skip to its final byte.
			i++
			for i < len(row) && (row[i] < 0x40 || row[i] > 0x7e) {
				i++
			}
			if i < len(row) {
				i++
			}
			continue
		}
		if bg != panel {
			return false
		}
		i++
	}
	return true
}
