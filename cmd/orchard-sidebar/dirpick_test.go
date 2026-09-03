package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The launch modal's directory picker: hiding, searching, and scrolling a
// listing, independent of the rest of the form.

func TestSearchDirsHidesAndMatches(t *testing.T) {
	names := []string{"src", ".git", "Docs", "docker", "target"}
	got := strings.Join(searchDirs(names, false, ""), ",")
	if got != "docker,Docs,src,target" { // AC3: empty query, case-insensitive sort, hidden dropped
		t.Errorf("unsearched = %q", got)
	}
	if got := strings.Join(searchDirs(names, true, ""), ","); !strings.Contains(got, ".git") {
		t.Errorf("hidden toggle did not reveal .git: %q", got)
	}
	// AC5: fuzzy match is case-insensitive-ish — "doc" finds docker and Docs both (order is fuzzy's call, so assert the set).
	if want := map[string]bool{"docker": true, "Docs": true}; !sameSet(searchDirs(names, false, "doc"), want) {
		t.Errorf("searched %q, want the set {docker, Docs}", searchDirs(names, false, "doc"))
	}
}

// AC1: an entry shows iff the query is a case-insensitive subsequence of its
// name — "ocs" finds orchard-codex-scripts, "xyz" finds nothing.
func TestSearchDirsSubsequence(t *testing.T) {
	names := []string{"orchard-codex-scripts", "internal", "target"}
	if got := searchDirs(names, false, "ocs"); len(got) != 1 || got[0] != "orchard-codex-scripts" {
		t.Errorf("ocs matched %v, want [orchard-codex-scripts]", got)
	}
	if got := searchDirs(names, false, "xyz"); len(got) != 0 {
		t.Errorf("xyz matched %v, want nothing", got)
	}
}

// AC2: results are ordered by match score, best first — a contiguous "cmd"
// outranks the scattered subsequence in "crates-orchard-md".
func TestSearchDirsRanksByScore(t *testing.T) {
	if got := searchDirs([]string{"crates-orchard-md", "cmd"}, false, "cmd"); len(got) < 1 || got[0] != "cmd" {
		t.Errorf("ranked %v, want cmd first", got)
	}
	// Regression: sahilm/fuzzy ranked the scattered match orchard-codex-scripts
	// above the prefix match ocs-tools for "ocs" — a contiguous/prefix match
	// must outrank a scattered one.
	names := []string{"orchard-codex-scripts", "ocs-tools", "docs", "Docs2"}
	got := searchDirs(names, false, "ocs")
	if len(got) != 4 || got[0] != "ocs-tools" || got[1] != "orchard-codex-scripts" {
		t.Fatalf("ranked %v, want [ocs-tools orchard-codex-scripts ...]", got)
	}
	// Case-insensitive: "DOC" still finds both docs and Docs2 (uppercase query
	// against lowercase and mixed-case names).
	docMatches := searchDirs(names, false, "DOC")
	if !contains(docMatches, "docs") || !contains(docMatches, "Docs2") {
		t.Errorf("DOC matched %v, want docs and Docs2 present", docMatches)
	}
}

func sameSet(got []string, want map[string]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for _, g := range got {
		if !want[g] {
			return false
		}
	}
	return true
}

// The search field's placeholder is the only affordance telling you the
// picker box takes typed text — worth a cheap regression against a typo.
func TestPickerSearchPlaceholder(t *testing.T) {
	p := newPicker(t.TempDir())
	if got := p.search.ti.Placeholder; got != "(type to search)" {
		t.Errorf("placeholder = %q, want %q", got, "(type to search)")
	}
}

// Typing a search has to leave the cursor on the first match. Parking it on
// ".." meant typing a directory name and pressing enter walked *up*.
func TestPickerSearchLandsOnTheFirstMatch(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"cmd", "docs", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := newPicker(root)
	if p.entries[0] != parentEntry || p.cursor != 0 {
		t.Fatalf("fresh picker = %v cursor %d", p.entries, p.cursor)
	}
	p.searchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("cmd")})
	if p.cursor != 1 || p.entries[p.cursor] != "cmd" {
		t.Fatalf("after typing: entries %v cursor %d", p.entries, p.cursor)
	}
	p.enter()
	if p.dir != filepath.Join(root, "cmd") {
		t.Fatalf("enter went to %q", p.dir)
	}
	if p.search.value() != "" {
		t.Errorf("search %q survived the descent", p.search.value())
	}
	p.parent()
	if p.dir != root {
		t.Fatalf("parent went to %q, want %q", p.dir, root)
	}
	p.toggleHidden()
	if !contains(p.entries, ".hidden") {
		t.Errorf("hidden toggle left entries %v", p.entries)
	}
}

// AC4: ".." is prepended by the caller and never searched away — a query that
// matches no directory still leaves the parent entry, cursor parked on it.
func TestPickerKeepsParentWhenNothingMatches(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"cmd", "docs"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := newPicker(root)
	p.searchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("zzzz")})
	if len(p.entries) != 1 || p.entries[0] != parentEntry || p.cursor != 0 {
		t.Fatalf("no-match picker = %v cursor %d, want [..] cursor 0", p.entries, p.cursor)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// The picker's viewport: a directory with more entries than the modal has
// rows scrolls under the cursor instead of overflowing the popup. Off-by-one
// here is a list that hides its last entry or shows one that isn't there.
func TestPickerWindowScrollsUnderTheCursor(t *testing.T) {
	p := &picker{entries: []string{"..", "a", "b", "c", "d", "e"}} // 6 entries
	cases := []struct {
		name   string
		cursor int
		rows   int
		top    int
		window []string
	}{
		{"everything fits", 0, 6, 0, []string{"..", "a", "b", "c", "d", "e"}},
		{"more rows than entries", 3, 10, 0, []string{"..", "a", "b", "c", "d", "e"}},
		{"cursor inside the first window", 2, 3, 0, []string{"..", "a", "b"}},
		{"cursor on the last visible row", 2, 3, 0, []string{"..", "a", "b"}},
		{"cursor past the window: scrolls by one", 3, 3, 1, []string{"a", "b", "c"}},
		{"cursor at the end", 5, 3, 3, []string{"c", "d", "e"}},
		{"no rows to draw", 5, 0, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p.cursor = c.cursor
			if got := p.top(c.rows); got != c.top {
				t.Errorf("top(%d) = %d, want %d", c.rows, got, c.top)
			}
			got := p.window(c.rows)
			if strings.Join(got, ",") != strings.Join(c.window, ",") {
				t.Errorf("window(%d) = %v, want %v", c.rows, got, c.window)
			}
			// the cursor is always inside the window it just chose
			if len(got) > 0 {
				if i := p.cursor - p.top(c.rows); i < 0 || i >= len(got) {
					t.Errorf("cursor %d falls outside the window %v", p.cursor, got)
				}
			}
		})
	}
}
