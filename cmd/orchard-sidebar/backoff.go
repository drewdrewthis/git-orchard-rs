package main

import "time"

// ---- client-lane cadence (#727)
//
// fetchClientSession execs `tmux list-clients` on every client-lane tick. At a
// flat 150ms that is ~7 forks/sec per sidebar — ~40 across a desktop of six —
// forever, whether or not the answer moved. 150ms is only worth paying while
// the answer is actually changing, so the lane decays toward a 2s idle cadence
// and snaps back the instant anything moves. RULES O6 (steady-state poll cost
// is bounded; slow down when nothing changes).

// clientRead is the answer one client-lane exec produces: which session this
// client is on. Compared by value — a changed session snaps the cadence back
// to fast. (The outer server owns width here, so the client lane no longer
// reads it — see width.go; a switch is the only thing this lane's answer
// tracks.)
type clientRead struct {
	session string
}

// clientLadder is the cadence ladder, fast rung first; the last rung is a cap,
// not a wrap. Idle steady state is one exec per sidebar per 2s.
var clientLadder = []time.Duration{
	clientEvery,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
}

// clientFastHold is how many identical reads the lane tolerates before it
// starts decaying, i.e. ~1.2s of 150ms responsiveness AFTER the last change —
// which is exactly when the next one is most likely (a switch lands, then the
// shared-width enforcement echoes back through the same read).
const clientFastHold = 8

// idleBackoff is the client lane's tick policy. Pure state: no clock, no I/O,
// no tmux — the whole ladder is decided from the sequence of reads and push-
// lane health alone. The zero value is a lane that has read nothing yet,
// assumes a healthy push lane, and ticks at the fast rung.
type idleBackoff struct {
	last     clientRead
	idle     int  // consecutive identical reads since the last change
	pushDown bool // true while the push lane cannot be trusted to re-arm us
}

// observe records one read and returns how long to wait before the next. A
// read that differs from the previous one drops back to the fast rung;
// identical reads hold there for clientFastHold, then step down one rung per
// read until the cap. While the push lane is down, every read is pinned to
// the fast rung regardless of whether it changed: the push lane's
// attach/detach signal is what normally re-arms the lane for a switch it
// didn't cause itself, and a lane that cannot receive that signal cannot
// afford to coast on the assumption nothing is happening (PR #757 review,
// discussion_r3918791010).
func (b *idleBackoff) observe(r clientRead) time.Duration {
	if b.pushDown || r != b.last {
		b.last = r
		b.idle = 0
		return b.interval()
	}
	b.idle++
	return b.interval()
}

// reset snaps the lane back to the fast rung without recording a read — for
// the events that predict a change rather than being one: a pane resize, a
// switch driven from this sidebar, an attach/detach seen on the push lane.
// It restarts the hold window, so the following identical read does not
// immediately resume decaying.
func (b *idleBackoff) reset() { b.idle = 0 }

// observePushHealth records whether the push lane is currently delivering.
// Going down snaps the lane back to the fast rung immediately, the same as
// any other reset trigger; observe then holds it there for as long as
// pushDown stays true. The lane cannot rely on attach signals it isn't
// receiving, so it cannot climb past the one rung it can still get right on
// its own (PR #757 review, discussion_r3918791010).
func (b *idleBackoff) observePushHealth(live bool) {
	b.pushDown = !live
	if b.pushDown {
		b.idle = 0
	}
}

// interval reports the current cadence without advancing it.
func (b *idleBackoff) interval() time.Duration {
	i := b.idle - clientFastHold
	if i < 0 {
		i = 0
	}
	if i >= len(clientLadder) {
		i = len(clientLadder) - 1
	}
	return clientLadder[i]
}

// sameAttach reports whether two attach snapshots agree. An attach or detach
// anywhere is the one hint that "which session is THIS client on" is about to
// move which the sidebar gets without having caused it itself, so it is what
// re-arms the fast rung off the push lane.
func sameAttach(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
