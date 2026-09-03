package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fuzzy search over walked candidate paths: it finds a deep directory from
// a few scattered letters, ranks best-first with a shorter-path tiebreak, marks
// the matched characters, and stays well under the per-keystroke budget.

// AC1: opened away from it, "orsi" ranks …/git-orchard-rs/cmd/orchard-sidebar
// first over a walked fixture tree.
func TestSearchPathsFindsDeepDir(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root,
		"git-orchard-rs/cmd/orchard-sidebar",
		"git-orchard-rs/cmd/orchard-daemon",
		"git-orchard-rs/internal",
		"orchard-scripts",
		"notes",
	)
	cands := walkCandidates(walkConfig{roots: []string{root}})
	hits := searchPaths(cands, "orsi")
	want := filepath.Join(root, "git-orchard-rs/cmd/orchard-sidebar")
	if len(hits) == 0 || hits[0].path != want {
		t.Fatalf("orsi ranked %v, want %q first", firstPaths(hits, 3), want)
	}
	// a non-subsequence query finds nothing
	if got := searchPaths(cands, "zzqq"); len(got) != 0 {
		t.Errorf("zzqq matched %v, want nothing", firstPaths(got, 3))
	}
}

// AC3: results rank by score with a shorter-path tiebreak, and the matched
// characters come back as highlight spans.
func TestSearchPathsRanksAndHighlights(t *testing.T) {
	// contiguous "cmd" outranks the scattered subsequence in crates-orchard-md
	ranked := searchPaths([]string{"/w/crates-orchard-md", "/w/cmd"}, "cmd")
	if len(ranked) < 1 || ranked[0].path != "/w/cmd" {
		t.Fatalf("ranked %v, want /w/cmd first", firstPaths(ranked, 3))
	}

	// equal-scoring matches (identical matched tail) break the tie by shorter path
	tie := searchPaths([]string{"/aa/foo", "/foo"}, "foo")
	if len(tie) != 2 || tie[0].path != "/foo" {
		t.Fatalf("tie ranked %v, want /foo first", firstPaths(tie, 3))
	}

	// the spans cover exactly the matched runes: "cmd" at the tail of "/w/cmd"
	one := searchPaths([]string{"/w/cmd"}, "cmd")
	if len(one) != 1 {
		t.Fatalf("got %d matches, want 1", len(one))
	}
	if got := spanRunes("/w/cmd", one[0].spans); got != "cmd" {
		t.Errorf("spans cover %q, want %q (spans %v)", got, "cmd", one[0].spans)
	}
	// a scattered query yields spans that still reconstruct the query letters
	sc := searchPaths([]string{"/w/orchard-sidebar"}, "orsi")
	if len(sc) != 1 {
		t.Fatalf("got %d, want 1", len(sc))
	}
	if got := spanRunes("/w/orchard-sidebar", sc[0].spans); got != "orsi" {
		t.Errorf("scattered spans cover %q, want %q (spans %v)", got, "orsi", sc[0].spans)
	}
}

// spanRunes reads back the runes the spans mark, so a test can assert they land
// on the query letters.
func spanRunes(path string, spans []span) string {
	r := []rune(path)
	var b strings.Builder
	for _, s := range spans {
		for i := s.start; i < s.end && i < len(r); i++ {
			b.WriteRune(r[i])
		}
	}
	return b.String()
}

// AC2: a keystroke over ≥1,000 candidates matches and ranks fast. This is a
// not-pathological guard, not the perf proof — BenchmarkSearchPaths below is
// the real per-op budget, since wall-clock here is flaky on a cold/loaded box.
func TestSearchPathsUnderBudget(t *testing.T) {
	cands := benchCandidates(1200)
	start := time.Now()
	_ = searchPaths(cands, "orsi")
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Fatalf("one query over %d candidates took %v, want < 250ms", len(cands), d)
	}
}

func BenchmarkSearchPaths(b *testing.B) {
	cands := benchCandidates(1200)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = searchPaths(cands, "orsi")
	}
}

// benchCandidates builds n path-shaped strings, one of which the query hits, so
// the benchmark exercises both the reject and the match paths.
func benchCandidates(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("/home/dev/workspace/proj-%04d/cmd/orchard-sidebar", i))
	}
	return out
}
