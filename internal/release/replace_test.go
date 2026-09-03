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

	err := release.ReplaceAll([]release.ReplaceItem{
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

	err := release.ReplaceAll([]release.ReplaceItem{
		{Path: filepath.Join(dir, "orchard-shell"), Data: []byte("v2")},
		{Path: blocked, Data: []byte("v2")},
	})
	if err == nil {
		t.Fatal("ReplaceAll succeeded despite an uninstallable member")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "orchard-shell")); string(got) != "v1" {
		t.Errorf("orchard-shell = %q; want the original v1 restored by the rollback", got)
	}
	assertNoStrayFiles(t, dir, "orchard", "orchard-shell")
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
