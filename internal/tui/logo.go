package tui

import "strings"

// logoFont is a tiny 5x5 block font used to render the "garess" hero logo.
// Each row is a string of '1' (pixel) and '0' (gap). Pixels are rendered
// double-width (██) so the 5x5 grid reads square on terminal cells.
var logoFont = map[rune][]string{
	'g': {
		"01110",
		"10001",
		"10000",
		"10011",
		"01110",
	},
	'a': {
		"01110",
		"10001",
		"11111",
		"10001",
		"10001",
	},
	'r': {
		"11110",
		"10001",
		"11110",
		"10001",
		"10001",
	},
	'e': {
		"11111",
		"10000",
		"11110",
		"10000",
		"11111",
	},
	's': {
		"01111",
		"10000",
		"01110",
		"00001",
		"11110",
	},
}

// logoWord is the pixel-font word shown on the hero screen.
const logoWord = "garess"

// logoLineWidth returns the display width (in terminal columns) of one logo
// line for the word, i.e. (pixels*2) per letter plus one gap column between
// letters.
const (
	logoPixelW = 2 // rendered columns per font pixel
	logoHeight = 5 // font rows
	logoGapCol = 2 // terminal columns between letters
)

// logoWidth is the terminal-column width of the rendered logo.
func logoWidth() int {
	return len(logoWord)*logoFontW*logoPixelW + (len(logoWord)-1)*logoGapCol
}

const logoFontW = 5

// logoLines renders the hero logo as double-width block lines (unstyled — the
// caller styles each line).
func logoLines() []string {
	lines := make([]string, 0, logoHeight)
	for row := 0; row < logoHeight; row++ {
		var b strings.Builder
		for i, r := range logoWord {
			if i > 0 {
				b.WriteString(strings.Repeat(" ", logoGapCol))
			}
			glyph := logoFont[r]
			if len(glyph) != logoHeight {
				continue
			}
			for _, c := range glyph[row] {
				if c == '1' {
					b.WriteString("██")
				} else {
					b.WriteString("  ")
				}
			}
		}
		lines = append(lines, b.String())
	}
	return lines
}
