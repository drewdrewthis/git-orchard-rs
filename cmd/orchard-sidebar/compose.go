package main

import "strings"

// Composing the pane: the three bands are laid out, the list is scrolled, the
// menu is stamped on top, and the result — text plus everything a click needs
// to interpret it — is stored as one frame.
//
// This runs in Update, never in View. Composition is not a read: it moves the
// viewport, publishes the mouse maps and places the buttons, and a View that
// did all that would mean the pane's state depended on how often bubbletea
// happened to repaint it. View hands back the frame Update already resolved.

// paneFrame is one composed pane. Nothing outside compose writes one.
type paneFrame struct {
	text         string
	lineToRow    []int    // rendered line -> row index (-1 = not a row)
	lineToCopy   []string // rendered line -> click-to-copy payload ("" = none)
	launchZone   clickZone
	collapseZone clickZone
	updateZone   clickZone // the header's update glyph: a click opens the overlay
	menuBox      clickZone // the whole menu box: a click outside dismisses
	menuRows     []clickZone
}

// rowAtLine is the click-to-row lookup, bounds-checked: a mouse event can
// arrive for a line the last frame never drew (a resize in flight), and a row
// index of -1 means the line is chrome.
func (f paneFrame) rowAtLine(y int) (int, bool) {
	if y < 0 || y >= len(f.lineToRow) {
		return 0, false
	}
	i := f.lineToRow[y]
	return i, i >= 0
}

// copyAtLine is the same for the git box's click-to-copy payloads.
func (f paneFrame) copyAtLine(y int) (string, bool) {
	if y < 0 || y >= len(f.lineToCopy) {
		return "", false
	}
	return f.lineToCopy[y], f.lineToCopy[y] != ""
}

// paneWidth is the width to compose at. Before the first WindowSizeMsg there
// is no honest answer, so it draws at a plausible one rather than at zero.
func (m *model) paneWidth() int {
	if m.width <= 0 {
		return defaultPaneWidth
	}
	return m.width
}

// defaultPaneWidth is what the pane draws at before tmux has told it anything.
const defaultPaneWidth = 42

// headZones is where the header's two buttons landed. Only the band that
// drew them knows, and only the composed frame may publish them — a zone
// published by a band that was then clipped away would answer to clicks that
// can no longer reach it.
type headZones struct{ launch, collapse, update clickZone }

// compose resolves the frame View will paint.
func (m *model) compose() {
	w, h := m.paneWidth(), m.height
	if isCollapsedWidth(w) {
		list, zones := m.collapsedLines(w, h)
		m.render(nil, list, nil, w, h, zones)
		return
	}
	compact := w < minWidth
	head, zones := m.header(w)
	m.render(head, m.cards(w, compact), m.footer(w, compact), w, h, zones)
}

// render composes the three bands into the pane and publishes the mouse maps
// for the composed result — the list is scrolled and both other bands are
// pinned, so a screen line's meaning is nothing a caller could derive from
// the band slices alone. An unknown height (before the first WindowSizeMsg)
// draws every line rather than guessing a viewport to clip to.
func (m *model) render(head, list, foot []viewLine, width, height int, zones headZones) {
	if height <= 0 {
		height = len(head) + len(list) + len(foot)
	}
	lay := layoutPane(height, len(head), len(foot))
	out := make([]viewLine, 0, height)
	out = append(out, clampBand(head, lay.headerH, false)...)
	// The scroll position is the user's, and neither a data refresh nor a
	// selection you can already see is a reason to take it away.
	//
	// The viewport moves for exactly one reason: the selection MOVED and is
	// off screen. That covers j/k walking past an edge and an attach that
	// landed on a card the user isn't looking at — and it deliberately
	// excludes a mouse click, which can only land on a line that is already
	// drawn. Snapping on any selection change (what this did before) meant
	// every click re-derived the offset and threw away wherever the user had
	// scrolled to.
	//
	// "Moved" is a fact the events record (snapSel), not one this function
	// infers by comparing against the last paint: rows re-sort on every 2s
	// refresh, so the cursor INDEX moves under the user constantly, and every
	// comparison built on it read a refresh as a user gesture.
	//
	// Everything else re-derives the offset from the card that was on top,
	// so rows appearing, disappearing or re-sorting slide under a viewport
	// that stays where it was pointed.
	off := clampOffset(m.anchoredOffset(list), len(list), lay.listH)
	if m.snapSel && !rowOnScreen(list, m.cursor, off, lay.listH) {
		off = scrollOffset(list, lay.listH, m.cursor, off)
	}
	m.scroll, m.snapSel = off, false
	// re-anchor to whatever card the viewport now starts on, and to where in
	// that card it starts: the top line can be a section header or the middle
	// of a card, and both have to come back to the same place next paint
	m.anchorSess, m.anchorDelta = "", 0
	for i := off; i < len(list); i++ {
		r, ok := m.rowAt(list[i].row)
		if !ok {
			continue
		}
		m.anchorSess = r.session
		m.anchorDelta = off - firstLineOfRow(list, list[i].row)
		break
	}
	for i := 0; i < lay.listH; i++ {
		if off+i < len(list) {
			out = append(out, list[off+i])
			continue
		}
		out = append(out, viewLine{row: -1}) // pad: the footer stays at the bottom
	}
	out = append(out, clampBand(foot, lay.footerH, true)...)
	// both overlays go on last, over the finished pane, and the maps built
	// below must be the COVERED result — a click on either must not also
	// reach the card or footer line underneath it. Update first, menu
	// second: a right-click can open the row menu without a keypress ever
	// having dismissed an update overlay left open, and the more actively
	// engaged-with surface should win where the two could ever overlap.
	out = m.overlayUpdate(out, width, height, lay.headerH)
	out, box, rows := m.overlayMenu(out, width, height)

	lineMap := make([]int, len(out))
	copyMap := make([]string, len(out))
	texts := make([]string, len(out))
	for i, l := range out {
		texts[i], lineMap[i], copyMap[i] = l.text, l.row, l.copy
	}
	m.pane = paneFrame{
		// joined, not one trailing newline per line: an extra final line would
		// scroll the whole pane up by one and unpin the footer
		text:         strings.Join(texts, "\n"),
		lineToRow:    lineMap,
		lineToCopy:   copyMap,
		launchZone:   zones.launch,
		collapseZone: zones.collapse,
		updateZone:   zones.update,
		menuBox:      box,
		menuRows:     rows,
	}
}
