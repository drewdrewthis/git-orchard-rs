package main

import "fmt"

// The scrolling middle band: one section header per bucket, then a card per
// session. Split from view.go because this is the only band whose height is
// data-driven — the header and footer are fixed furniture, and keeping the two
// apart is what makes "only the middle band moves" checkable.

// A card is always exactly this tall, whatever the session has to say: a
// missing mission or directory pads with a blank line instead of shortening
// the card. Uniform cards are what make the list scannable and scrolling
// predictable — line count is a function of the row count, not of how much
// metadata happened to land. Section headers sit outside the budget.
const cardRows = 4

// compactCardRows is the same card in a pane too narrow for the detail lines.
const compactCardRows = 2

// selBar is the selection rail: a half-width block, so the bar reads as a
// margin rule beside the card rather than a second column of content.
const selBar = "▌"

// cards renders the scrolling band: one section header per bucket, then a card
// per session the filter keeps. It walks visibleRows rather than m.rows, but
// every line it emits still carries the row's index IN THE MODEL — a click map
// built off screen positions would select a different session with a filter on
// than without one.
func (m *model) cards(w int, compact bool) []viewLine {
	var out []viewLine

	// one-cell gutter for the attached-session indicator bar, two cells of
	// breathing room on the right so nothing butts against the pane edge
	iw := w - 3

	vis := m.visibleRows()
	if len(vis) == 0 {
		if m.filterQuery() != "" {
			return m.noMatchLines(iw)
		}
		return nil
	}

	// selection and the attached session are the same thing (see selectRow), so
	// the gutter bar marks the cursor row — it moves the instant you press j/k
	// rather than waiting for the next daemon poll to report the new attach.
	sel := m.railIndex(vis)
	cur, hasCur := m.rowAt(sel)
	curBucket := bucketRunning
	if hasCur {
		curBucket = rowBucket(cur)
	}

	first := true
	prev := bucketRunning
	for n, i := range vis {
		r := m.rows[i]
		if b := rowBucket(r); first || b != prev {
			if !first {
				out = append(out, viewLine{text: "", row: -1}) // gap before the next header
			}
			// the selected card's own section title lights up with it
			headSty := styDim
			if hasCur && curBucket == b {
				headSty = stySelHead
			}
			// right-aligned, opposite the cards' left border rail
			out = append(out, viewLine{text: " " + headSty.Render(line(iw, "", groupLabel(b))), row: -1})
			prev, first = b, false
		}
		// only the attached session gets a border — a half-width neon bar down
		// every line of the card. Other cards render a plain space in that
		// column so the layout doesn't shift on selection.
		pfx := " "
		if i == sel {
			pfx = stySelBar.Render(selBar)
		}
		for li, s := range cardBody(r, m.frame, iw, i == sel, compact) {
			out = append(out, viewLine{text: railCell(pfx, n, li) + s, row: i})
		}
	}
	return out
}

// railCell is what the one-cell gutter holds on a given line of a card: the
// selection rail wherever there is one, otherwise the M-1..M-9 ordinal on the
// card's first line. The rail wins on the selected card — you are already
// there, so the chord that would take you there is the one marker worth
// giving up, and both glyphs are one cell so no card ever shifts.
func railCell(pfx string, n, line int) string {
	if pfx != " " || line != 0 {
		return pfx
	}
	if o := jumpOrdinal(n); o != "" {
		return styDim.Render(o)
	}
	return pfx
}

// cardBody is one card's lines, and it is EXACTLY cardRows of them (or
// compactCardRows in a narrow pane) by construction: it fills a fixed-size
// slice, so a body that forgot a line pads instead of shortening the card and
// sliding every card below it up by one.
func cardBody(r row, frame, iw int, selected, compact bool) []string {
	n := cardRows
	if compact {
		n = compactCardRows
	}
	lines := make([]string, n)

	ageSty, bodySty, promptSty := styDim, styDim, styPrompt
	name := r.session
	if selected {
		ageSty, bodySty, promptSty = stySelAge, stySelBody, stySelPrompt
		name = stySelName.Render(r.session)
	}
	g, gSty := marker(r, frame)
	left := fmt.Sprintf(" %s %s", gSty.Render(g), name)
	if !r.hooked && r.state != "shell" {
		left += bodySty.Render("?")
	}
	if r.model != "" {
		left += " " + bodySty.Render("["+r.model+"]")
	}
	lines[0] = line(iw, left, ageSty.Render(age(r.lastAct)))
	if compact {
		// too narrow for detail lines to say anything useful — name only,
		// then the blank line that closes every card
		return lines
	}
	if r.mission != "" {
		// truncate the mission first so the closing quote always survives
		lines[1] = promptSty.Render(trunc("  “"+r.mission, iw-1) + "”")
	}
	// dir on the left, whatever the session is *for* on the right: the branch
	// and the full issue/PR line live in the footer's git box for whichever
	// card is selected, so the card carries the short form
	dir := ""
	if d := dirLabel(r); d != "" {
		dir = bodySty.Render("  📁 " + d)
	}
	lines[2] = line(iw, dir, bodySty.Render(cardTag(r)))
	// lines[3] stays empty: one blank line closes every card
	return lines
}
