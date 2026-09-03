package release_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

func TestReplace_SwapsContentsAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orchard-shell")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := release.Replace(path, []byte("new"), 0); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("contents = %q; want new", got)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v; want the existing 0755 to survive the replace", st.Mode().Perm())
	}
}

// The temp file must land in the destination's own directory, or os.Rename
// fails with EXDEV across filesystems. Assert nothing is left behind either.
func TestReplace_LeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orchard")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := release.Replace(path, []byte("new"), 0); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "orchard" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v; want only the installed binary", names)
	}
}

func TestReplace_NewFileGets0755(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orchard-upgrade")
	if err := release.Replace(path, []byte("fresh"), 0); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v; want 0755 for a newly created binary", st.Mode().Perm())
	}
}

func TestReplace_ReadOnlyDirectoryIsErrNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not gate root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "orchard")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err := release.Replace(path, []byte("new"), 0)
	if !errors.Is(err, release.ErrNotWritable) {
		t.Fatalf("Replace into a read-only dir = %v; want ErrNotWritable", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Errorf("contents = %q; the original must survive a failed install", got)
	}
}

func TestReplaceAll_InstallsEverySetMember(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"orchard", "orchard-shell"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("v1"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	results, err := release.ReplaceAll([]release.ReplaceItem{
		{Path: filepath.Join(dir, "orchard-shell"), Data: []byte("v2")},
		{Path: filepath.Join(dir, "orchard"), Data: []byte("v2")},
	})
	if err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	for _, n := range []string{"orchard", "orchard-shell"} {
		if got, _ := os.ReadFile(filepath.Join(dir, n)); string(got) != "v2" {
			t.Errorf("%s = %q; want v2", n, got)
		}
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d; want 2", len(results))
	}
	for _, r := range results {
		if r.Action != release.ActionUpdated {
			t.Errorf("%s action = %q; want %q", r.Path, r.Action, release.ActionUpdated)
		}
	}
	assertNoStrayFiles(t, dir, "orchard", "orchard-shell")
}

// A half-applied upgrade is the failure mode ReplaceAll exists to prevent:
// when a later binary cannot be installed, the ones already swapped must go
// back to the version they were.
func TestReplaceAll_RollsBackEveryEarlierMemberOnFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "orchard-shell"), []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory where the second binary should be: creating the temp file
	// succeeds, the rename over a non-empty directory does not.
	blocked := filepath.Join(dir, "orchard")
	if err := os.MkdirAll(filepath.Join(blocked, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}

	results, err := release.ReplaceAll([]release.ReplaceItem{
		{Path: filepath.Join(dir, "orchard-shell"), Data: []byte("v2")},
		{Path: blocked, Data: []byte("v2")},
	})
	if err == nil {
		t.Fatal("ReplaceAll succeeded despite an uninstallable member")
	}
	if results != nil {
		t.Errorf("results = %v; want nil on a rolled-back failure", results)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "orchard-shell")); string(got) != "v1" {
		t.Errorf("orchard-shell = %q; want the original v1 restored by the rollback", got)
	}
	assertNoStrayFiles(t, dir, "orchard", "orchard-shell")
}

func TestReplaceAll_ReportsInstalledForANewFile(t *testing.T) {
	dir := t.TempDir()
	results, err := release.ReplaceAll([]release.ReplaceItem{
		{Path: filepath.Join(dir, "orchard-upgrade"), Data: []byte("v1")},
	})
	if err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	if len(results) != 1 || results[0].Action != release.ActionInstalled {
		t.Fatalf("results = %+v; want one ActionInstalled", results)
	}
}

// The identical-content case is what a real `orchard upgrade` hits every
// time it's re-run without a new release available: every downloaded binary
// matches what's already on disk. ReplaceAll must not touch the file at all
// -- not even a rewrite with the same bytes -- which this proves
// structurally by chmodding the directory read-only first: any write
// attempt (temp file, backup, or otherwise) would fail loudly, so a passing
// test means none was made.
func TestReplaceAll_SkipsByteIdenticalContentWithoutTouchingDisk(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not gate root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "orchard")
	if err := os.WriteFile(path, []byte("same"), 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	results, err := release.ReplaceAll([]release.ReplaceItem{
		{Path: path, Data: []byte("same")},
	})
	if err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	if len(results) != 1 || results[0].Action != release.ActionUnchanged {
		t.Fatalf("results = %+v; want one ActionUnchanged", results)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("mtime changed from %v to %v; unchanged content must not be rewritten", before.ModTime(), after.ModTime())
	}
	if got, _ := os.ReadFile(path); string(got) != "same" {
		t.Errorf("contents = %q; want same", got)
	}
}

// A mixed batch reports each member's own action -- a fresh binary, a
// changed one, and one already current all land in the same ReplaceAll call
// during a real upgrade.
func TestReplaceAll_ReportsPerItemActionsInAMixedBatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "changed"), []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unchanged"), []byte("same"), 0o755); err != nil {
		t.Fatal(err)
	}

	results, err := release.ReplaceAll([]release.ReplaceItem{
		{Path: filepath.Join(dir, "changed"), Data: []byte("v2")},
		{Path: filepath.Join(dir, "unchanged"), Data: []byte("same")},
		{Path: filepath.Join(dir, "new"), Data: []byte("v1")},
	})
	if err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	want := map[string]release.ReplaceAction{
		filepath.Join(dir, "changed"):   release.ActionUpdated,
		filepath.Join(dir, "unchanged"): release.ActionUnchanged,
		filepath.Join(dir, "new"):       release.ActionInstalled,
	}
	if len(results) != len(want) {
		t.Fatalf("len(results) = %d; want %d", len(results), len(want))
	}
	for _, r := range results {
		if want[r.Path] != r.Action {
			t.Errorf("%s action = %q; want %q", r.Path, r.Action, want[r.Path])
		}
	}
}

func TestWritable_ReportsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := release.Writable(dir); err != nil {
		t.Fatalf("Writable(%s) = %v; want nil", dir, err)
	}
	if err := release.Writable(filepath.Join(dir, "nope")); !errors.Is(err, release.ErrNotWritable) {
		t.Fatalf("Writable(missing dir) = %v; want ErrNotWritable", err)
	}
}

func assertNoStrayFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	allowed := map[string]bool{}
	for _, w := range want {
		allowed[w] = true
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !allowed[e.Name()] {
			t.Errorf("stray file %q left in the install dir", e.Name())
		}
	}
}
