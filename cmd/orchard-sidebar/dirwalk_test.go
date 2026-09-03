package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The candidate walk that feeds the fuzzy picker: what it prunes, how deep it
// goes, and how it picks its roots.

func mkdirs(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, r := range rels {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(r)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func hasPath(paths []string, p string) bool {
	for _, x := range paths {
		if x == p {
			return true
		}
	}
	return false
}

// AC5: the walk drops the skip list and hidden dirs, stops at depth 3, and
// honours the cap.
func TestWalkCandidatesSkipsAndBounds(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root,
		"a/b/c",   // depth 3 — kept
		"a/b/c/d", // depth 4 — beyond the limit
		".git/objects",
		"node_modules/pkg",
		"target/debug",
		".hidden",
		"visible",
	)

	got := walkCandidates(walkConfig{roots: []string{root}})

	if !hasPath(got, root) {
		t.Errorf("walk missing the root %q", root)
	}
	for _, in := range []string{"a", "a/b", "a/b/c", "visible"} {
		if p := filepath.Join(root, filepath.FromSlash(in)); !hasPath(got, p) {
			t.Errorf("walk missing %q", p)
		}
	}
	for _, out := range []string{"a/b/c/d", ".git", ".git/objects", "node_modules", "target", ".hidden"} {
		if p := filepath.Join(root, filepath.FromSlash(out)); hasPath(got, p) {
			t.Errorf("walk should have pruned %q", p)
		}
	}

	// hidden toggle on reveals the hidden dir (still inside the depth limit)
	shown := walkCandidates(walkConfig{roots: []string{root}, showHidden: true})
	if !hasPath(shown, filepath.Join(root, ".hidden")) {
		t.Errorf("showHidden did not reveal .hidden: %v", shown)
	}

	// the cap is a hard ceiling on the count
	capped := walkCandidates(walkConfig{roots: []string{root}, cap: 3})
	if len(capped) > 3 {
		t.Errorf("cap=3 yielded %d entries", len(capped))
	}
}

// The cap bounds descendants, not roots: a root must never be dropped just
// because an earlier root's children already filled the cap.
func TestWalkCandidatesCapNeverDropsRoots(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()
	mkdirs(t, root1, "a", "b", "c")

	got := walkCandidates(walkConfig{roots: []string{root1, root2}, cap: 1})

	if !hasPath(got, root1) {
		t.Errorf("walk dropped root %q under cap=1", root1)
	}
	if !hasPath(got, root2) {
		t.Errorf("walk dropped root %q under cap=1", root2)
	}
}

// AC7: with no session selected, the roots are the shared parent of the known
// cwds plus $HOME, and a walk over them still reaches the fuzzy target.
func TestResolveRootsNoSelection(t *testing.T) {
	tmp := t.TempDir()
	mkdirs(t, tmp,
		"ws/p1", "ws/p2",
		"ws/git-orchard-rs/cmd/orchard-sidebar",
		"home",
	)
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)

	known := []string{filepath.Join(tmp, "ws/p1"), filepath.Join(tmp, "ws/p2")}
	roots := resolveRoots("", known)

	want := []string{filepath.Join(tmp, "ws"), home}
	if len(roots) != 2 || roots[0] != want[0] || roots[1] != want[1] {
		t.Fatalf("roots = %v, want %v", roots, want)
	}

	// and orsi still finds the target under the resolved roots
	cands := walkCandidates(walkConfig{roots: roots})
	hits := searchPaths(cands, "orsi")
	target := filepath.Join(tmp, "ws/git-orchard-rs/cmd/orchard-sidebar")
	if len(hits) == 0 || hits[0].path != target {
		t.Fatalf("orsi ranked %v, want %q first", firstPaths(hits, 3), target)
	}
}

func TestCommonParent(t *testing.T) {
	sep := string(os.PathSeparator)
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"/a/b/p1", "/a/b/p2"}, "/a/b"},
		{[]string{"/a/b/c", "/a/x/y"}, "/a"},
		{[]string{"/a/b/only"}, "/a/b"},
		{nil, ""},
	}
	for _, c := range cases {
		in := make([]string, len(c.in))
		for i, p := range c.in {
			in[i] = filepath.FromSlash(p)
		}
		if got := commonParent(in); got != filepath.FromSlash(c.want) && !(c.want == "" && got == "") {
			t.Errorf("commonParent(%v) = %q, want %q (sep %q)", c.in, got, c.want, sep)
		}
	}
}

// A symlink loop (a/b/loop -> a) must not send the walk descending forever —
// it should stop at the loop entry and still surface the legitimate dirs.
func TestWalkCandidatesSkipsSymlinkCycle(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a/b", "a/c")
	loop := filepath.Join(root, "a/b/loop")
	if err := os.Symlink(filepath.Join(root, "a"), loop); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	got := walkCandidates(walkConfig{roots: []string{root}, maxDepth: 6})

	for _, x := range got {
		if strings.HasPrefix(x, loop+string(os.PathSeparator)) {
			t.Errorf("walk descended into the symlink loop: %q", x)
		}
	}
	for _, in := range []string{"a", "a/b", "a/c"} {
		if p := filepath.Join(root, filepath.FromSlash(in)); !hasPath(got, p) {
			t.Errorf("walk missing legitimate dir %q (got %v)", p, got)
		}
	}
}

func firstPaths(hits []dirMatch, n int) []string {
	var out []string
	for i, h := range hits {
		if i >= n {
			break
		}
		out = append(out, h.path)
	}
	return out
}

// AC4: with hidden dirs walked, a hidden dir that is a symlink back to an
// ancestor is caught by the same cycle guard — no infinite descent, no dup.
func TestWalkCandidatesHiddenSymlinkCycle(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a/b")
	loop := filepath.Join(root, "a", ".loop")
	if err := os.Symlink(filepath.Join(root, "a"), loop); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	got := walkCandidates(walkConfig{roots: []string{root}, showHidden: true, maxDepth: 6})

	for _, x := range got {
		if strings.HasPrefix(x, loop+string(os.PathSeparator)) {
			t.Errorf("hidden walk descended into the symlink loop: %q", x)
		}
	}
	seen := map[string]bool{}
	for _, x := range got {
		if seen[x] {
			t.Errorf("duplicate path in walk output: %q", x)
		}
		seen[x] = true
	}
	for _, in := range []string{"a", "a/b"} {
		if p := filepath.Join(root, filepath.FromSlash(in)); !hasPath(got, p) {
			t.Errorf("hidden walk missing legitimate dir %q", p)
		}
	}
}
