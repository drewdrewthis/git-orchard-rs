package release

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// RevisionBinaries and UnstampedBinaries must partition SuiteBinaries exactly:
// their union is SuiteBinaries with no overlap. This is the anti-drift guard —
// a Go binary added to SuiteBinaries lands in RevisionBinaries automatically,
// and no binary is both covered and excluded.
func TestRevisionAndUnstampedPartitionSuite(t *testing.T) {
	union := append(slices.Clone(RevisionBinaries), UnstampedBinaries...)
	slices.Sort(union)
	want := slices.Clone(SuiteBinaries)
	slices.Sort(want)
	if !slices.Equal(union, want) {
		t.Errorf("RevisionBinaries ∪ UnstampedBinaries = %v; want SuiteBinaries %v", union, want)
	}
	for _, name := range RevisionBinaries {
		if slices.Contains(UnstampedBinaries, name) {
			t.Errorf("%q is in both RevisionBinaries and UnstampedBinaries", name)
		}
	}
}

// The -ldflags override wins over the build-info fallback, so a release build
// reports the commit stamped into it (orchardist#803).
func TestRevisionOverrideWins(t *testing.T) {
	old := revision
	t.Cleanup(func() { revision = old })

	revision = "deadbeef"
	if got := Revision(); got != "deadbeef" {
		t.Errorf("Revision() = %q; want the ldflags override %q", got, "deadbeef")
	}
}

// HandleRevisionFlag answers a bare --revision with exactly one line, the bare
// revision, and reports it handled so the caller exits.
func TestHandleRevisionFlag(t *testing.T) {
	old := revision
	t.Cleanup(func() { revision = old })
	revision = "abc123"

	t.Run("handles --revision", func(t *testing.T) {
		var buf bytes.Buffer
		if !HandleRevisionFlag([]string{"--revision"}, &buf) {
			t.Fatal("HandleRevisionFlag returned false for --revision")
		}
		if got := buf.String(); got != "abc123\n" {
			t.Errorf("output = %q; want one bare line %q", got, "abc123\n")
		}
		if lines := strings.Count(buf.String(), "\n"); lines != 1 {
			t.Errorf("output has %d lines; want exactly 1", lines)
		}
	})

	t.Run("ignores other args", func(t *testing.T) {
		var buf bytes.Buffer
		if HandleRevisionFlag([]string{"--version"}, &buf) {
			t.Error("HandleRevisionFlag claimed --version")
		}
		if HandleRevisionFlag(nil, &buf) {
			t.Error("HandleRevisionFlag claimed an empty argv")
		}
		if buf.Len() != 0 {
			t.Errorf("wrote %q for non-matching args; want nothing", buf.String())
		}
	})
}
