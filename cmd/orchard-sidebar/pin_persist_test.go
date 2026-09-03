package main

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

// persistState carries the pinned slice, so no other save path (a width drag,
// a collapse, a bell toggle) can wipe it. AC5, AC6.
func TestOtherSavesPreservePins(t *testing.T) {
	saves := captureSaves(t)
	m := pinModel("a", "b")
	m.togglePin("a") // write 1
	m.togglePin("b") // write 2: pinned = [a, b]
	if last := (*saves)[len(*saves)-1]; !reflect.DeepEqual(last.Pinned, []string{"a", "b"}) {
		t.Fatalf("persisted pins = %v, want [a b]", last.Pinned)
	}
	// a bell toggle is an unrelated save: it must not drop the pins.
	m.toggleBell()
	if last := (*saves)[len(*saves)-1]; !reflect.DeepEqual(last.Pinned, []string{"a", "b"}) {
		t.Errorf("a bell toggle wiped the pins: %v", last.Pinned)
	}
}

// A restored pinned slice reproduces the block order on load. AC5.
func TestLoadRestoresPinOrder(t *testing.T) {
	stateHome(t)
	writeStateFile(t, "sidebar-state.json", `{"pinned":["A","B"]}`)
	st := loadSidebarState()
	if !reflect.DeepEqual(st.Pinned, []string{"A", "B"}) {
		t.Fatalf("loaded pinned = %v, want [A B]", st.Pinned)
	}
	m := &model{pinned: st.Pinned, rows: []row{{session: "B"}, {session: "A"}}, width: 42, height: 30}
	m.rebuild()
	if want := []string{"A", "B"}; !reflect.DeepEqual(pinOrder(m), want) {
		t.Errorf("restored order = %v, want %v", pinOrder(m), want)
	}
}

// A corrupt or missing state file yields empty pins and a model that still
// renders with no block. AC8.
func TestCorruptStateYieldsEmptyPins(t *testing.T) {
	stateHome(t)
	writeStateFile(t, "sidebar-state.json", `{not json`)
	if st := loadSidebarState(); len(st.Pinned) != 0 {
		t.Errorf("corrupt file loaded pins = %v, want none", st.Pinned)
	}

	// missing file also yields empty pins
	if err := os.Remove(stateFile("sidebar-state.json")); err != nil {
		t.Fatal(err)
	}
	if st := loadSidebarState(); len(st.Pinned) != 0 {
		t.Errorf("missing file loaded pins = %v, want none", st.Pinned)
	}
}

// applySessions drops a pin whose session is gone from the authoritative live
// set and persists the pruned slice; a fast-lane miss never does. AC7.
func TestStaleDropOnlyOnAuthoritativeAbsence(t *testing.T) {
	saves := captureSaves(t)
	m := pinModel("keep", "gone")
	m.togglePin("keep")
	m.togglePin("gone")

	// a transient fast-lane failure that omits "gone" must NOT drop it.
	m.applyFast(fastDataMsg{err: errors.New("daemon spike")})
	if !m.isPinned("gone") {
		t.Fatalf("a fast-lane miss dropped a still-live pin")
	}

	before := len(*saves)
	// a real sessions snapshot without "gone" is the authoritative drop.
	m.applySessions([]tmuxSession{{Name: "keep"}})
	if m.isPinned("gone") {
		t.Errorf("a vanished session stayed pinned: %v", m.pinned)
	}
	if len(*saves) == before {
		t.Errorf("the pruned pin slice was not persisted")
	}
}
