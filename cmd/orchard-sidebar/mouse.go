package main

import (
	tea "github.com/charmbracelet/bubbletea"
)

// mouseCmd is the one mouse gesture that produces a command rather than a
// state change: a click on a git-box line copies its payload.
func (m *model) mouseCmd(msg tea.MouseMsg) tea.Cmd {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft ||
		m.menuOpen() || m.pane.launchZone.hit(msg.X, msg.Y) ||
		m.pane.collapseZone.hit(msg.X, msg.Y) || m.pane.updateZone.hit(msg.X, msg.Y) {
		return nil
	}
	if payload, ok := m.pane.copyAtLine(msg.Y); ok {
		return copyCmd(payload)
	}
	return nil
}

// mouse routes a mouse event. Clicking a card is the same gesture as walking
// onto it with j/k: selection and the attached session are one thing, not two.
// Git-box lines copy instead of selecting — they map to no row, and mouseCmd
// has already claimed the click. The wheel scrolls the list and nothing else —
// no selection, so no attach. tmux forwards wheel events into this pane like
// any other mouse report once mouse mode is on.
func (m *model) mouse(msg tea.MouseMsg) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		// the menu is anchored to a card, and the cards are about to move out
		// from under it
		m.closeMenu()
		m.scrollBy(-wheelStep)
		return
	case tea.MouseButtonWheelDown:
		m.closeMenu()
		m.scrollBy(wheelStep)
		return
	}
	// a left-button motion or release is only ever the middle or end of a
	// drag the press below started (drag.go); the press itself falls through.
	if msg.Button == tea.MouseButtonLeft && msg.Action != tea.MouseActionPress {
		m.dragMove(msg)
		return
	}
	if msg.Action != tea.MouseActionPress {
		return
	}
	// right-click is the row menu (menu.go) — rename or close the session
	// under the pointer. It never selects, so it never attaches.
	if msg.Button == tea.MouseButtonRight {
		m.rightClick(msg.X, msg.Y)
		return
	}
	if msg.Button != tea.MouseButtonLeft {
		return
	}
	// an open menu owns every left click: on an item it acts, anywhere else it
	// dismisses, and either way the click stops there rather than reaching the
	// card the box is covering
	if m.menuOpen() {
		m.menuClick(msg.X, msg.Y)
		return
	}
	// the collapse button first: it owns a small rectangle of the header (or
	// the whole strip when collapsed), and neither maps to a row or a payload
	if m.pane.collapseZone.hit(msg.X, msg.Y) {
		m.toggleCollapse()
		return
	}
	if m.pane.updateZone.hit(msg.X, msg.Y) {
		m.openUpdateOverlay()
		return
	}
	if m.pane.launchZone.hit(msg.X, msg.Y) {
		m.openLaunch()
		return
	}
	if _, ok := m.pane.copyAtLine(msg.Y); ok {
		return // mouseCmd is copying it; a copy never also selects
	}
	if ri, ok := m.pane.rowAtLine(msg.Y); ok {
		// a press attaches the card (the click that always worked) and arms a
		// drag: capture the session BEFORE selectRow re-sorts the list, then a
		// release with motion pins/unpins it. See drag.go.
		r, _ := m.rowAt(ri)
		m.selectRow(ri, true)
		m.dragStart(r.session, msg.Y)
	}
}

// wheelStep is how many rendered lines one wheel notch moves. Cards are a few
// lines tall, so one line per notch would feel stuck.
const wheelStep = 3
