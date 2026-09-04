package claudeinstance

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// Issue #826: the janitor's delete decision now depends on pid liveness
// ALONE — the liveSessions/tmux dependency is removed from the
// constructor. These tests exercise the dir-handling/logging behavior
// of the new pid-only Sweep(); the delete-decision matrix (AC3, AC6,
// AC7, AC8) lives in janitor_pid_test.go.

// sidecarJSON mirrors the shape orchard-state.sh writes, plus the new
// top-level numeric "pid" field (see ~/.claude/hooks/orchard-state.sh).
type sidecarJSON struct {
	State       string `json:"state"`
	SessionID   string `json:"session_id"`
	TmuxSession string `json:"tmux_session"`
	Cwd         string `json:"cwd"`
	Event       string `json:"event"`
	Timestamp   string `json:"timestamp"`
	Pid         int    `json:"pid"`
}

// writeSidecar writes a well-formed sidecar file with the given pid.
func writeSidecar(t *testing.T, dir, name string, pid int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body, err := json.Marshal(sidecarJSON{
		State:       "idle",
		SessionID:   "sess-1",
		TmuxSession: "main",
		Cwd:         "/tmp",
		Event:       "Stop",
		Timestamp:   "2026-09-04T00:00:00Z",
		Pid:         pid,
	})
	if err != nil {
		t.Fatalf("writeSidecar: marshal: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("writeSidecar: %v", err)
	}
	return path
}

// fakeLiveness is an in-memory LivenessChecker stub (a fake, not a
// mock): pids present in the map with value true are alive, false are
// dead, and any pid absent from the map is treated as dead (fail
// loud in tests rather than silently "alive").
type fakeLiveness map[int]bool

func (f fakeLiveness) IsAlive(pid int) bool {
	return f[pid]
}

// bufLogger returns a logger that writes to a buffer and the buffer.
func bufLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestJanitor_EmptyDirReturnsZero(t *testing.T) {
	dir := t.TempDir()
	logger, _ := bufLogger()
	j := NewSidecarJanitor(dir, fakeLiveness{}, logger)

	count := j.Sweep(context.Background())

	if count != 0 {
		t.Errorf("Sweep in empty dir returned %d, want 0", count)
	}
}

func TestJanitor_NonExistentDirReturnsZero(t *testing.T) {
	logger, _ := bufLogger()
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")
	j := NewSidecarJanitor(nonexistent, fakeLiveness{}, logger)

	count := j.Sweep(context.Background())

	if count != 0 {
		t.Errorf("Sweep in non-existent dir returned %d, want 0", count)
	}
}

func TestJanitor_NonExistentDirDoesNotPanic(t *testing.T) {
	logger, _ := bufLogger()
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")
	j := NewSidecarJanitor(nonexistent, fakeLiveness{}, logger)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Sweep panicked on non-existent dir: %v", r)
		}
	}()
	j.Sweep(context.Background())
}

func TestJanitor_LogsRemovalOfDeadPidSidecar(t *testing.T) {
	dir := t.TempDir()
	writeSidecar(t, dir, "orchard-claude-dead.json", 4242)

	logger, buf := bufLogger()
	j := NewSidecarJanitor(dir, fakeLiveness{4242: false}, logger)

	count := j.Sweep(context.Background())

	if count != 1 {
		t.Fatalf("Sweep returned %d, want 1", count)
	}
	if !bytes.Contains(buf.Bytes(), []byte("orchard-claude-dead.json")) {
		t.Errorf("expected log to mention removed file; log output: %s", buf.String())
	}
}

func TestJanitor_IgnoresFilesNotMatchingSidecarGlob(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "not-a-sidecar.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	logger, _ := bufLogger()
	j := NewSidecarJanitor(dir, fakeLiveness{}, logger)

	count := j.Sweep(context.Background())

	if count != 0 {
		t.Errorf("Sweep returned %d, want 0", count)
	}
	if _, err := os.Stat(filepath.Join(dir, "not-a-sidecar.txt")); err != nil {
		t.Errorf("expected unrelated file to survive: %v", err)
	}
}
