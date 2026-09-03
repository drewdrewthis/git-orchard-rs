package main

import (
	"strconv"
	"strings"
	"time"
)

// The pane's three bands. Only the middle one moves; the header and footer are
// fixed furniture, which is what keeps the list from jumping under the cursor.

// workFrames spins the working glyph. Data lanes poll every 2s, far too slow to
// read as motion, so animation gets its own tick that touches nothing but the
// frame counter.
var workFrames = []string{"◐", "◓", "◑", "◒"}

const animEvery = 250 * time.Millisecond

// collapseGlyph points the way the click will move the pane's edge: « pulls
// it closed, » pushes it back open.
const collapseGlyph = "«"

// launchGlyph opens the launch modal — the one thing in the sidebar that
// creates rather than reads.
const launchGlyph = "+"

const expandGlyph = "»"

// badgeMinWidth is the inner width below which the Needs-attention badge is
// dropped. Four cells of badge in front of the four-cell button strip is more
// than a very narrow pane can spare, and the buttons outrank it.
const badgeMinWidth = 18

// subDownGlyph marks a degraded push lane in the title: the subscription
// dropped and the sidebar is polling, which is slower to notice an attach but
// not broken. Small and dim on purpose — it is a fact about freshness, not a
// failure the user has to act on (that is the offline banner).
const subDownGlyph = "↯"

// header is the fixed top band: title, the two buttons, and the banners —
// a broken wrapper environment, and the daemon being unreachable. It also
// reports where the buttons landed, since only this function knows.
func (m *model) header(w int) ([]viewLine, headZones) {
	iw := w - 3
	title := stySelName.Render("orchard")
	if m.subErr != nil {
		title += " " + styDim.Render(subDownGlyph)
	}
	// two buttons, right-aligned: "+" opens the launch modal, "«" collapses the
	// pane. Two spaces between them so a slightly-off click can't hit the wrong
	// one — they do very different things.
	buttons := stySelHead.Render(launchGlyph) + "  " + stySelHead.Render(collapseGlyph)
	// The badge joins the RIGHT-hand strip, ahead of the buttons: line()
	// right-aligns that strip whole, so "«" still lands on the pane's last
	// usable cell and the click zones below stay where they are. Suppressed in
	// a pane too narrow to hold it and the buttons both — the collapse button
	// is the only way back out of a mis-sized pane, so nothing may crowd it.
	right := buttons
	if b := m.attnBadge(); b != "" && iw >= badgeMinWidth {
		right = b + "  " + buttons
	}
	// the update hint joins ahead of the badge — the LEADING segment of the
	// right-hand strip, not the trailing one the button constants below
	// assume, so its zone is computed from the strip's own final width
	// rather than hardcoded. Suppressed first on a narrowing pane: dropped
	// below ~24 inner columns, well before the badge (18) or the buttons
	// (never), so it can never crowd either. The click zone is set only when
	// an upgrade is actually available: a dev build shows its ident here too
	// (dev@<rev>), but that is a label, not a click-to-upgrade target (#789).
	var updateZone clickZone
	if u := m.updateHint(); u != "" && iw >= updateMinWidth {
		right = u + "  " + right
		if m.updateAvailable() {
			updateZone = clickZone{x: max(0, 1+iw-cellWidth(right)), y: 0, w: cellWidth(u), h: 1}
		}
	}
	// the filter takes the title's place rather than a line of its own: an
	// extra header line would shorten the list every time someone typed "/"
	left := title
	if m.filterOn() {
		left = m.filterHead(iw - cellWidth(right) - 1)
	}
	out := []viewLine{{text: " " + line(iw, left, right), row: -1}}
	// line() right-aligns the 4-cell button strip in iw cells after a one-cell
	// indent, so "«" lands at column w-3 and "+" at w-6. Each zone is wider
	// than its glyph: a button you have to pixel-hunt for isn't a button.
	zones := headZones{
		launch:   clickZone{x: max(0, w-6), y: 0, w: 2, h: 1},
		collapse: clickZone{x: max(0, w-3), y: 0, w: 3, h: 1},
		update:   updateZone,
	}
	// a half-configured wrapper is otherwise silent: switches are refused and
	// collapse does nothing, with nothing on screen saying why
	if p := env.problem(); p != "" {
		out = append(out, viewLine{text: styErr.Render(trunc("⚠ "+p, w)), row: -1})
	}
	// same judgment as the row wipe: a transient fast-lane error holds the
	// rows silently, so it must not also claim the daemon is offline
	if m.daemonDown() {
		out = append(out,
			viewLine{text: styErr.Render(trunc("⚠ DAEMON OFFLINE — hook states live", w)), row: -1},
			viewLine{text: styDim.Render(trunc(m.err.Error(), w)), row: -1})
	}
	return append(out, viewLine{text: "", row: -1}), zones // breathing room under the header
}

// collapsedLines is the whole pane at collapsed width: the expand button, then
// one state glyph per session so the strip still says what the sidebar is
// watching. The selected session's glyph keeps its accent, which is all the
// "you are here" a 3-column strip has room for.
func (m *model) collapsedLines(w, h int) ([]viewLine, headZones) {
	out := []viewLine{{text: " " + stySelHead.Render(expandGlyph), row: -1}}
	// the count, under the button: three cells cannot say what needs you, but
	// they can say how much does, which is the number that decides whether the
	// strip is worth reopening
	if n := m.attnCount(); n > 0 {
		out = append(out, viewLine{
			text: " " + styAttn.Render(trunc(strconv.Itoa(n), max(1, w-1))), row: -1})
	}
	// bare — three cells is not room for a version number, only the fact
	// that there is one; the version lives in the full-width header's hint
	if m.updateAvailable() {
		out = append(out, viewLine{text: " " + styDim.Render(updateGlyph), row: -1})
	}
	out = append(out, viewLine{text: "", row: -1})
	for i, r := range m.rows {
		if h > 0 && len(out) >= h {
			break
		}
		g, sty := marker(r, m.frame)
		if i == m.cursor {
			sty = stySelName // "you are here" outranks the bucket in a 3-cell strip
		}
		out = append(out, viewLine{text: " " + sty.Render(g), row: -1})
	}
	// the entire strip is the expand button: there is nothing else in it to
	// click, and a 3-cell target is small enough already. No room for a +.
	return out, headZones{collapse: clickZone{x: 0, y: 0, w: max(1, w), h: max(1, h)}}
}

// hintLine is the footer's last line: the keys that work right now, in the
// order a user reaches for them. It takes bell rather than reading m
// directly so a test can render either state without a model.
//
// j/k is deliberately absent — arrow keys reach the same gesture and need no
// hint of their own. The rest that still don't fit are each discoverable
// elsewhere: "+ new" is a button drawn two lines above it, "M-s" is the
// wrapper's own binding and lives in outer.conf, and "q" quits the sidebar,
// which inside the wrapper leaves a dead pane and is not a thing to
// advertise. Anything added here has to displace one of the four that remain.
func hintLine(bell bool) string {
	state := "off"
	if bell {
		state = "on"
	}
	return "/ filter · M-1-9 jump · b bell·" + state
}

// hintWidth is that budget: the inner width of a pane at the wrapper's default
// 40 columns, which is the width the hint has to fit in unclipped. Both bell
// states fit with room to spare (33-34 cells against a 37-cell budget).
const hintWidth = defaultWidth - 3

// footer is the fixed bottom band: the selected session's git facts laid out
// to be taken (clicking any line copies its payload), then the key hints.
func (m *model) footer(w int, compact bool) []viewLine {
	iw := w - 3
	// a full-width rule, edge to edge: the list scrolls under it, so the eye
	// needs a hard line telling it where the scrolling stops and the fixed
	// facts start
	out := []viewLine{{text: styDim.Render(strings.Repeat("─", max(1, w))), row: -1}}
	// the box is drawn whether or not the selection has anything to put in it
	// (and even with no selection at all): its height is part of the footer's
	// height, and the footer's height is what keeps the list from jumping
	if !compact {
		var items []boxItem
		if r, ok := m.railRow(); ok {
			items = gitBoxItems(r)
		}
		// box clicks copy, never select: gitBoxRender says so on every line
		out = append(out, gitBoxRender(items, iw, time.Now().Before(m.copiedUntil))...)
	}
	if !m.stateDirOK {
		out = append(out, viewLine{
			text: styDim.Render(trunc("no state dir — install claude-session-state", w)), row: -1})
	}
	// a keyboard-chord refusal/notice (M-Enter, M-w) takes the hint line's place
	// while it is fresh, so the height stays fixed and the list never jumps
	if s := m.statusText(); s != "" {
		return append(out, viewLine{text: " " + styErr.Render(trunc(s, iw)), row: -1})
	}
	return append(out, viewLine{
		text: " " + styDim.Render(trunc(hintLine(m.bell), iw)), row: -1})
}

// View hands back the frame Update composed. It is deliberately a pure
// accessor: bubbletea calls it on its own schedule, and a View that laid out
// the pane would move the viewport and republish the mouse maps every time it
// happened to be called.
func (m *model) View() string { return m.pane.text }
