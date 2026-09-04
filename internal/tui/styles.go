package tui

import "github.com/charmbracelet/lipgloss"

// Palette (dark theme).
const (
	colorAccent    = "214" // warm orange
	colorDim       = "245"
	colorUser      = "222"
	colorError     = "203"
	colorStreaming = "228"
	colorInfo      = "111"
	colorHeaderFg  = "230"
	colorHeaderBg  = "94"
)

type styles struct {
	header         lipgloss.Style
	userLabel      lipgloss.Style
	userBody       lipgloss.Style
	status         lipgloss.Style
	err            lipgloss.Style
	streaming      lipgloss.Style
	confirm        lipgloss.Style
	info           lipgloss.Style
	composer       lipgloss.Style
	thinkingHeader lipgloss.Style
	thinkingBody   lipgloss.Style
	summaryHeader  lipgloss.Style
}

var ui styles

func init() {
	base := lipgloss.NewStyle()
	ui.header = base.
		Bold(true).
		Foreground(lipgloss.Color(colorHeaderFg)).
		Background(lipgloss.Color(colorHeaderBg)).
		Padding(0, 1)
	ui.userLabel = base.Bold(true).Foreground(lipgloss.Color(colorAccent))
	ui.userBody = base.Foreground(lipgloss.Color(colorUser))
	ui.status = base.Foreground(lipgloss.Color(colorDim))
	ui.err = base.Bold(true).Foreground(lipgloss.Color(colorError))
	ui.streaming = base.Bold(true).Foreground(lipgloss.Color(colorStreaming))
	ui.confirm = base.Bold(true).Foreground(lipgloss.Color(colorAccent))
	ui.info = base.Foreground(lipgloss.Color(colorInfo))
	ui.composer = base.
		Padding(0, 1).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colorDim))
	ui.thinkingHeader = base.Italic(true).Foreground(lipgloss.Color(colorDim))
	ui.thinkingBody = base.Foreground(lipgloss.Color("240")).PaddingLeft(2)
	ui.summaryHeader = base.Bold(true).Foreground(lipgloss.Color(colorInfo))
}
