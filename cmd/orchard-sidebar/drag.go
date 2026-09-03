package main

import (
	tea "github.com/charmbracelet/bubbletea"
)

// Drag-to-pin. The outer conf leaves MouseDrag1Pane unbound and forwards raw
// mouse bytes, so bubbletea receives the full press → motion → release path
// (WithMouseCellMotion reports motion while a button is held).
//
// A press still attaches the card immediately, exactly as a click always has —
// so clicking keeps working even where the terminal never forwards a release,
// and a press with no motion is nothing but that click. Motion during the hold
// arms a drag; the release then pins or unpins by where it landed:
//
//   - release above the separator, was unpinned → pin
//   - release below the separator, was pinned   → unpin
//   - anything else → nothing (the attach on press already stands)

type dragState struct {
	active    bool
	session   string // the session pressed on, re-found by name on release
	startY    int    // press line, the fallback hit-test when no block exists yet
	moved     bool   // a motion event arrived between press and release
	wasPinned bool   // the session's pin state at press, so release knows the direction
}

// dragStart arms a potential drag on the just-pressed (and already attached)
// card. It records the session by name, not index, since the attach re-sorted
// the list out from under the pressed row.
func (m *model) dragStart(session string, y int) {
	if session == "" {
		return
	}
	m.drag = dragState{active: true, session: session, startY: y,
		wasPinned: m.isPinned(session)}
}

// dragMove folds a held-button motion or the terminating release into the
// in-flight drag. Called for every left-button event that is not a press.
func (m *model) dragMove(msg tea.MouseMsg) {
	if !m.drag.active {
		return
	}
	switch msg.Action {
	case tea.MouseActionMotion:
		m.drag.moved = true
	case tea.MouseActionRelease:
		m.dragRelease(msg.Y)
	}
}

// dragThreshold is the minimum vertical travel, in lines, before a release
// counts as a drag rather than a click. A press promotes the attached card
// toward the top (selectRow re-sorts), so a near-click can land a line or two
// off the press without the user meaning to drag; below this it is a plain
// click and never changes a pin.
const dragThreshold = 3

// dragRelease resolves the gesture and clears the drag. A release without
// motion — or with sub-threshold travel — is an ordinary click, already handled
// by the attach on press, so it changes no pin.
func (m *model) dragRelease(y int) {
	d := m.drag
	m.drag = dragState{}
	if !d.active || !d.moved || abs(y-d.startY) < dragThreshold {
		return
	}
	over := m.releaseOverPinned(y)
	switch {
	case over && !d.wasPinned:
		m.togglePin(d.session) // dragged into the block
	case !over && d.wasPinned:
		m.togglePin(d.session) // dragged out below the separator
	}
}

// releaseOverPinned reports whether a release line lands at or above the
// pinned/flat boundary. With a block on screen the separator is the divider (at
// or above it pins); before the first pin there is no separator, so the drop
// target is the first card line — a release at or above the top of the list is
// what pins the first card.
func (m *model) releaseOverPinned(y int) bool {
	if m.pane.pinSep >= 0 {
		return y <= m.pane.pinSep
	}
	return y <= m.pane.firstCardLine()
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
