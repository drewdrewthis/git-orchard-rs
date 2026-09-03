package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/reinhrst/fzf-lib/algo"
	"github.com/reinhrst/fzf-lib/util"
)

// The launch modal's directory picker. Everything here is a pure function of a
// directory listing plus the keys pressed, so the navigation rules can be
// tested without a filesystem walk in the test.

const parentEntry = ".."

// searchDirs hides entries and orders the rest. Hidden entries are out unless
// asked for. Everything else is sorted case-insensitive alphabetically first
// — so score ties keep that order — then, with a query, re-ranked by fzf's
// real match score (best first): a contiguous or prefix match outranks a
// scattered one, so "ocs" puts "ocs-tools" above "orchard-codex-scripts". The
// parent entry is prepended by the caller so it is never searched away: you
// can always go back up, whatever you have typed.
func searchDirs(names []string, showHidden bool, query string) []string {
	visible := make([]string, 0, len(names))
	for _, n := range names {
		if !showHidden && strings.HasPrefix(n, ".") {
			continue
		}
		visible = append(visible, n)
	}
	sort.Slice(visible, func(i, j int) bool {
		return strings.ToLower(visible[i]) < strings.ToLower(visible[j])
	})
	if query == "" {
		return visible
	}

	// FuzzyMatchV2's caseSensitive=false still expects a lowercased pattern —
	// it lowercases the haystack but not the needle, so "DOC" would otherwise
	// match nothing.
	pat := []rune(strings.ToLower(query))
	type hit struct {
		name  string
		score int
	}
	hits := make([]hit, 0, len(visible))
	for _, n := range visible {
		chars := util.ToChars([]byte(n))
		res, _ := algo.FuzzyMatchV2(false, true, true, &chars, pat, false, nil)
		if res.Start >= 0 {
			hits = append(hits, hit{n, res.Score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.name
	}
	return out
}

// readDirNames returns the sub-directory names of dir (symlinked directories
// included — a worktree tree is full of them). An unreadable directory yields
// nothing rather than an error: the picker still has ".." to escape with.
func readDirNames(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && fi.IsDir() {
				out = append(out, e.Name())
			}
		}
	}
	return out
}

// picker is the browsing half of the launch modal.
type picker struct {
	dir     string
	all     []string // every sub-directory, unfiltered
	entries []string // ".." plus what the search and hidden toggle leave
	cursor  int
	search  textField
	hidden  bool
}

// searchWidth is the visible width of the search field; a directory name
// longer than this scrolls under the cursor.
const searchWidth = 40

func newPicker(dir string) *picker {
	p := &picker{dir: cleanDir(dir)}
	p.search = newTextField("", searchWidth)
	p.search.placeholder("(type to search)")
	p.load()
	return p
}

// cleanDir resolves dir to an absolute, existing directory, falling back to
// $HOME and then the working directory. The picker must always have somewhere
// to stand — a cwd that has since been deleted is a normal thing to inherit
// from a session whose worktree was removed.
func cleanDir(dir string) string {
	for _, c := range []string{dir, os.Getenv("HOME")} {
		if c == "" {
			continue
		}
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			return abs
		}
	}
	wd, _ := os.Getwd()
	return wd
}

func (p *picker) load() {
	p.all = readDirNames(p.dir)
	p.rebuild()
}

// rebuild recomputes the visible list and keeps the cursor in range. It is
// called on every keystroke that changes the search, so it must be cheap: no
// filesystem access, just the cached listing.
func (p *picker) rebuild() {
	p.entries = append([]string{parentEntry}, searchDirs(p.all, p.hidden, p.search.value())...)
	if p.cursor >= len(p.entries) {
		p.cursor = len(p.entries) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

func (p *picker) move(d int) {
	p.cursor += d
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor >= len(p.entries) {
		p.cursor = len(p.entries) - 1
	}
}

// enter descends into the highlighted entry (or climbs, on ".."). Moving
// clears the search: it described the directory you were in, not this one.
func (p *picker) enter() {
	if p.cursor < 0 || p.cursor >= len(p.entries) {
		return
	}
	if p.entries[p.cursor] == parentEntry {
		p.parent()
		return
	}
	p.goTo(filepath.Join(p.dir, p.entries[p.cursor]))
}

func (p *picker) parent() {
	if parent := filepath.Dir(p.dir); parent != p.dir {
		p.goTo(parent)
	}
}

func (p *picker) goTo(dir string) {
	p.dir, p.cursor = dir, 0
	p.search.set("")
	p.load()
}

func (p *picker) toggleHidden() {
	p.hidden = !p.hidden
	p.rebuild()
}

// searchKey hands one key to the search field and re-derives the list. The
// field is a textField like every other input here, so a coalesced burst of
// keystrokes (or a paste) lands whole — reading one rune per message silently
// swallowed most of what was typed.
func (p *picker) searchKey(msg tea.KeyMsg) {
	before := p.search.value()
	p.search.key(msg)
	if p.search.value() == before {
		return
	}
	p.rebuild()
	p.restCursor()
}

// searchView renders the field, placeholder included.
func (p *picker) searchView(w int) string { return p.search.view(w) }

// backspaceSearch deletes the last search character, reporting false when
// there was nothing to delete — the caller then reads the backspace as "go up
// a directory" instead.
func (p *picker) backspaceSearch() bool {
	if p.search.value() == "" {
		return false
	}
	p.searchKey(tea.KeyMsg{Type: tea.KeyBackspace})
	return true
}

// window returns the visible slice of entries, and top the index it starts at,
// so a long directory scrolls under the cursor instead of overflowing.
func (p *picker) window(n int) []string {
	if n <= 0 {
		return nil
	}
	top := p.top(n)
	end := top + n
	if end > len(p.entries) {
		end = len(p.entries)
	}
	return p.entries[top:end]
}

func (p *picker) top(n int) int {
	if n <= 0 || p.cursor < n {
		return 0
	}
	return p.cursor - n + 1
}

// restCursor parks the cursor where the next Enter should go. With a search
// typed, that is the first match, not ".." — typing "cmd" and pressing enter
// has to descend into cmd/, not climb to the parent.
func (p *picker) restCursor() {
	p.cursor = 0
	if p.search.value() != "" && len(p.entries) > 1 {
		p.cursor = 1
	}
}
