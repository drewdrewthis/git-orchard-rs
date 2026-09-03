package main

// What the row menu actually does to a session: rename it, or close it. Split
// from the menu's state machine (menu.go) because these are the only two
// functions in the sidebar that MUTATE a session rather than reading one, and
// they carry the guards that go with that.

// commitRename renames the session and carries every identity that pointed at
// the old name over to the new one, so the selection, the scroll anchor and
// the hook/attach maps do not lose the row during the two seconds before the
// next poll re-reads tmux.
func (m *model) commitRename() {
	old, name := m.menu.sess, sessionSafe(m.menu.input.value())
	if name == "" || name == old {
		m.closeMenu()
		return
	}
	if m.menu.fake {
		m.menu.notice = "synthetic row — nothing to rename"
		return
	}
	if err := renameSession(old, name); err != nil {
		m.menu.notice = err.Error()
		return
	}
	m.renameEverywhere(old, name)
	m.closeMenu()
}

func (m *model) renameEverywhere(old, name string) {
	for i := range m.rows {
		if m.rows[i].session == old {
			m.rows[i].session = name
		}
	}
	for _, p := range []*string{&m.cursorSess, &m.anchorSess} {
		if *p == old {
			*p = name
		}
	}
	rekey(m.hooksBySess, old, name)
	rekey(m.attachedBySess, old, name)
	// paneToSess holds the name as its VALUE — a stale one there would make
	// applyHooks append a ghost card under the old name on the next tick
	for k, v := range m.paneToSess {
		if v == old {
			m.paneToSess[k] = name
		}
	}
	m.reanchorCursor()
}

// rekey moves one entry to a new key, for the several session-keyed maps a
// rename has to follow.
func rekey[V any](mm map[string]V, old, name string) {
	if v, ok := mm[old]; ok {
		delete(mm, old)
		mm[name] = v
	}
}

// commitClose kills the session, after the guard that matters: never kill the
// session this wrapper's own client is sitting in without moving the client
// somewhere else FIRST. tmux drops a client whose session dies, which on this
// wrapper means the user's terminal goes with it. With nowhere to move to, the
// close is refused outright rather than taking the terminal down.
func (m *model) commitClose() {
	name := m.menu.sess
	if m.menu.fake {
		m.menu.notice = "synthetic row — nothing to close"
		return
	}
	if name == m.cursorSess {
		alt := m.altSession(name)
		if alt == "" {
			m.menu.notice = "last session — close refused"
			return
		}
		switchClient(alt, false)
		m.cursorSess = alt
	}
	if err := killSession(name); err != nil {
		m.menu.notice = err.Error()
		return
	}
	m.dropRow(name)
	m.closeMenu()
}

// altSession is a real session to move the client to before killing name.
// Synthetic rows name no tmux session, so they can never be the escape hatch.
func (m *model) altSession(name string) string {
	for _, r := range m.rows {
		if !r.fake && r.session != name {
			return r.session
		}
	}
	return ""
}

// dropRow retires the killed session's card immediately. The next poll would
// do it anyway; waiting leaves a card on screen that attaches to nothing.
func (m *model) dropRow(name string) {
	kept := m.rows[:0]
	for _, r := range m.rows {
		if r.session != name {
			kept = append(kept, r)
		}
	}
	m.rows = kept
	if m.anchorSess == name {
		m.anchorSess = ""
	}
	m.reanchorCursor()
}
