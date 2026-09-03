package main

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The launch modal's directory picker. It is no longer a level-by-level
// browser: a background walk (dirwalk.go) gathers every candidate directory
// under a few roots once, and each keystroke fuzzy-searches the whole set
// (dirsearch.go). Enter picks the highlighted match as the launch directory —
// there is no descend/ascend, only a query. Backspace on an empty query widens
// the roots as the escape hatch.

// searchWidth is the visible width of the search field; a longer query scrolls
// under the cursor.
const searchWidth = 40

// walkDoneMsg carries the finished candidate set back to the update loop, so
// the walk runs off the main goroutine and typing stays responsive while it
// does. gen ties the result to the walk that produced it, so a widen/toggle
// fired while an older walk is still in flight can tell its now-stale result
// apart from the one that matches the current roots.
type walkDoneMsg struct {
	gen   int
	cands []string
}

// picker is the browsing half of the launch modal.
type picker struct {
	roots   []string   // walk roots; also the empty-query tail
	recents []string   // recent launch dirs; the empty-query head
	matches []dirMatch // the current ranked view
	cursor  int
	search  textField
	mode    hiddenMode
	walking bool
	walkGen int // bumped each time a new walk is kicked off; see walkDoneMsg
	spin    spinner.Model
	cfg     walkConfig

	// The walked candidate sets, one per hidden state, cached so a flip back
	// to a state already walked reuses it instead of re-walking. Both nil until
	// their walk lands; a widen (roots change) invalidates both.
	plainCands  []string // hidden-off walk
	hiddenCands []string // hidden-on walk
}

func newPicker(selected string, known []string) *picker {
	roots := resolveRoots(selected, known)
	p := &picker{
		roots:   roots,
		recents: existingRecents(),
		cfg:     walkConfig{roots: roots},
		walking: true,
		spin:    spinner.New(spinner.WithSpinner(spinner.Dot)),
	}
	p.search = newTextField("", searchWidth)
	p.search.placeholder("(type to search)")
	p.rebuild() // show recents+roots immediately, before the walk lands
	return p
}

// walkCmd runs the filesystem walk off the update loop. The modal keeps
// painting — the spinner, the recents — until walkDoneMsg replaces the set.
// It captures the current generation, so a result from a walk superseded by a
// later widen/toggle can be told apart from the one that matches p.roots now.
func (p *picker) walkCmd() tea.Cmd {
	cfg := p.cfg
	gen := p.walkGen
	return func() tea.Msg { return walkDoneMsg{gen: gen, cands: walkCandidates(cfg)} }
}

// setCands caches the walked set under the hidden state it was walked with
// (read from cfg, which the matching generation still reflects) and re-derives
// the visible list — unless gen names a walk that a later widen/toggle has since
// superseded, in which case the stale result is dropped and p.walking is left
// alone so the in-flight walk's spinner keeps spinning.
func (p *picker) setCands(gen int, cands []string) {
	if gen != p.walkGen {
		return
	}
	if p.cfg.showHidden {
		p.hiddenCands = cands
	} else {
		p.plainCands = cands
	}
	p.walking = false
	p.rebuild()
}

// pool is the set a typed query searches: recents first (so a recent launch dir
// is always reachable), then the walked candidates — or, while the walk is
// still running, just the roots, so typing is never dead.
func (p *picker) pool() []string {
	base := p.activeCands()
	if base == nil {
		base = p.roots
	}
	return dedupPaths(append(append([]string{}, p.recents...), base...))
}

// emptyOrder is the empty-query list: persisted recents first, then the roots.
func emptyOrder(recents, roots []string) []string {
	return dedupPaths(append(append([]string{}, recents...), roots...))
}

// rebuild recomputes the visible list. Cheap by design — no filesystem access,
// only the cached candidate set — because it runs on every keystroke.
func (p *picker) rebuild() {
	if q := p.search.value(); q != "" {
		p.matches = searchPaths(p.pool(), q)
	} else {
		p.matches = searchPaths(emptyOrder(p.recents, p.roots), "")
	}
	p.clampCursor()
}

func (p *picker) clampCursor() {
	if p.cursor >= len(p.matches) {
		p.cursor = len(p.matches) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

func (p *picker) move(d int) {
	p.cursor += d
	p.clampCursor()
}

// dir is the directory a launch would use: the highlighted match, or the first
// root when the query matched nothing.
func (p *picker) dir() string {
	if p.cursor >= 0 && p.cursor < len(p.matches) {
		return p.matches[p.cursor].path
	}
	if len(p.roots) > 0 {
		return p.roots[0]
	}
	return ""
}

// widen is the escape hatch: it adds each root's parent as a new root and
// re-walks, so a query that reaches nothing under the current roots can search
// a level up. It returns the re-walk command, or nil when there is nothing
// higher to add.
func (p *picker) widen() tea.Cmd {
	var next []string
	for _, r := range p.roots {
		next = append(next, r)
		if parent := filepath.Dir(r); parent != r {
			next = append(next, parent)
		}
	}
	next = dedupPaths(next)
	if len(next) == len(p.roots) {
		return nil
	}
	p.roots = next
	p.cfg.roots = next
	p.cfg.showHidden = effectiveHidden(p.mode, p.search.value()) // cfg.showHidden can be stale in auto mode; re-derive before the re-walk
	p.plainCands, p.hiddenCands = nil, nil                       // the roots moved; both caches are stale
	p.walking = true
	p.walkGen++
	p.rebuild()
	return p.walkCmd()
}

// searchKey hands one key to the search field and re-derives the list, returning
// a re-walk command when the new query crossed the dot-mode boundary (auto). The
// field is a textField, so a coalesced burst or a paste lands whole.
func (p *picker) searchKey(msg tea.KeyMsg) tea.Cmd {
	before := p.search.value()
	p.search.key(msg)
	if p.search.value() == before {
		return nil
	}
	p.cursor = 0 // a new query re-ranks from the top
	return p.syncHidden()
}

// backspaceSearch deletes the last search character, reporting false when the
// query was already empty — the caller then reads the backspace as "widen the
// roots" instead. A non-nil command means the deletion crossed the dot-mode
// boundary and a re-walk is needed.
func (p *picker) backspaceSearch() (bool, tea.Cmd) {
	if p.search.value() == "" {
		return false, nil
	}
	return true, p.searchKey(tea.KeyMsg{Type: tea.KeyBackspace})
}

func (p *picker) searchView(w int) string { return p.search.view(w) }

// window returns the visible slice of matches and top the index it starts at,
// so a long result list scrolls under the cursor instead of overflowing.
func (p *picker) window(n int) []dirMatch {
	if n <= 0 {
		return nil
	}
	top := p.top(n)
	end := min(top+n, len(p.matches))
	return p.matches[top:end]
}

func (p *picker) top(n int) int {
	if n <= 0 || p.cursor < n {
		return 0
	}
	return p.cursor - n + 1
}

// abbrevHome shortens a $HOME-rooted path to "~/…" and shifts the highlight
// spans left by the runes it collapsed, so the underline still lands on the
// right runes. spans are rune-indexed (matchSpans reads fzf's match positions
// as rune offsets), so the shift must be counted in runes too — path[len(home):]
// stays a byte slice (home is confirmed a byte-exact prefix above, so a byte
// offset is the correct way to strip it), only delta needs the rune count.
func abbrevHome(path string, spans []span) (string, []span) {
	home := os.Getenv("HOME")
	if home == "" || (path != home && !strings.HasPrefix(path, home+"/")) {
		return path, spans
	}
	delta := utf8.RuneCountInString(home) - 1 // the home runes become a single "~"
	short := "~" + path[len(home):]
	var out []span
	for _, s := range spans {
		st, en := s.start-delta, s.end-delta
		if en <= 1 {
			continue // wholly inside the collapsed prefix
		}
		if st < 1 {
			st = 1 // clamp past the "~"
		}
		out = append(out, span{st, en})
	}
	return short, out
}

// renderMatch draws one path, its matched runes wearing hi and everything else
// wearing base, clipped to width.
func renderMatch(m dirMatch, base, hi lipgloss.Style, width int) string {
	disp, spans := abbrevHome(m.path, m.spans)
	if len(spans) == 0 {
		return base.Render(trunc(disp, width))
	}
	r := []rune(disp)
	var b strings.Builder
	i := 0
	for _, s := range spans {
		if s.start >= len(r) {
			break
		}
		end := min(s.end, len(r))
		if s.start > i {
			b.WriteString(base.Render(string(r[i:s.start])))
		}
		b.WriteString(hi.Render(string(r[s.start:end])))
		i = end
	}
	if i < len(r) {
		b.WriteString(base.Render(string(r[i:])))
	}
	return trunc(b.String(), width)
}
