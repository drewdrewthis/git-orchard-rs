package main

// Drawing the row menu. It is an overlay, not a band: the pane is composed
// first (compose.go) and the box is stamped over the finished result, so
// nothing else in the layout has to know it exists.

// overlayMenu splices the box over the composed pane, hanging under the card
// that was clicked and pushed up when it would fall off the bottom — a menu
// missing its last item is worse than one that is not quite where you clicked.
// Its lines map to no row and carry no copy payload, so a click on the menu
// can never fall through to the card underneath it.
//
// The zones it returns are where the menu LANDED, which only this function
// knows; they go into the composed frame beside the mouse maps.
func (m *model) overlayMenu(out []viewLine, w, height int) (_ []viewLine, box clickZone, rows []clickZone) {
	lines := m.menuLines(w)
	if len(lines) == 0 || len(lines) > height {
		return out, clickZone{}, nil // closed, too narrow, or a pane too short
	}
	y0 := min(m.menu.anchor+1, height-len(lines))
	if y0 < 0 {
		y0 = 0
	}
	box = clickZone{x: 0, y: y0, w: w, h: len(lines)}
	for i, l := range lines {
		if y0+i >= len(out) {
			break
		}
		out[y0+i] = viewLine{text: l, row: -1}
	}
	// only the body rows are items; the borders are decoration
	if m.menu.mode == menuActions {
		for i := range menuItems {
			rows = append(rows, clickZone{x: 0, y: y0 + 1 + i, w: max(1, w-3), h: 1})
		}
	}
	return out, box, rows
}

// menuLines draws the box at the pane's interior width, in the git box's own
// idiom (boxRender) so the two panels read as the same furniture.
func (m *model) menuLines(w int) []string {
	bw := w - 3
	if !m.menuOpen() || bw < boxMinWidth {
		return nil
	}
	return boxRender(m.menuTitle(), m.menuBody(boxInner(bw)), bw, menuBoxStyle)
}

func (m *model) menuTitle() string {
	switch m.menu.mode {
	case menuRename:
		return "rename"
	case menuConfirm:
		return "close"
	}
	return m.menu.sess
}

// menuBody is the box's content rows. boxRender pads each one to the inner
// width, so these only have to say what they say.
func (m *model) menuBody(inner int) []string {
	var out []string
	switch m.menu.mode {
	case menuRename:
		out = append(out,
			m.menu.input.view(max(1, inner)),
			styDim.Render("⏎ save · esc cancel"))
	case menuConfirm:
		out = append(out, styAttn.Render(trunc("Close "+m.menu.sess+"? y/N", inner)))
	default:
		for i, it := range menuItems {
			mark, sty := "  ", styDim
			if i == m.menu.item {
				mark, sty = "▸ ", stySelName
			}
			out = append(out, sty.Render(mark+it))
		}
	}
	if m.menu.notice != "" {
		out = append(out, styErr.Render(trunc(m.menu.notice, inner)))
	}
	return out
}
