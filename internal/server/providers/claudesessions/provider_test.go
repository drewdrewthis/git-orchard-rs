package claudesessions

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root string, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Happy path: a well-formed <pid>.json yields the session.
func TestSessionByPid_Happy(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "8164.json",
		`{"pid":8164,"sessionId":"e2ee4f99-086c","cwd":"/w/issue743","version":"2.1.261","status":"busy"}`)

	p := New(root, nil)
	got, ok := p.SessionByPid(8164)
	if !ok {
		t.Fatal("SessionByPid(8164) = not found, want found")
	}
	if got.SessionUUID != "e2ee4f99-086c" || got.Cwd != "/w/issue743" || got.Pid != 8164 {
		t.Errorf("got %+v", got)
	}
}

// Missing directory / missing file → not found, no panic.
func TestSessionByPid_MissingDir(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "does-not-exist"), nil)
	if _, ok := p.SessionByPid(8164); ok {
		t.Error("want not found for a missing registry dir")
	}
}

func TestSessionByPid_MissingFile(t *testing.T) {
	p := New(t.TempDir(), nil)
	if _, ok := p.SessionByPid(9999); ok {
		t.Error("want not found for an absent pid file")
	}
}

// Garbage JSON → not found (tolerant decode, never a panic/error).
func TestSessionByPid_GarbageJSON(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "8164.json", "not json at all {{{")
	p := New(root, nil)
	if _, ok := p.SessionByPid(8164); ok {
		t.Error("want not found for a garbage json file")
	}
}

// Missing required fields (no sessionId, or no pid) → not found.
func TestSessionByPid_MissingFields(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "8164.json", `{"pid":8164,"cwd":"/w"}`) // no sessionId
	writeFile(t, root, "8165.json", `{"sessionId":"x","cwd":"/w"}`)
	p := New(root, nil)
	if _, ok := p.SessionByPid(8164); ok {
		t.Error("want not found when sessionId missing")
	}
	if _, ok := p.SessionByPid(8165); ok {
		t.Error("want not found when pid missing")
	}
}

// Unknown/renamed fields are ignored (format-bump tolerance).
func TestSessionByPid_IgnoresUnknownFields(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "8164.json",
		`{"pid":8164,"sessionId":"s","cwd":"/w","futureField":42,"tmux":"sess:@24.%24","agent":"lead"}`)
	p := New(root, nil)
	got, ok := p.SessionByPid(8164)
	if !ok || got.SessionUUID != "s" {
		t.Errorf("got %+v ok=%v, want session 's' despite unknown fields", got, ok)
	}
}

// Guard: non-positive pid never touches the filesystem.
func TestSessionByPid_NonPositivePid(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "0.json", `{"pid":0,"sessionId":"s"}`)
	p := New(root, nil)
	if _, ok := p.SessionByPid(0); ok {
		t.Error("want not found for pid 0")
	}
	if _, ok := p.SessionByPid(-1); ok {
		t.Error("want not found for negative pid")
	}
}
