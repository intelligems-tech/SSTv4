package invocations

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
)

// ansiLines turns text coloured for a terminal into styled rows, one per
// line, starting from base. SGR sequences (colours and attributes) become
// tcell styles; every other escape sequence is dropped.
func ansiLines(text string, base tcell.Style) []styledLine {
	lines := []styledLine{{}}
	style := base
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			lines[len(lines)-1] = append(lines[len(lines)-1], span{run.String(), style})
			run.Reset()
		}
	}
	p := ansi.NewParser(32, 1)
	var state byte
	for len(text) > 0 {
		seq, width, n, next := ansi.DecodeSequence(text, state, p)
		state, text = next, text[n:]
		switch {
		case seq == "\n":
			flush()
			lines = append(lines, styledLine{})
		case width > 0 || seq == "\t":
			run.WriteString(seq)
		case ansi.HasCsiPrefix(seq) && isSGR(ansi.Cmd(p.Cmd)):
			flush()
			params := make([]int, p.ParamsLen)
			for i := range params {
				params[i] = max(0, ansi.Param(p.Params[i]).Param())
			}
			style = applySGR(style, base, params)
		}
	}
	flush()
	return lines
}

func isSGR(cmd ansi.Cmd) bool {
	return cmd.Command() == 'm' && cmd.Marker() == 0 && cmd.Intermediate() == 0
}

// applySGR applies one Select Graphic Rendition sequence's parameters.
// A reset returns to base rather than the terminal default, so uncoloured
// text keeps the pane's own style.
func applySGR(style tcell.Style, base tcell.Style, params []int) tcell.Style {
	if len(params) == 0 {
		return base
	}
	baseFg, baseBg, _ := base.Decompose()
	for i := 0; i < len(params); i++ {
		switch code := params[i]; {
		case code == 0:
			style = base
		case code == 1:
			style = style.Bold(true)
		case code == 2:
			style = style.Dim(true)
		case code == 3:
			style = style.Italic(true)
		case code == 4:
			style = style.Underline(true)
		case code == 7:
			style = style.Reverse(true)
		case code == 9:
			style = style.StrikeThrough(true)
		case code == 22:
			style = style.Bold(false).Dim(false)
		case code == 23:
			style = style.Italic(false)
		case code == 24:
			style = style.Underline(false)
		case code == 27:
			style = style.Reverse(false)
		case code == 29:
			style = style.StrikeThrough(false)
		case code >= 30 && code <= 37:
			style = style.Foreground(tcell.PaletteColor(code - 30))
		case code >= 90 && code <= 97:
			style = style.Foreground(tcell.PaletteColor(code - 90 + 8))
		case code == 39:
			style = style.Foreground(baseFg)
		case code >= 40 && code <= 47:
			style = style.Background(tcell.PaletteColor(code - 40))
		case code >= 100 && code <= 107:
			style = style.Background(tcell.PaletteColor(code - 100 + 8))
		case code == 49:
			style = style.Background(baseBg)
		case code == 38 || code == 48:
			color, used := extendedColor(params[i+1:])
			i += used
			if color == tcell.ColorDefault {
				continue
			}
			if code == 38 {
				style = style.Foreground(color)
			} else {
				style = style.Background(color)
			}
		}
	}
	return style
}

// extendedColor reads the 5;n (256 colour) or 2;r;g;b (true colour) tail of
// a 38 or 48 parameter, returning the colour and how many parameters it used.
func extendedColor(params []int) (tcell.Color, int) {
	switch {
	case len(params) >= 2 && params[0] == 5:
		return tcell.PaletteColor(params[1]), 2
	case len(params) >= 4 && params[0] == 2:
		return tcell.NewRGBColor(int32(params[1]), int32(params[2]), int32(params[3])), 4
	}
	return tcell.ColorDefault, len(params)
}
