package resolvers

// Test for the cwd-normalization fix reviewed on #743: on macOS ps-derived
// cwds (lsof) resolve symlinks (/private/tmp/x) while Claude Code's session
// registry keeps the launch path (/tmp/x) — a raw string compare falsely
// rejects a legitimate pid-reuse-guard match.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSameDir_SymlinkAndRealPathMatch(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if !sameDir(link, real) {
		t.Fatalf("sameDir(%q, %q) = false, want true", link, real)
	}
}

func TestSameDir_DifferentDirsMismatch(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()

	if sameDir(a, b) {
		t.Fatalf("sameDir(%q, %q) = true, want false", a, b)
	}
}
