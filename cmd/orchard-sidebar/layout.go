package main

// clickZone is a rectangle of the rendered pane that answers to the mouse on
// its own, outside the line-indexed lineToRow/lineToCopy maps: those map a
// whole row, and the collapse button is one small target on a line whose rest
// does nothing. The zero value is deliberately unhittable (w/h 0), so a click
// arriving before the first paint can't toggle anything.
type clickZone struct{ x, y, w, h int }

func (z clickZone) hit(x, y int) bool {
	return z.w > 0 && z.h > 0 && x >= z.x && x < z.x+z.w && y >= z.y && y < z.y+z.h
}

// View layout — three bands, of which only the middle one moves:
//
//	header   — FIXED to the top: the title and the collapse button («), the
//	           daemon-offline banner when the daemon is unreachable, then a
//	           blank line
//	list     — the session cards, taking every row header and footer leave
//	           over, scrolled to keep the selected card in view. Growing the
//	           list can never push the other two bands off the pane, at any
//	           terminal height.
//	           sections — one dim cap-first header per contiguous state group
//	                      (rows are sorted by state, so groups are runs),
//	                      padded one cell in and followed by a blank line
//	           cards    — every session, everything visible (nothing
//	                      collapsed), all dim except the session name. The
//	                      selected card carries a thick neon purple left
//	                      border down its full height, including a padding
//	                      line above and below its content; other cards keep a
//	                      blank column there so nothing shifts on selection.
//	                      No gap lines between cards — the padding lines make
//	                      each group read continuous. The border is the only
//	                      selection glyph (there is no cursor caret, because
//	                      selecting a card attaches its tmux session: cursor
//	                      and current session are one thing).
//	                       ● name [model]     right-aligned age
//	                         (blank)
//	                         “first prompt” (italic)
//	                         🌿 branch ↑a ↓b
//	                         📁 directory
//	                         issue#N | pr#M (one status word — see prStatus)
//	footer   — FIXED to the bottom: the git box for the selected session
//	           (every line click-to-copy), then the key hints
//
// Below minWidth the card sub-lines are dropped; below collapsedMax the whole
// pane is the collapsed strip (see collapsedLines).
//
// minWidth is the narrowest pane the full card layout stays readable in.
// Below it the sub-lines (prompt, branch, dir, issue/pr) are dropped rather
// than shredded into one-word slivers — see compact mode in View().
const minWidth = 34

// collapsedWidth is what the pane shrinks to when collapsed: wide enough for
// the » button and a one-cell state rail, narrow enough to be a margin.
// defaultWidth is what it reopens to when no shared width is known — the same
// 40 columns orchard-shell splits the wrapper at.
const collapsedWidth = 3

const defaultWidth = 40

// collapsedMax is the width at or below which the pane can only be the strip.
// The sidebar is told it was collapsed by the size it is given, not by a flag:
// the collapse can equally come from outer.conf's M-s binding or its resize
// hooks, which never tell the sidebar anything. A couple of cells of slack
// over collapsedWidth so a tmux minimum or a border cell can't read as
// "expanded but unusably narrow".
const collapsedMax = 6

func isCollapsedWidth(w int) bool { return w > 0 && w <= collapsedMax }

// viewLine is one rendered pane row together with what a click on it does:
// the row index it selects (-1 = none) and the payload it copies ("" = none).
// Carrying both with the text is what keeps the mouse maps from being
// reconstructed by index arithmetic once the bands are composed and the list
// is scrolled.
type viewLine struct {
	text string
	row  int
	copy string
	// sep marks the pinned-block separator line: it maps to no row (row == -1),
	// but compose records its final screen position so a drag release can be
	// hit-tested above (into the block) or below (out of it).
	sep bool
}

// paneLayout is the vertical division of the pane. headerY is always 0;
// footerY+footerH is always the pane height, so both bands stay pinned.
type paneLayout struct {
	headerH      int
	listY, listH int
	footerY      int
	footerH      int
}

// minListRows is how much of the session list survives a short pane: the
// footer is the band that yields, since the header carries the only way back
// out of a mis-sized pane (the collapse button).
const minListRows = 3

// layoutPane splits height rows between a header pinned to the top, a footer
// pinned to the bottom, and the list filling everything between. Pure: the
// same height always yields the same rects, which is the whole point —
// header and footer must not move when the list grows, at any height.
func layoutPane(height, headerH, footerH int) paneLayout {
	if height <= 0 {
		return paneLayout{}
	}
	if headerH > height {
		headerH = height
	}
	if headerH < 0 {
		headerH = 0
	}
	rest := height - headerH
	if footerH > rest-minListRows {
		footerH = rest - minListRows
	}
	if footerH < 0 {
		footerH = 0
	}
	listH := rest - footerH
	return paneLayout{
		headerH: headerH,
		listY:   headerH,
		listH:   listH,
		footerY: headerH + listH,
		footerH: footerH,
	}
}

// rowSpan is the first and last list line a row's card occupies, or (-1, -1)
// when the row draws nothing (it was filtered out, or the index is stale).
func rowSpan(lines []viewLine, row int) (lo, hi int) {
	lo, hi = -1, -1
	for i, l := range lines {
		if l.row != row {
			continue
		}
		if lo < 0 {
			lo = i
		}
		hi = i
	}
	return lo, hi
}

// rowOnScreen reports whether ANY line of a row's card is inside the viewport
// [off, off+listH).
//
// This is the gate on the snap-to-selection rule (see render), and "any line"
// rather than "the whole card" is load-bearing: a click can only ever land on
// a line that is already drawn, so a click's new selection is on screen by
// construction and the snap never fires for it. That is the whole fix for
// "clicking something resets the position of everything" — the viewport moves
// for a selection you cannot see, and for nothing else.
func rowOnScreen(lines []viewLine, row, off, listH int) bool {
	if listH <= 0 {
		return false
	}
	lo, hi := rowSpan(lines, row)
	return lo >= 0 && hi >= off && lo < off+listH
}

// scrollOffset is the first list line to draw so the selected card comes back
// on screen — the MINIMUM move from cur that gets it there, so walking off an
// edge with j/k scrolls by the card and never re-derives a fresh offset from
// the top of the list. (It used to ignore cur entirely: pressing k onto a card
// just above the viewport jumped the list to line 0.)
// A card taller than the viewport shows its top (the name line), not its tail.
func scrollOffset(lines []viewLine, listH, cursor, cur int) int {
	if listH <= 0 || len(lines) <= listH {
		return 0
	}
	lo, hi := rowSpan(lines, cursor)
	if lo < 0 {
		return cur
	}
	off := cur
	if hi >= off+listH {
		off = hi - listH + 1 // walked off the bottom: bring its last line up
	}
	if lo < off {
		off = lo // walked off the top: bring its first line down
	}
	return clampOffset(off, len(lines), listH)
}

// clampOffset holds a scroll offset inside the list. A list that shrank shows
// its tail rather than jumping to its top.
func clampOffset(off, n, listH int) int {
	if maxOff := n - listH; off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	return off
}

// clampBand cuts a band to n lines. The header keeps its first lines (the
// title, and with it the collapse button, is the line that must never go);
// the footer keeps its last (the key hints sit at the very bottom, and a git
// box degrades from its top border rather than losing them).
func clampBand(lines []viewLine, n int, keepTail bool) []viewLine {
	if n <= 0 {
		return nil
	}
	if len(lines) <= n {
		return lines
	}
	if keepTail {
		return lines[len(lines)-n:]
	}
	return lines[:n]
}

// firstLineOfRow is the list line a row's card starts on, which is where the
// scroll anchor measures from. A row that draws nothing measures from the top
// of the list — the anchor is about to be re-derived anyway.
func firstLineOfRow(lines []viewLine, row int) int {
	lo, _ := rowSpan(lines, row)
	return max(lo, 0)
}
