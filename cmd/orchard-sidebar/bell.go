package main

import (
	"fmt"
	"os"
)

// The Needs-attention count, and the one sound this program makes.
//
// The badge is a projection of the same bucket the list's first section is
// (rowBucket), so the number and the section can never disagree — and the
// filter, which only decides what is drawn, cannot change it.

// attnGlyph is the badge's dot: the same glyph the attention cards carry, in
// the same amber, so the count reads as "that many of those".
const attnGlyph = "●"

// attnCount is the size of the Needs-attention bucket, synthetic rows
// included: the badge counts what the list shows.
func (m *model) attnCount() int {
	n := 0
	for _, r := range m.rows {
		if rowBucket(r) == bucketAttention {
			n++
		}
	}
	return n
}

// attnBadge is the header's count, empty when nothing needs anyone. A "0"
// badge is a badge that teaches you to stop reading badges.
func (m *model) attnBadge() string {
	n := m.attnCount()
	if n == 0 {
		return ""
	}
	return styAttn.Render(fmt.Sprintf("%s%d", attnGlyph, n))
}

// toggleBell turns the bell on or off and remembers it. A preference that has
// to be set again every morning is a preference nobody sets.
func (m *model) toggleBell() {
	m.bell = !m.bell
	m.persistState()
}

// emitBell is a var so tests observe the ring without a terminal. BEL is a C0
// control, which a terminal executes wherever it lands, so a byte written on
// its own fd cannot corrupt an escape sequence bubbletea is midway through.
var emitBell = func() {
	f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString("\a")
}

// bellCheck rings once per rebuild that brings a NEW session into the
// Needs-attention bucket — which covers the 0→N transition and every later
// arrival, and stays silent when the count merely falls or holds.
//
// Three things it deliberately never rings for:
//
//	a synthetic row  ORCHARD_SIDEBAR_FAKE rows exist to be scrolled past; they
//	                 belong in the badge and nowhere near the speaker.
//	the first list   a snapshot of what was already waiting when the sidebar
//	                 started is not a transition.
//	a returning row  rows vanish wholesale when a lane fails (applyFast's wipe,
//	                 applySessions dropping a session tmux stopped reporting),
//	                 so a session is forgotten only while it is still ON SCREEN
//	                 and has genuinely left the bucket. Forgetting it the moment
//	                 its row disappeared would re-ring the whole list on the
//	                 next good poll.
func (m *model) bellCheck() {
	if m.attnSeen == nil {
		m.attnSeen = map[string]bool{}
	}
	now, present := map[string]bool{}, map[string]bool{}
	for _, r := range m.rows {
		present[r.session] = true
		if !r.fake && rowBucket(r) == bucketAttention {
			now[r.session] = true
		}
	}
	ring := false
	for s := range now {
		if !m.attnSeen[s] {
			ring = true
		}
		m.attnSeen[s] = true
	}
	for s := range m.attnSeen {
		if present[s] && !now[s] {
			delete(m.attnSeen, s)
		}
	}
	if !m.attnSeeded {
		m.attnSeeded = len(m.rows) > 0
		return
	}
	if ring && m.bell {
		emitBell()
	}
}
