package main

import (
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The launch modal's fuzzy directory picker: how it orders an empty query,
// walks candidates in, widens the roots, and scrolls a long result list.

func matchPaths(ms []dirMatch) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.path
	}
	return out
}

func matchesOf(paths ...string) []dirMatch {
	ms := make([]dirMatch, len(paths))
	for i, p := range paths {
		ms[i] = dirMatch{path: p}
	}
	return ms
}

// AC8: an empty query lists the persisted recent launch dirs first, then the
// roots, deduped.
func TestPickerEmptyQueryOrder(t *testing.T) {
	p := &picker{
		recents: []string{"/w/recent-a", "/w/recent-b"},
		roots:   []string{"/w/recent-a", "/home"}, // recent-a also a root: shown once
	}
	p.search = newTextField("", searchWidth)
	p.rebuild()
	got := matchPaths(p.matches)
	want := []string{"/w/recent-a", "/w/recent-b", "/home"}
	if len(got) != len(want) {
		t.Fatalf("empty-query order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("empty-query order = %v, want %v", got, want)
		}
	}
}

// The search field's placeholder is the only affordance telling you the picker
// takes typed text — a cheap regression against a typo.
func TestPickerSearchPlaceholder(t *testing.T) {
	p := newPicker(t.TempDir(), nil)
	if got := p.search.ti.Placeholder; got != "(type to search)" {
		t.Errorf("placeholder = %q, want %q", got, "(type to search)")
	}
}

// Once the walk lands, a query searches the whole candidate set and the
// highlighted match is the launch directory.
func TestPickerSearchesWalkedCandidates(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // no leaked recents in the pool
	root := t.TempDir()
	mkdirs(t, root, "cmd/orchard-sidebar", "docs")
	p := newPicker(root, nil)
	p.cfg = walkConfig{roots: []string{root}} // keep the test walk off the real $HOME
	p.setCands(p.walkGen, walkCandidates(p.cfg))

	p.searchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("orsi")})
	if p.cursor != 0 {
		t.Fatalf("cursor = %d after typing, want 0", p.cursor)
	}
	if want := filepath.Join(root, "cmd/orchard-sidebar"); p.dir() != want {
		t.Fatalf("dir() = %q, want %q", p.dir(), want)
	}
	// clearing the query returns to the empty-query list (roots, no crash)
	if !p.backspaceSearch() {
		t.Fatal("backspace on a typed query reported nothing to delete")
	}
}

// Backspace on an empty query widens the roots one level up and re-walks.
func TestPickerWidenAddsParents(t *testing.T) {
	tmp := t.TempDir()
	mkdirs(t, tmp, "ws/proj")
	p := &picker{roots: []string{filepath.Join(tmp, "ws", "proj")}}
	p.search = newTextField("", searchWidth)
	p.cfg = walkConfig{roots: p.roots}

	if cmd := p.widen(); cmd == nil {
		t.Fatal("widen returned no re-walk command")
	}
	if !hasPath(p.roots, filepath.Join(tmp, "ws")) {
		t.Errorf("widen did not add the parent: %v", p.roots)
	}
}

// A widen/toggle fired while a walk is in flight bumps walkGen, so the older
// walk's eventual result is a stale walkDoneMsg: setCands must drop it (and
// leave walking alone) rather than let it clobber the newer walk's state.
func TestPickerSetCandsDropsStaleGeneration(t *testing.T) {
	tmp := t.TempDir()
	mkdirs(t, tmp, "ws/proj")
	p := &picker{roots: []string{filepath.Join(tmp, "ws", "proj")}}
	p.search = newTextField("", searchWidth)
	p.cfg = walkConfig{roots: p.roots}

	staleGen := p.walkGen
	if cmd := p.widen(); cmd == nil {
		t.Fatal("widen returned no re-walk command")
	}
	if p.walkGen == staleGen {
		t.Fatal("widen did not bump walkGen")
	}
	if !p.walking {
		t.Fatal("widen did not leave the picker walking")
	}

	// the superseded walk's result lands late: it must be ignored
	p.setCands(staleGen, []string{"/should-not-apply"})
	if !p.walking {
		t.Error("a stale walkDoneMsg cleared walking")
	}
	if hasPath(p.cands, "/should-not-apply") {
		t.Error("a stale walkDoneMsg's candidates were applied")
	}

	// the current walk's result lands: it must be applied
	current := []string{filepath.Join(tmp, "ws", "proj")}
	p.setCands(p.walkGen, current)
	if p.walking {
		t.Error("the current walkDoneMsg left walking true")
	}
	if !hasPath(p.cands, current[0]) {
		t.Error("the current walkDoneMsg's candidates were not applied")
	}
}

// The picker's viewport: a result list longer than the modal has rows scrolls
// under the cursor instead of overflowing the popup.
func TestPickerWindowScrollsUnderTheCursor(t *testing.T) {
	p := &picker{matches: matchesOf("/a", "/b", "/c", "/d", "/e", "/f")} // 6 entries
	cases := []struct {
		name   string
		cursor int
		rows   int
		top    int
		window []string
	}{
		{"everything fits", 0, 6, 0, []string{"/a", "/b", "/c", "/d", "/e", "/f"}},
		{"more rows than entries", 3, 10, 0, []string{"/a", "/b", "/c", "/d", "/e", "/f"}},
		{"cursor inside the first window", 2, 3, 0, []string{"/a", "/b", "/c"}},
		{"cursor past the window: scrolls by one", 3, 3, 1, []string{"/b", "/c", "/d"}},
		{"cursor at the end", 5, 3, 3, []string{"/d", "/e", "/f"}},
		{"no rows to draw", 5, 0, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p.cursor = c.cursor
			if got := p.top(c.rows); got != c.top {
				t.Errorf("top(%d) = %d, want %d", c.rows, got, c.top)
			}
			got := matchPaths(p.window(c.rows))
			if len(got) != len(c.window) {
				t.Fatalf("window(%d) = %v, want %v", c.rows, got, c.window)
			}
			for i := range got {
				if got[i] != c.window[i] {
					t.Fatalf("window(%d) = %v, want %v", c.rows, got, c.window)
				}
			}
			if len(got) > 0 {
				if i := p.cursor - p.top(c.rows); i < 0 || i >= len(got) {
					t.Errorf("cursor %d falls outside the window %v", p.cursor, got)
				}
			}
		})
	}
}
