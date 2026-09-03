// Tests for the generic per-provider cache. The interesting contract is
// isolation: a value handed to a caller must not be a window onto the
// store's own entry, or a resolver could corrupt the cache by writing
// through it.

package store_test

import (
	"sort"
	"testing"

	provider "github.com/drewdrewthis/orchardist/internal/server/adapter"
	"github.com/drewdrewthis/orchardist/internal/server/store"
)

type row struct {
	Name string
	Tags []string
}

func seeded(t *testing.T) *store.Store[string, row] {
	t.Helper()
	s := store.New[string, row]()
	s.Put("a", row{Name: "alpha", Tags: []string{"x"}}, provider.SourcePoll)
	s.Put("b", row{Name: "bravo"}, provider.SourcePoll)
	s.Put("c", row{Name: "charlie"}, provider.SourcePoll)
	return s
}

func names(rows []row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	sort.Strings(out)
	return out
}

// TestFilter_ReturnsOnlyMatches is the whole point of Filter: a narrow
// read must not pay for the entries it does not want. Snapshot() copies
// all three; Filter copies one.
func TestFilter_ReturnsOnlyMatches(t *testing.T) {
	s := seeded(t)
	got := s.Filter(func(k string, _ row) bool { return k == "b" })
	if len(got) != 1 {
		t.Fatalf("Filter returned %d rows, want 1: %v", len(got), names(got))
	}
	if got[0].Name != "bravo" {
		t.Errorf("Filter returned %q, want bravo", got[0].Name)
	}
}

// TestFilter_MatchesOnValueNotJustKey asserts the predicate sees the
// value too — the tmux accessors filter panes on WindowKey, which is a
// value field, not part of the store key.
func TestFilter_MatchesOnValueNotJustKey(t *testing.T) {
	s := seeded(t)
	got := s.Filter(func(_ string, v row) bool { return len(v.Tags) > 0 })
	if len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("Filter by value = %v, want [alpha]", names(got))
	}
}

// TestFilter_NoMatchesReturnsEmpty asserts a miss is empty rather than a
// panic or a whole-population fallback.
func TestFilter_NoMatchesReturnsEmpty(t *testing.T) {
	s := seeded(t)
	if got := s.Filter(func(string, row) bool { return false }); len(got) != 0 {
		t.Errorf("Filter with no matches = %v, want empty", names(got))
	}
}

// TestFilter_ResultIsIndependentOfStore asserts a caller cannot write
// through the returned slice into the cache — the immutability guarantee
// Snapshot() provided by handing back a fresh map.
func TestFilter_ResultIsIndependentOfStore(t *testing.T) {
	s := seeded(t)
	got := s.Filter(func(k string, _ row) bool { return k == "a" })
	if len(got) != 1 {
		t.Fatalf("Filter returned %d rows, want 1", len(got))
	}
	got[0].Name = "MUTATED"
	got = append(got, row{Name: "APPENDED"})
	_ = got

	again, _, ok := s.Get("a")
	if !ok {
		t.Fatal("Get(a) after mutating the Filter result: entry vanished")
	}
	if again.Name != "alpha" {
		t.Errorf("store entry = %q after caller mutated the Filter result, want alpha", again.Name)
	}
	if n := len(s.Keys()); n != 3 {
		t.Errorf("store holds %d keys after caller appended to the Filter result, want 3", n)
	}
}
