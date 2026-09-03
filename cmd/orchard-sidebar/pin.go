package main

// The pinned set: an ordered []string of session names on the model, mirrored
// to sidebar-state.json. Everything view-facing derives from it — a row's
// pinRank (applyPins) which sortRows reads, and the block/separator cards.go
// draws. Kept here, pure and small, so model.go / selection.go stay under the
// file cap and the gestures (P, drag, M-Shift, menu) are all thin callers.

// isPinned reports whether a session name is in the pinned set.
func (m *model) isPinned(name string) bool {
	for _, n := range m.pinned {
		if n == name {
			return true
		}
	}
	return false
}

// applyPins stamps each row's pinRank from m.pinned so sortRows can place the
// block. Called in rebuild before the sort. A row whose session is not pinned
// gets rank 0; ranks are 1-based and in pinned-slice order.
func (m *model) applyPins() {
	rank := make(map[string]int, len(m.pinned))
	for i, name := range m.pinned {
		rank[name] = i + 1
	}
	for i := range m.rows {
		m.rows[i].pinRank = rank[m.rows[i].session]
	}
}

// togglePin pins an unpinned session (appended to the end of the block) or
// unpins a pinned one, then persists and rebuilds. It never attaches or moves
// the selection — pinning is a statement about the list, not a switch.
func (m *model) togglePin(name string) {
	if name == "" {
		return
	}
	if m.isPinned(name) {
		kept := m.pinned[:0]
		for _, n := range m.pinned {
			if n != name {
				kept = append(kept, n)
			}
		}
		m.pinned = kept
	} else {
		m.pinned = append(m.pinned, name)
	}
	m.persistState()
	m.rebuild()
}

// reorderPin moves a pinned session by one place within the block: dir -1 up,
// +1 down. A no-op on an unpinned session and at the block ends, so the two
// M-Shift chords clamp rather than wrap.
func (m *model) reorderPin(name string, dir int) {
	at := -1
	for i, n := range m.pinned {
		if n == name {
			at = i
			break
		}
	}
	if at < 0 {
		return // not pinned: M-Shift on an unpinned selection does nothing
	}
	to := at + dir
	if to < 0 || to >= len(m.pinned) {
		return // already at the top/bottom of the block
	}
	m.pinned[at], m.pinned[to] = m.pinned[to], m.pinned[at]
	m.persistState()
	// keep the viewport on the card the user is moving
	m.snapSel = true
	m.rebuild()
}

// pruneStalePins drops pinned names absent from the authoritative tmux live
// set and persists the pruned slice. Called ONLY from applySessions (a real
// sessions snapshot), never from a transient fast-lane miss, so a daemon spike
// cannot persist away a still-alive pin.
func (m *model) pruneStalePins(live map[string]bool) {
	if len(m.pinned) == 0 {
		return
	}
	kept := m.pinned[:0]
	changed := false
	for _, name := range m.pinned {
		if live[name] {
			kept = append(kept, name)
		} else {
			changed = true
		}
	}
	m.pinned = kept
	if changed {
		m.persistState()
	}
}
