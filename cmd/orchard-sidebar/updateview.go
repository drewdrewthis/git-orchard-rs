package main

import "fmt"

// Drawing the update indicator: the header glyph, the collapsed-strip glyph
// (view.go draws both, reading updateGlyph directly), and the one-line
// overlay a click on the header glyph opens. The overlay is an OVERLAY, not
// a band, on the same footing as the row menu (menuview.go): the pane is
// composed first (compose.go) and this is stamped over the finished result.

// updateGlyph is the header hint and the collapsed-strip line alike: an
// up arrow, since ● (the badge) and b (the bell) already own dot and letter.
const updateGlyph = "⇡"

// updateMinWidth is the inner width below which the hint is dropped — same
// judgment as badgeMinWidth, just for a strip that also carries a version
// number rather than only a count.
const updateMinWidth = 24

// updateHint is the header's glyph plus version, empty when there is
// nothing to show. Dim, like the badge and the buttons it sits beside — the
// header is not the place an update becomes urgent, only visible.
func (m *model) updateHint() string {
	if !m.updateAvailable() {
		return ""
	}
	return styDim.Render(updateGlyph + "v" + m.updateCheck.Latest)
}

// overlayUpdate splices the one-line detail over the composed pane, just
// under the header — the glyph that opens it lives there, so the detail
// appears where the click was. Its line maps to no row and carries no copy
// payload (viewLine's zero value), so a click on it can never fall through
// to whatever card it covers; nothing more is needed to make that true,
// since dismissal here is keypress-only (selection.go), never a click.
func (m *model) overlayUpdate(out []viewLine, w, height, headerH int) []viewLine {
	lines := m.updateLines(w)
	if len(lines) == 0 || len(lines) > height {
		return out // closed, too narrow, or a pane too short
	}
	y0 := headerH
	if y0+len(lines) > height {
		y0 = max(0, height-len(lines))
	}
	for i, l := range lines {
		if y0+i >= len(out) {
			break
		}
		out[y0+i] = viewLine{text: l, row: -1}
	}
	return out
}

// updateLines draws the box at the pane's interior width, in the row menu's
// own idiom (boxRender, menuBoxStyle) so an overlay reads as an overlay
// whichever one is open.
func (m *model) updateLines(w int) []string {
	bw := w - 3
	if !m.updateOpen || bw < boxMinWidth {
		return nil
	}
	return boxRender("update", []string{m.updateBody(boxInner(bw))}, bw, menuBoxStyle)
}

// updateBody is the overlay's one line: what changed, and the command that
// fixes it. The sidebar cannot run the upgrade itself (RULES T1) — only say
// to.
func (m *model) updateBody(inner int) string {
	return trunc(fmt.Sprintf("update available v%s → v%s — run: orchard upgrade",
		version, m.updateCheck.Latest), inner)
}
