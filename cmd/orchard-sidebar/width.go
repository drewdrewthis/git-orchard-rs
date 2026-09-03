package main

// The width contract, in one place.
//
// The OUTER server owns the sidebar's width. It holds main-pane-width (tmux's
// own option, widthOption in tmux.go — not a custom @orchard one, see the
// comment above that const), its M-s binding reads it, and its
// client-resized / window-resized hooks re-pin the pane to it after every
// terminal resize (scripts/outer-shell/outer.conf). The sidebar's job is to
// notice the ONE event the outer server cannot see — the user dragging the
// pane border — and publish it.
//
// Two owners is what this replaces: the sidebar published a width inwards
// while the outer hooks re-pinned to a hard-coded 40, so a drag to 60 survived
// exactly until the next terminal resize, at which point the hook pinned 40,
// the sidebar read that back as a fresh drag, and republished 40 over the 60
// the user had asked for.

// applyWidth records the width tmux just handed this pane and decides whether
// the user caused it.
func (m *model) applyWidth(w int) {
	if isCollapsedWidth(w) {
		// A collapsed pane is not a drag, whoever collapsed it (this sidebar's
		// own button, outer.conf's M-s, or its resize hooks re-pinning after a
		// terminal resize). Publishing 3 as the width would collapse the pane
		// for good, and enforcing the readable floor back over it would fight
		// the collapse open again on the next tick.
		m.width, m.collapsed, m.sized = w, true, true
		return
	}
	m.collapsed = false
	if !m.sized {
		// the first size is the wrapper's own split, not a gesture. It seeds
		// the width only when nothing was restored from disk: a restore that
		// has not landed yet must not be overwritten by the pre-restore size.
		m.sized = true
		if m.desiredWidth == 0 {
			m.desiredWidth = w
		}
		m.width = w
		return
	}
	if w != m.desiredWidth {
		m.publishWidth(w)
	}
	m.width = w
}

// publishWidth makes a dragged width the shared one: the outer server's
// option (what the hooks and M-s re-pin to for the rest of this tmux server's
// life) and the state file (what survives it).
func (m *model) publishWidth(w int) {
	clamped := max(w, minWidth)
	m.desiredWidth = clamped
	setWidthOption(clamped)
	m.persistState()
	if clamped != w {
		resizePane(clamped) // the readable floor kicked in
	}
}

// toggleCollapse drives the pane between its full width and the 3-column
// strip, and remembers which one the user left it in.
func (m *model) toggleCollapse() {
	m.collapsed = !m.collapsed
	w := m.expandWidth()
	if m.collapsed {
		w = collapsedWidth
	}
	m.width = w
	setCollapsed(m.collapsed, w)
	m.persistState()
	handBackFocus()
}

// expandWidth is the width a collapsed sidebar reopens to: the width the user
// dragged to when there is one (restored from disk at startup, so it survives
// a restart), else the 40 columns the wrapper splits at.
func (m *model) expandWidth() int {
	if m.desiredWidth >= minWidth {
		return m.desiredWidth
	}
	return defaultWidth
}

// persistState writes every preference the sidebar remembers, in one go: the
// file is one object, so a writer that knew only about the layout would drop
// the bell setting on the next drag. Every caller is a single deliberate
// gesture — a drag, a collapse, a bell toggle — so this is a few dozen bytes
// at human frequency, not a write loop.
func (m *model) persistState() {
	st := sidebarState{Width: m.desiredWidth, Collapsed: m.collapsed, Bell: m.bell}
	if err := saveSidebarState(st); err != nil {
		logf("saving sidebar state: %v", err)
	}
}
