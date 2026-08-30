package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/glamour"
)

// markdownRenderer renders markdown to ANSI for the terminal.
type markdownRenderer struct {
	tr *glamour.TermRenderer
}

// newMarkdownRenderer builds a renderer wrapping to the given width.
func newMarkdownRenderer(width int, theme string) (*markdownRenderer, error) {
	style := "dark"
	if theme == "light" {
		style = "light"
	}
	tr, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return nil, fmt.Errorf("init markdown renderer: %w", err)
	}
	return &markdownRenderer{tr: tr}, nil
}

// Render converts markdown to ANSI text.
func (r *markdownRenderer) Render(s string) (string, error) {
	out, err := r.tr.Render(s)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}
