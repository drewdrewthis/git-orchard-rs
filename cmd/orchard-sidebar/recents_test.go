package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The launch modal's memory: recent launch directories, written into the same
// state file the "last command" already lived in, kept backward-compatible with
// the pre-recents single-record shape.

// AC6: with only a pre-recents last-launch file (no recents file yet),
// existingRecents falls back to the prior launch dir; a launch then writes the
// separate recents file, which supersedes the fallback.
func TestExistingRecentsLoadsOldFile(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := t.TempDir() // a real directory so it survives the existence filter

	// the pre-recents shape: only the last-launch file, no recents file
	old := map[string]string{"cmd": "claude", "dir": dir, "name": "x", "at": "2026-01-01T00:00:00Z"}
	b, _ := json.Marshal(old)
	p := lastLaunchPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}

	if l := loadLastLaunch(); l.Dir != dir {
		t.Fatalf("loaded Dir = %q, want %q", l.Dir, dir)
	}
	if got := loadRecents(); got != nil {
		t.Fatalf("recents file absent but loadRecents = %v, want nil", got)
	}
	// the picker's recents fall back to the single prior launch dir
	if got := existingRecents(); len(got) != 1 || got[0] != dir {
		t.Fatalf("existingRecents = %v, want [%q]", got, dir)
	}

	// a launch writes the recents file, which then supersedes the fallback
	next := t.TempDir()
	rememberRecent(next)
	if got := loadRecents(); len(got) != 1 || got[0] != next {
		t.Fatalf("reloaded recents = %v, want [%q]", got, next)
	}
	if got := existingRecents(); len(got) != 1 || got[0] != next {
		t.Fatalf("existingRecents after a launch = %v, want [%q]", got, next)
	}
}

func TestAddRecent(t *testing.T) {
	// prepend, most-recent first
	if got := addRecent([]string{"/b"}, "/a", 8); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Errorf("addRecent = %v, want [/a /b]", got)
	}
	// re-launching an existing dir moves it to the front, no duplicate
	if got := addRecent([]string{"/a", "/b", "/c"}, "/b", 8); len(got) != 3 || got[0] != "/b" || got[1] != "/a" || got[2] != "/c" {
		t.Errorf("addRecent dedupe = %v, want [/b /a /c]", got)
	}
	// the cap trims the oldest
	if got := addRecent([]string{"/a", "/b"}, "/c", 2); len(got) != 2 || got[0] != "/c" || got[1] != "/a" {
		t.Errorf("addRecent cap = %v, want [/c /a]", got)
	}
}
