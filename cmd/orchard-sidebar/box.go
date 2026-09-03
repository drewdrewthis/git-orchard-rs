package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The sidebar draws exactly one kind of panel: a titled box the width of the
// pane's interior. The git box under the card list and the right-click row
// menu are both that box, so they are both this function — drawn twice, they
// drifted into two sets of border arithmetic that had to be kept in step by
// hand.

// boxItem is one line of content going INTO a box: what it says, and what
// clicking it puts on the clipboard ("" = nothing to copy). What comes out is
// a viewLine (layout.go) — the same pair plus the row a click selects, which
// for a box is always none.
type boxItem struct {
	text string
	copy string
}

// boxStyle is how a box is drawn. The two panels differ only here: the git box
// is furniture (dim border, dim title, brightening only to acknowledge a copy),
// the row menu is a modal (accent border, so it reads as sitting on top of the
// list rather than in it).
type boxStyle struct {
	border lipgloss.Style
	title  lipgloss.Style
}

var (
	gitBoxStyle  = boxStyle{border: styDim, title: styDim}
	copiedStyle  = boxStyle{border: styDim, title: stySelBody}
	menuBoxStyle = boxStyle{border: stySelHead, title: stySelHead}
)

// boxMinWidth is the narrowest box worth OPENING: below it the borders and
// the two-cell padding leave no room for content at all. boxRender itself
// still draws at any width (clamped, equal-width lines) — the footer's git box
// is part of a fixed-height band and may not vanish; only the row menu, which
// is an overlay that can simply decline to open, consults this.
const boxMinWidth = 12

// boxInner is the content width of a box drawn at width cells: " │ " on the
// left and " │" on the right.
func boxInner(width int) int { return width - 5 }

// boxRender draws the panel: a title border, one line per body entry, a
// closing border. Every line is clamped to width cells (the line()/lineToRow
// invariant — an overshooting line soft-wraps and skews the mouse maps), and
// every body entry is padded to exactly the inner width so the closing │ lands
// in the same column on all of them.
//
// The body is drawn as given: callers that need a fixed height pad it
// themselves, because only they know what an empty row means.
func boxRender(title string, body []string, width int, sty boxStyle) []string {
	inner := boxInner(width)
	head := "─ " + trunc(title, inner) + " "
	hw := ansi.StringWidth(head)
	mid := head + strings.Repeat("─", max(0, width-3-hw))
	if hw > width-3 {
		mid = strings.Repeat("─", max(0, width-3))
	}
	out := []string{trunc(" "+sty.border.Render("╭")+sty.title.Render(mid)+sty.border.Render("╮"), width)}
	pipe := sty.border.Render("│")
	for _, b := range body {
		out = append(out, trunc(" "+pipe+" "+padTo(b, inner)+" "+pipe, width))
	}
	return append(out,
		trunc(" "+sty.border.Render("╰"+strings.Repeat("─", max(0, width-3))+"╯"), width))
}
