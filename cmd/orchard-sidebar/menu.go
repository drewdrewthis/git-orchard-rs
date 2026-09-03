package main

import (
	tea "github.com/charmbracelet/bubbletea"
)

// The row menu: right-click a card for the two things you do TO a session
// rather than with it — rename it, or close it. Everything else in this
// sidebar reads; these two write, so they sit behind a gesture nothing else
// uses, and the destructive one sits behind a confirm as well.
//
// It is an IN-APP overlay, not tmux's display-menu or display-popup, per the
// modal rule in docs/outer-shell.md: it acts on a row and has to be
// capture-pane-testable. A popup composites into the attached client's stream
// and into no pane's grid, so capture-pane cannot see it — and it would live
// on the OUTER server while every session it acts on lives on the inner one.
//
// The menu holds its row by SESSION NAME, never by index: rows re-sort under
// an open menu on every 2s refresh, and an index would quietly come to point at
// a different session. This menu kills things.

type menuMode int

const (
	menuClosed    menuMode = iota
	menuActions            // Rename / Close
	menuRename             // text input, prefilled with the current name
	menuConfirm            // Close <name>? y/N
	menuBreakPane          // text input, name for the pane's new session
)

// The actions in draw order; the index is what rowMenu.item holds and what a
// click on the nth body row selects. The pin item's LABEL flips with the row's
// pinned state, so the list is built per-open (menuActionLabels) rather than
// held as a static slice.
// itemBreakPane is LAST so Rename/Close/Pin keep indices 0/1/2 whether or not
// the break item is present — several callers address those three by index.
// The break item is appended to menuActionLabels only when canBreakPane holds,
// which puts it exactly at index 3 = itemBreakPane. See menuActionLabels.
const (
	itemRename = iota
	itemClose
	itemPin
	itemBreakPane
)

// menuActionLabels is the current menu's items. Pin/Unpin depends on whether
// the session the menu is acting on is pinned right now.
func (m *model) menuActionLabels() []string {
	pin := "Pin"
	if m.isPinned(m.menu.sess) {
		pin = "Unpin"
	}
	labels := []string{"Rename", "Close", pin}
	// Appended last so the three above keep indices 0/1/2; present only on the
	// attached session's multi-pane active window (see openRowMenu).
	if m.menu.canBreakPane {
		labels = append(labels, breakPaneLabel)
	}
	return labels
}

type rowMenu struct {
	mode   menuMode
	sess   string // the session this menu acts on
	fake   bool   // synthetic row: the menu opens, the actions decline
	anchor int    // pane line clicked; the box hangs under it
	item   int
	input  textField // rename buffer
	notice string    // why the last action did nothing

	// Break-pane state, cached when the menu opens on the attached session so
	// the item's presence is stable while the menu is up (see openRowMenu):
	canBreakPane bool   // active window has ≥2 panes → offer Pane → own session
	activePane   string // the pane id that gets broken out
}

func (m *model) menuOpen() bool { return m.menu.mode != menuClosed }

func (m *model) closeMenu() { m.menu = rowMenu{} }

// openRowMenu points the menu at the row under the pointer.
//
// It deliberately does NOT select that row — selection attaches the terminal
// to a session, and "tell me about this one" must not move the user. It also
// does not hand focus back to the inner pane the way a left-click does: the
// menu is the one thing in this sidebar that needs the keyboard.
func (m *model) openRowMenu(rowIdx, y int) {
	r, ok := m.rowAt(rowIdx)
	if !ok {
		m.closeMenu()
		return
	}
	m.menu = rowMenu{mode: menuActions, sess: r.session, fake: r.fake, anchor: y}
	// Offer Pane → own session only on the ATTACHED session (the one whose
	// active pane this wrapper's client actually sits in) and only when that
	// active window has a pane to spare. Fetched once, here, so the item does
	// not blink in and out as panes come and go under an open menu.
	if !r.fake && m.attachedBySess[r.session] {
		if pane, n := paneInfo(r.session); n >= 2 {
			m.menu.canBreakPane = true
			m.menu.activePane = pane
		}
	}
}

// rightClick opens the menu on the card under the pointer, and closes it
// anywhere else — including on the menu's own lines, which map to no row.
func (m *model) rightClick(x, y int) {
	if ri, ok := m.pane.rowAtLine(y); ok {
		m.openRowMenu(ri, y)
		return
	}
	m.closeMenu()
}

// menuClick routes a left click while the menu is open. Either way the click
// STOPS here: falling through to the row/copy maps underneath would mean
// dismissing the menu also attached whatever session the box was covering.
func (m *model) menuClick(x, y int) {
	for i, z := range m.pane.menuRows {
		if z.hit(x, y) {
			m.menu.item = i
			m.activate()
			return
		}
	}
	if !m.pane.menuBox.hit(x, y) {
		m.closeMenu()
	}
}

// menuKey routes a key while the menu is open. It runs INSTEAD of the list's
// own handler (see key), so q does not quit the program and j/k do not walk
// the selection — which would attach a session out from under an open menu.
func (m *model) menuKey(msg tea.KeyMsg) tea.Cmd {
	switch m.menu.mode {
	case menuRename:
		return m.renameKey(msg)
	case menuBreakPane:
		return m.breakPaneKey(msg)
	case menuConfirm:
		return m.confirmKey(msg)
	}
	switch msg.Type {
	case tea.KeyEsc:
		m.closeMenu()
		return nil
	case tea.KeyUp:
		m.menuMove(-1)
		return nil
	case tea.KeyDown:
		m.menuMove(1)
		return nil
	case tea.KeyEnter:
		m.activate()
		return nil
	}
	for _, r := range typedRunes(msg) {
		switch r {
		case 'k':
			m.menuMove(-1)
		case 'j':
			m.menuMove(1)
		case 'q':
			m.closeMenu()
		}
	}
	return nil
}

func (m *model) menuMove(d int) {
	m.menu.notice = ""
	n := len(m.menuActionLabels())
	m.menu.item = (m.menu.item + d + n) % n
}

// activate runs the highlighted item. Rename opens its input prefilled with
// the current name, so the usual edit is a suffix rather than a retype; Close
// asks first, being the only irreversible thing this program does.
func (m *model) activate() {
	m.menu.notice = ""
	switch m.menu.item {
	case itemRename:
		m.menu.mode = menuRename
		m.menu.input = newTextField(m.menu.sess, boxInner(m.paneWidth()-3)-1)
	case itemClose:
		m.menu.mode = menuConfirm
	case itemPin:
		// pin/unpin the row the menu is on, then close. It does not attach or
		// move the selection — the menu acts ON a session, it does not switch
		// to it. A synthetic row names no tmux session, so it is not pinnable.
		if m.menu.fake {
			m.menu.notice = "synthetic row — nothing to pin"
			return
		}
		m.togglePin(m.menu.sess)
		m.closeMenu()
	case itemBreakPane:
		// Same prefilled-name field the rename uses, so a break-out is a
		// suffix edit rather than a retype. commitBreakPane runs on Enter.
		m.menu.mode = menuBreakPane
		m.menu.input = newTextField(m.menu.sess, boxInner(m.paneWidth()-3)-1)
	}
}

// renameKey drives the rename field. Esc and Enter are the menu's; everything
// else is the text field's, which is what buys the editing keys (word moves,
// ^U, ^K, left/right) without this file growing an editor of its own.
func (m *model) renameKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.closeMenu()
		return nil
	case tea.KeyEnter:
		m.commitRename()
		return nil
	}
	if msg.Alt && msg.Type == tea.KeyRunes {
		return nil // M-x belongs to the outer wrapper, not to a text field
	}
	return m.menu.input.key(msg)
}

// confirmKey takes y as yes and everything else — n, Esc, a stray click's
// worth of keys — as no. A confirm that could be dismissed INTO the action is
// not a confirm.
func (m *model) confirmKey(msg tea.KeyMsg) tea.Cmd {
	for _, r := range typedRunes(msg) {
		if r == 'y' || r == 'Y' {
			m.commitClose()
			return nil
		}
	}
	m.closeMenu()
	return nil
}
