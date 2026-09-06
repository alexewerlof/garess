package tui

import "github.com/charmbracelet/lipgloss"

// railChar is the left-rail glyph painted on user/assistant message lines
// (opencode-style). It is a full-height block, so a rail reads as a solid
// colored bar down the whole message.
const railChar = "▎"

// colors holds the resolved palette for the active theme (dark or light).
// Everything the TUI paints derives from these so "always-on, adapt to the
// terminal theme" is a single lookup.
type colors struct {
	accent      string // brand orange (logo, confirm, palette selection)
	accentDim   string // softer orange (logo secondary)
	dim         string // secondary text
	faint       string // tertiary text / internal bodies
	body        string // user message text
	info        string // info text
	err         string // errors
	streaming   string // in-flight assistant text
	user        string // user message rail + label
	assistant   string // assistant message rail
	panelBG     string // editor panel background (lighter than the terminal bg)
	panelBorder string // editor panel border
	selFG       string // palette selected foreground (on accent bg)
}

var darkColors = colors{
	accent:      "214", // warm orange
	accentDim:   "216",
	dim:         "245",
	faint:       "240",
	body:        "252",
	info:        "111",
	err:         "203",
	streaming:   "228",
	user:        "75",  // soft blue
	assistant:   "213", // pink/rose
	panelBG:     "236",
	panelBorder: "240",
	selFG:       "0",
}

var lightColors = colors{
	accent:      "130",
	accentDim:   "136",
	dim:         "242",
	faint:       "243",
	body:        "0",
	info:        "25",
	err:         "196",
	streaming:   "94",
	user:        "27",
	assistant:   "126",
	panelBG:     "255",
	panelBorder: "249",
	selFG:       "15",
}

type styles struct {
	// Hero / empty-state.
	logo    lipgloss.Style
	tagline lipgloss.Style
	hint    lipgloss.Style
	// Bottom chrome.
	status    lipgloss.Style
	err       lipgloss.Style
	streaming lipgloss.Style
	confirm   lipgloss.Style
	info      lipgloss.Style
	meta      lipgloss.Style // provider · model on the bottom bar
	// Editor panel.
	composer      lipgloss.Style
	composerRail  string // user left rail (over the panel bg) for the prompt box
	composerInner int    // cell width of the rail prefix (composer renders at w-inner)
	// Placeholder row style for the EMPTY composer (drawn directly in
	// composerBody — see model.go): dim text on the panel background.
	placeholderBody lipgloss.Style
	// Messages.
	userLabel lipgloss.Style
	userBody  lipgloss.Style
	// Internal (plain, dim) blocks.
	thinkingHeader lipgloss.Style
	thinkingBody   lipgloss.Style
	summaryHeader  lipgloss.Style
	toolHeader     lipgloss.Style // "⚙ name"
	toolBody       lipgloss.Style
	// Slash-command palette.
	palItem lipgloss.Style
	palDesc lipgloss.Style
	palSel  lipgloss.Style
	// Right session rail.
	railTitle   lipgloss.Style
	railLabel   lipgloss.Style // session list label line
	railPreview lipgloss.Style // session preview / card meta
	railSel     lipgloss.Style // selected session rows (accent bg)
	railCurrent lipgloss.Style // current-session card marker
	railHint    lipgloss.Style
	railDivider string // precomputed "│ " painted between content and rail
	// Precomputed per-line rail prefixes (see railPrefix).
	userRail      string
	assistantRail string
}

// ui is the active style set. It is theme-resolved once at startup by
// resolveUI (called from New); tests default to the dark set.
var ui = buildStyles(darkColors)

// resolveUI switches the package style set to the given theme (dark | light).
func resolveUI(theme string) {
	if theme == "light" {
		ui = buildStyles(lightColors)
		return
	}
	ui = buildStyles(darkColors)
}

func buildStyles(c colors) styles {
	base := lipgloss.NewStyle()
	// The prompt box wears the same left rail as user messages, painted over
	// the panel background so the bar reads as the box's left edge.
	composerRail := base.Foreground(lipgloss.Color(c.user)).
		Background(lipgloss.Color(c.panelBG)).
		Render(railChar + " ")
	return styles{
		logo:    base.Bold(true).Foreground(lipgloss.Color(c.accent)),
		tagline: base.Foreground(lipgloss.Color(c.dim)),
		hint:    base.Foreground(lipgloss.Color(c.faint)),

		status:    base.Foreground(lipgloss.Color(c.dim)),
		err:       base.Bold(true).Foreground(lipgloss.Color(c.err)),
		streaming: base.Bold(true).Foreground(lipgloss.Color(c.streaming)),
		confirm:   base.Bold(true).Foreground(lipgloss.Color(c.accent)),
		info:      base.Foreground(lipgloss.Color(c.info)),
		meta:      base.Foreground(lipgloss.Color(c.dim)),

		composer: base.
			// Editor panel: a subtly lighter background only (no border — the
			// color difference is the boundary) with a little breathing room.
			// No LEFT padding: the user rail prefix (composerRail) occupies
			// the two left cells, so typed text stays at the same column as
			// the conversation content.
			Background(lipgloss.Color(c.panelBG)).
			Padding(1, 2, 1, 0),

		userLabel: base.Bold(true).Foreground(lipgloss.Color(c.user)),
		userBody:  base.Foreground(lipgloss.Color(c.body)),

		thinkingHeader: base.Italic(true).Foreground(lipgloss.Color(c.dim)),
		thinkingBody:   base.Foreground(lipgloss.Color(c.faint)).PaddingLeft(2),
		summaryHeader:  base.Bold(true).Foreground(lipgloss.Color(c.dim)),
		toolHeader:     base.Bold(true).Foreground(lipgloss.Color(c.dim)),
		toolBody:       base.Foreground(lipgloss.Color(c.faint)),

		palItem: base.Foreground(lipgloss.Color(c.dim)),
		palDesc: base.Foreground(lipgloss.Color(c.faint)),
		palSel:  base.Bold(true).Foreground(lipgloss.Color(c.selFG)).Background(lipgloss.Color(c.accent)),

		railTitle:   base.Bold(true).Foreground(lipgloss.Color(c.accent)),
		railLabel:   base.Foreground(lipgloss.Color(c.dim)),
		railPreview: base.Foreground(lipgloss.Color(c.faint)),
		railSel:     base.Foreground(lipgloss.Color(c.selFG)).Background(lipgloss.Color(c.accent)),
		railCurrent: base.Bold(true).Foreground(lipgloss.Color(c.accent)),
		railHint:    base.Foreground(lipgloss.Color(c.faint)),
		railDivider: base.Foreground(lipgloss.Color(c.faint)).Render("│") + " ",

		composerRail:  composerRail,
		composerInner: lipgloss.Width(composerRail),

		placeholderBody: base.Foreground(lipgloss.Color(c.faint)).
			Background(lipgloss.Color(c.panelBG)),

		userRail:      railPrefixColor(c.user),
		assistantRail: railPrefixColor(c.assistant),
	}
}

func railPrefixColor(c string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(railChar) + " "
}

// railPrefix returns the ANSI prefix that paints the left rail for a zone's
// lines ("" for plain zones). Called per visible line in convView.view() —
// keep it a pure string lookup.
func railPrefix(z lineZone) string {
	switch z {
	case zoneUser:
		return ui.userRail
	case zoneAssistant:
		return ui.assistantRail
	default:
		return ""
	}
}
