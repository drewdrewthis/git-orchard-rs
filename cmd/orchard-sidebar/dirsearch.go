package main

import (
	"sort"
	"strings"

	"github.com/reinhrst/fzf-lib/algo"
	"github.com/reinhrst/fzf-lib/util"
)

// The fuzzy search over the walked candidate paths (dirwalk.go). One query runs
// against every candidate's full absolute path, so a few scattered characters
// reach a directory anywhere in the tree — the fzf feel the modal is after,
// rather than a level-by-level browse.

// span is a [start, end) run of rune offsets into a path, the matched
// characters the view highlights.
type span struct{ start, end int }

// dirMatch is one candidate a query kept: its path, fzf's score, and the runs
// of matched characters. An empty-query result carries score 0 and no spans.
type dirMatch struct {
	path  string
	score int
	spans []span
}

// searchPaths ranks the candidates against query. A non-subsequence candidate
// drops out; the rest sort by fzf score (best first), then by shorter path so a
// top-level worktree beats a deeper directory that matched the same letters. An
// empty query returns the candidates untouched, in the order given — the caller
// has already ordered them (recents, then roots).
func searchPaths(cands []string, query string) []dirMatch {
	if query == "" {
		out := make([]dirMatch, len(cands))
		for i, c := range cands {
			out[i] = dirMatch{path: c}
		}
		return out
	}
	// FuzzyMatchV2's caseSensitive=false lowercases the haystack but not the
	// needle, so an upper-case query would otherwise match nothing.
	pat := []rune(strings.ToLower(query))
	var hits []dirMatch
	for _, c := range cands {
		chars := util.ToChars([]byte(c))
		res, pos := algo.FuzzyMatchV2(false, true, true, &chars, pat, true, nil)
		if res.Start < 0 {
			continue // -1 start is fzf's "not a subsequence"
		}
		hits = append(hits, dirMatch{path: c, score: res.Score, spans: matchSpans(pos)})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return len(hits[i].path) < len(hits[j].path)
	})
	return hits
}

// matchSpans folds fzf's matched rune positions into contiguous [start, end)
// runs, so the view underlines "orsi" as at most a couple of segments rather
// than four separate cells.
func matchSpans(pos *[]int) []span {
	if pos == nil || len(*pos) == 0 {
		return nil
	}
	p := append([]int(nil), (*pos)...)
	sort.Ints(p)
	var spans []span
	start, prev := p[0], p[0]
	for _, i := range p[1:] {
		if i == prev+1 {
			prev = i
			continue
		}
		spans = append(spans, span{start, prev + 1})
		start, prev = i, i
	}
	return append(spans, span{start, prev + 1})
}
