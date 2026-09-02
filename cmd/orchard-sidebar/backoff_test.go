package main

// Client-lane idle backoff (#727).
//
// fetchClientSession execs `tmux list-clients` on every tick. At a flat 150ms
// that is ~7 forks/sec per sidebar and ~40 across a desktop of six, forever,
// whether or not the answer moved. These tests pin the two halves of the fix:
// the pure ladder (idleBackoff), and the four events that snap it back to fast.

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ---- table helpers

func repDur(d time.Duration, n int) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = d
	}
	return out
}

func repRead(r clientRead, n int) []clientRead {
	out := make([]clientRead, n)
	for i := range out {
		out[i] = r
	}
	return out
}

func concatDur(groups ...[]time.Duration) []time.Duration {
	var out []time.Duration
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func concatRead(groups ...[]clientRead) []clientRead {
	var out []clientRead
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// quietTmux stubs every client-side exec the lane can reach, so a unit test
// never forks tmux.
func quietTmux(t *testing.T) (wrote *[]int, resized *[]int) {
	t.Helper()
	var w, r []int
	origSet, origRes, origSw := setWidthOption, resizePane, switchClient
	setWidthOption = func(x int) { w = append(w, x) }
	resizePane = func(x int) { r = append(r, x) }
	switchClient = func(string) {}
	t.Cleanup(func() { setWidthOption, resizePane, switchClient = origSet, origRes, origSw })
	return &w, &r
}

// captureClientTicks records the cadence the client lane actually schedules —
// the number this issue is about. Other lanes' ticks pass through untouched.
func captureClientTicks(t *testing.T) *[]time.Duration {
	t.Helper()
	var got []time.Duration
	orig := tickAfter
	tickAfter = func(d time.Duration, msg tea.Msg) tea.Cmd {
		if _, ok := msg.(clientTickMsg); ok {
			got = append(got, d)
		}
		return orig(d, msg)
	}
	t.Cleanup(func() { tickAfter = orig })
	return &got
}

// decayToCap feeds enough identical reads to walk the whole ladder down.
func decayToCap(m *model) {
	for _, r := range repRead(clientRead{session: "alpha", width: 42}, 1+clientFastHold+len(clientLadder)) {
		m.Update(clientSessMsg{name: r.session, width: r.width, gen: m.clientGen})
	}
}

// ---- the ladder itself

// The ladder's numbers are pinned here and only here; every other test uses
// the symbols, so re-tuning the policy touches one table.
func TestClientLadderShape(t *testing.T) {
	want := []time.Duration{
		150 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
	}
	if len(clientLadder) != len(want) {
		t.Fatalf("clientLadder = %v, want %v", clientLadder, want)
	}
	for i, d := range want {
		if clientLadder[i] != d {
			t.Errorf("clientLadder[%d] = %v, want %v", i, clientLadder[i], d)
		}
	}
	if clientLadder[0] != clientEvery {
		t.Errorf("fast rung = %v, want the lane's existing %v", clientLadder[0], clientEvery)
	}
	if clientFastHold <= 0 {
		t.Errorf("clientFastHold = %d: no fast window after a change", clientFastHold)
	}
}

func TestIdleBackoffLadder(t *testing.T) {
	alpha := clientRead{session: "alpha", width: 42}
	beta := clientRead{session: "beta", width: 42}
	wider := clientRead{session: "alpha", width: 60}

	// The first read differs from the zero value, so it counts as a change:
	// one fast tick for it, then the hold window, then one rung per read.
	fastRun := repDur(clientEvery, 1+clientFastHold)
	settled := repRead(alpha, 1+clientFastHold)
	decay := []time.Duration{
		500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second,
	}

	cases := []struct {
		name  string
		reads []clientRead
		want  []time.Duration
	}{
		{
			name:  "an answer that keeps changing never leaves the fast rung",
			reads: []clientRead{alpha, beta, alpha, beta, alpha, beta, alpha, beta, alpha, beta, alpha, beta},
			want:  repDur(clientEvery, 12),
		},
		{
			name:  "identical reads hold fast for a window, then step down and cap",
			reads: concatRead(settled, repRead(alpha, 4)),
			want:  concatDur(fastRun, decay),
		},
		{
			name:  "a changed session snaps back from the cap and holds fast again",
			reads: concatRead(settled, repRead(alpha, 4), repRead(beta, 3)),
			want:  concatDur(fastRun, decay, repDur(clientEvery, 3)),
		},
		{
			name:  "a width change under an unchanged session is still a change",
			reads: concatRead(settled, repRead(alpha, 4), []clientRead{wider}),
			want:  concatDur(fastRun, decay, []time.Duration{clientEvery}),
		},
		{
			name:  "a read that fails (empty answer) is itself a change, then settles",
			reads: concatRead(settled, repRead(alpha, 4), repRead(clientRead{}, 3)),
			want:  concatDur(fastRun, decay, repDur(clientEvery, 3)),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b idleBackoff
			var got []time.Duration
			for _, r := range c.reads {
				got = append(got, b.observe(r))
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %d intervals, want %d", len(got), len(c.want))
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("read %d (%+v): interval %v, want %v\nfull: %v", i, c.reads[i], got[i], c.want[i], got)
				}
			}
		})
	}
}

func TestIdleBackoffZeroValueIsFast(t *testing.T) {
	var b idleBackoff
	if got := b.interval(); got != clientEvery {
		t.Fatalf("zero value ticks at %v, want %v", got, clientEvery)
	}
}

func TestIdleBackoffIntervalDoesNotAdvance(t *testing.T) {
	var b idleBackoff
	r := clientRead{session: "alpha", width: 42}
	for i := 0; i < 1+clientFastHold+len(clientLadder); i++ {
		b.observe(r)
	}
	first := b.interval()
	for i := 0; i < 5; i++ {
		if got := b.interval(); got != first {
			t.Fatalf("interval() moved on read %d: %v then %v", i, first, got)
		}
	}
}

func TestIdleBackoffResetRestartsTheFastWindow(t *testing.T) {
	var b idleBackoff
	r := clientRead{session: "alpha", width: 42}
	for i := 0; i < 1+clientFastHold+len(clientLadder); i++ {
		b.observe(r)
	}
	if got := b.interval(); got != clientLadder[len(clientLadder)-1] {
		t.Fatalf("did not reach the cap: %v", got)
	}
	b.reset()
	if got := b.interval(); got != clientEvery {
		t.Fatalf("after reset: %v, want %v", got, clientEvery)
	}
	// reset is not a change: the same read afterwards must not re-trigger, but
	// it must still spend the whole hold window at the fast rung.
	for i := 0; i < clientFastHold; i++ {
		if got := b.observe(r); got != clientEvery {
			t.Fatalf("hold read %d after reset: %v, want %v", i, got, clientEvery)
		}
	}
	if got := b.observe(r); got != clientLadder[1] {
		t.Fatalf("decay did not resume after the hold window: %v, want %v", got, clientLadder[1])
	}
}

// ---- sameAttach: the push lane's snap-back trigger

func TestSameAttach(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]bool
		want bool
	}{
		{"both empty", map[string]bool{}, map[string]bool{}, true},
		{"nil vs empty", nil, map[string]bool{}, true},
		{"nil vs populated", nil, map[string]bool{"a": true}, false},
		{"identical", map[string]bool{"a": true, "b": false}, map[string]bool{"a": true, "b": false}, true},
		{"flag flipped", map[string]bool{"a": true}, map[string]bool{"a": false}, false},
		{"session added", map[string]bool{"a": true}, map[string]bool{"a": true, "b": false}, false},
		{"session gone", map[string]bool{"a": true, "b": false}, map[string]bool{"a": true}, false},
		{"same size, different names", map[string]bool{"a": true}, map[string]bool{"b": true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameAttach(c.a, c.b); got != c.want {
				t.Errorf("sameAttach(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// ---- wiring: the cadence the lane actually schedules

func TestClientLaneSchedulesTheBackedOffTick(t *testing.T) {
	wrote, _ := quietTmux(t)
	got := captureClientTicks(t)

	m := &model{width: 42, desiredWidth: 42}
	decayToCap(m)

	last := (*got)[len(*got)-1]
	if want := clientLadder[len(clientLadder)-1]; last != want {
		t.Fatalf("idle lane still scheduling %v, want the %v cap (ticks: %v)", last, want, *got)
	}
	if (*got)[0] != clientEvery {
		t.Fatalf("first tick after a change was %v, want %v", (*got)[0], clientEvery)
	}

	m.Update(clientSessMsg{name: "beta", width: 42, gen: m.clientGen})
	if last := (*got)[len(*got)-1]; last != clientEvery {
		t.Fatalf("a changed session scheduled %v, want %v", last, clientEvery)
	}
	// The lane reads the shared width; backing off must not publish one.
	if len(*wrote) != 0 {
		t.Fatalf("client lane published widths: %v", *wrote)
	}
}

func TestStaleClientReadIsNoEvidenceOfIdleness(t *testing.T) {
	quietTmux(t)
	got := captureClientTicks(t)

	// clientGen moved on: every read below predates it and describes the old
	// world, so none of them may be counted as "nothing is happening".
	m := &model{width: 42, desiredWidth: 42, clientGen: 1}
	for i := 0; i < 1+clientFastHold+len(clientLadder)+3; i++ {
		m.Update(clientSessMsg{name: "alpha", width: 42, gen: 0})
	}
	if iv := m.clientTick.interval(); iv != clientEvery {
		t.Fatalf("stale reads decayed the lane to %v", iv)
	}
	for i, d := range *got {
		if d != clientEvery {
			t.Fatalf("stale read %d rescheduled at %v, want %v", i, d, clientEvery)
		}
	}
}

// ---- wiring: the four snap-back events

func TestResizeResetsTheClientLane(t *testing.T) {
	wrote, resized := quietTmux(t)
	captureClientTicks(t)

	m := &model{width: 42, desiredWidth: 42}
	decayToCap(m)
	if m.clientTick.interval() == clientEvery {
		t.Fatal("lane never decayed; the rest of this test proves nothing")
	}

	// A size equal to desiredWidth takes none of the width branches, so this
	// asserts the reset alone — shared-width behaviour is untouched.
	m.Update(tea.WindowSizeMsg{Width: 42, Height: 50})
	if iv := m.clientTick.interval(); iv != clientEvery {
		t.Errorf("resize left the lane at %v, want %v", iv, clientEvery)
	}
	if len(*wrote) != 0 || len(*resized) != 0 {
		t.Errorf("resize touched width traffic: wrote=%v resized=%v", *wrote, *resized)
	}
}

func TestSelectRowResetsTheClientLane(t *testing.T) {
	quietTmux(t)
	captureClientTicks(t)

	m := &model{width: 42, desiredWidth: 42, rows: []row{{session: "alpha"}, {session: "beta"}}}
	decayToCap(m)
	if m.clientTick.interval() == clientEvery {
		t.Fatal("lane never decayed; the rest of this test proves nothing")
	}

	m.selectRow(1)
	if iv := m.clientTick.interval(); iv != clientEvery {
		t.Errorf("switch left the lane at %v, want %v", iv, clientEvery)
	}
}

func TestAttachChurnResetsTheClientLane(t *testing.T) {
	quietTmux(t)
	captureClientTicks(t)

	one := []tmuxSession{{Name: "alpha", Attached: true}}
	m := &model{width: 42, desiredWidth: 42}
	m.Update(tmuxSubMsg{sessions: one}) // first snapshot: adopt, nothing to compare
	decayToCap(m)
	if m.clientTick.interval() == clientEvery {
		t.Fatal("lane never decayed; the rest of this test proves nothing")
	}

	// A repeated snapshot is the steady state — it must NOT re-arm the fast
	// lane, or the push lane's keepalives would defeat the backoff entirely.
	m.Update(tmuxSubMsg{sessions: one})
	if iv := m.clientTick.interval(); iv == clientEvery {
		t.Fatal("an identical snapshot re-armed the fast lane")
	}

	// A detach anywhere means "which session is this client on" is about to
	// move, and it is the one such event the sidebar did not itself cause.
	m.Update(tmuxSubMsg{sessions: []tmuxSession{{Name: "alpha", Attached: false}}})
	if iv := m.clientTick.interval(); iv != clientEvery {
		t.Errorf("detach left the lane at %v, want %v", iv, clientEvery)
	}

	decayToCap(m)
	m.Update(tmuxSubMsg{sessions: []tmuxSession{
		{Name: "alpha", Attached: false}, {Name: "beta", Attached: true},
	}})
	if iv := m.clientTick.interval(); iv != clientEvery {
		t.Errorf("a new attached session left the lane at %v, want %v", iv, clientEvery)
	}

	// A dropped socket says nothing about attachment; it must not re-arm.
	decayToCap(m)
	m.Update(tmuxSubMsg{err: errors.New("socket dropped")})
	if iv := m.clientTick.interval(); iv == clientEvery {
		t.Error("a subscription error re-armed the fast lane")
	}
}
