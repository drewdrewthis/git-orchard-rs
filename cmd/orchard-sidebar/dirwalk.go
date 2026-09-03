package main

import (
	"os"
	"path/filepath"
	"strings"
)

// The launch modal walks a bounded tree of candidate directories once, when it
// opens, so every keystroke afterwards is pure in-memory matching (dirsearch.go)
// rather than a fresh filesystem walk. The walk is depth- and count-bounded so
// a large $HOME cannot stall the modal: the goroutine that runs it is behind a
// spinner (dirpick.go), and these bounds cap its worst case.

const (
	maxWalkDepth  = 3    // root at depth 0, so three levels of descent below it
	maxCandidates = 5000 // hard cap: a huge tree yields a truncated set, never a hang
)

// skipDirNames are the directories a source tree fills with entries no one
// launches a session into — a git object store, a dependency dump, a build
// output — pruned wholesale so the candidate set stays about workspaces.
var skipDirNames = map[string]bool{
	".git":         true,
	"node_modules": true,
	"target":       true,
}

// walkConfig is one walk's inputs. Zero maxDepth/cap fall back to the constants,
// so a caller only names the roots and the hidden toggle.
type walkConfig struct {
	roots      []string
	maxDepth   int
	cap        int
	showHidden bool
}

func (c walkConfig) depth() int {
	if c.maxDepth <= 0 {
		return maxWalkDepth
	}
	return c.maxDepth
}

func (c walkConfig) capN() int {
	if c.cap <= 0 {
		return maxCandidates
	}
	return c.cap
}

// resolveRoots picks the directories the walk starts from: the selected
// session's cwd, the shared parent of the known session cwds (so a search
// reaches sibling worktrees, not only the one selected), and $HOME as the
// backstop. Non-existent and duplicate roots drop out, so with no selection the
// roots are simply the known-cwd parent plus $HOME.
func resolveRoots(selected string, known []string) []string {
	var roots []string
	add := func(dir string) {
		if dir == "" {
			return
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return
		}
		for _, r := range roots {
			if r == abs {
				return
			}
		}
		roots = append(roots, abs)
	}
	add(selected)
	add(commonParent(known))
	add(os.Getenv("HOME"))
	return roots
}

// commonParent is the deepest directory that contains every path given. One
// path yields its own parent directory; no paths yields "".
func commonParent(paths []string) string {
	var abs []string
	for _, p := range paths {
		if a, err := filepath.Abs(p); err == nil {
			abs = append(abs, filepath.Clean(a))
		}
	}
	if len(abs) == 0 {
		return ""
	}
	if len(abs) == 1 {
		return filepath.Dir(abs[0])
	}
	sep := string(os.PathSeparator)
	parts := strings.Split(abs[0], sep)
	for _, p := range abs[1:] {
		seg := strings.Split(p, sep)
		n := min(len(parts), len(seg))
		i := 0
		for i < n && parts[i] == seg[i] {
			i++
		}
		parts = parts[:i]
	}
	cp := strings.Join(parts, sep)
	if cp == "" {
		return sep
	}
	return cp
}

// walkCandidates returns the absolute directory paths under the roots, the root
// itself included, bounded by depth and count and pruned of the skip list and
// (unless asked) hidden directories. Duplicates across overlapping roots — a
// selected cwd nested under $HOME — are collapsed.
func walkCandidates(cfg walkConfig) []string {
	seen := map[string]bool{}
	var out []string
	var visit func(dir string, depth int)
	visit = func(dir string, depth int) {
		if len(out) >= cfg.capN() || seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir)
		if depth >= cfg.depth() {
			return
		}
		for _, name := range readDirNames(dir) {
			if len(out) >= cfg.capN() {
				return
			}
			if skipDirNames[name] {
				continue
			}
			if !cfg.showHidden && strings.HasPrefix(name, ".") {
				continue
			}
			visit(filepath.Join(dir, name), depth+1)
		}
	}
	for _, r := range cfg.roots {
		visit(r, 0)
	}
	return out
}

// readDirNames returns the sub-directory names of dir (symlinked directories
// included — a worktree tree is full of them). An unreadable directory yields
// nothing rather than an error: the walk simply has one fewer branch.
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

// dedupPaths preserves order and drops the second and later sighting of a path.
func dedupPaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := paths[:0:0]
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
