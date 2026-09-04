package claudeinstance

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #826 — pid-liveness delete matrix. Helpers (writeSidecar,
// fakeLiveness, bufLogger, sidecarJSON) are defined in janitor_test.go.

// AC3: delete iff pid > 0 && !liveness.IsAlive(pid). Covers the dead,
// alive, and pid-reuse (recorded pid alive but unrelated process) rows
// — reuse is indistinguishable from "still alive" by design (fail
// safe = keep).
func TestJanitor_DeleteMatrix(t *testing.T) {
	tests := []struct {
		name        string
		pid         int
		alive       bool
		wantDeleted bool
		wantCount   int
	}{
		{name: "dead pid is deleted", pid: 4242, alive: false, wantDeleted: true, wantCount: 1},
		{name: "alive pid is kept", pid: 4242, alive: true, wantDeleted: false, wantCount: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeSidecar(t, dir, "orchard-claude-sess.json", tt.pid)

			logger, _ := bufLogger()
			j := NewSidecarJanitor(dir, fakeLiveness{tt.pid: tt.alive}, logger)

			count := j.Sweep(context.Background())

			if count != tt.wantCount {
				t.Errorf("Sweep returned %d, want %d", count, tt.wantCount)
			}

			_, err := os.Stat(path)
			deleted := os.IsNotExist(err)
			if deleted != tt.wantDeleted {
				t.Errorf("file deleted=%v, want %v (stat err: %v)", deleted, tt.wantDeleted, err)
			}
		})
	}
}

// AC6: a legacy sidecar with no "pid" field (old hook, field absent —
// json.Unmarshal leaves Pid at its zero value 0) is kept regardless of
// the liveness checker's contents, because pid<=0 can never prove
// death.
func TestJanitor_LegacySidecarWithNoPidIsKept(t *testing.T) {
	dir := t.TempDir()
	legacyBody := []byte(`{"state":"idle","session_id":"sess-1","tmux_session":"main","cwd":"/tmp","event":"Stop","timestamp":"2026-09-04T00:00:00Z"}`)
	path := filepath.Join(dir, "orchard-claude-legacy.json")
	if err := os.WriteFile(path, legacyBody, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	logger, _ := bufLogger()
	// Liveness checker reports everything dead — if the janitor mistakenly
	// treated missing-pid as pid 0 and asked the checker, a permissive
	// stub could mask the bug, so make the stub maximally hostile.
	j := NewSidecarJanitor(dir, fakeLiveness{0: false}, logger)

	count := j.Sweep(context.Background())

	if count != 0 {
		t.Errorf("Sweep returned %d, want 0 (legacy sidecar must be kept)", count)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected legacy sidecar to survive: %v", err)
	}
}

// AC7: an unparseable (invalid/truncated JSON) sidecar is skipped and
// logged as a warning, and the sweep still processes the remaining
// files — a well-formed dead-pid orphan in the same dir is still
// removed, with the returned count reflecting exactly that one
// removal.
func TestJanitor_MalformedSidecarIsKeptAndLoggedWhileOthersStillSwept(t *testing.T) {
	dir := t.TempDir()

	malformedPath := filepath.Join(dir, "orchard-claude-broken.json")
	if err := os.WriteFile(malformedPath, []byte(`{"pid": 999,`), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	deadPath := writeSidecar(t, dir, "orchard-claude-orphan.json", 5555)

	logger, buf := bufLogger()
	j := NewSidecarJanitor(dir, fakeLiveness{5555: false}, logger)

	count := j.Sweep(context.Background())

	if count != 1 {
		t.Errorf("Sweep returned %d, want 1 (only the valid dead-pid orphan)", count)
	}
	if _, err := os.Stat(malformedPath); err != nil {
		t.Errorf("expected malformed sidecar to survive: %v", err)
	}
	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Errorf("expected valid dead-pid orphan to be removed, stat err: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("orchard-claude-broken.json")) {
		t.Errorf("expected a warning naming the malformed file; log output: %s", buf.String())
	}
}

// AC8: an unreadable heartbeat directory is logged and swallowed —
// Sweep returns 0 and never panics, so daemon startup is never
// blocked by a janitor error.
func TestJanitor_UnreadableDirLogsAndReturnsZero(t *testing.T) {
	parent := t.TempDir()
	unreadable := filepath.Join(parent, "locked")
	if err := os.Mkdir(unreadable, 0o000); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits")
	}

	logger, buf := bufLogger()
	j := NewSidecarJanitor(unreadable, fakeLiveness{}, logger)

	var count int
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Sweep panicked on unreadable dir: %v", r)
			}
		}()
		count = j.Sweep(context.Background())
	}()

	if count != 0 {
		t.Errorf("Sweep on unreadable dir returned %d, want 0", count)
	}
	if !strings.Contains(buf.String(), "failed to read heartbeat dir") {
		t.Errorf("expected log to mention failed to read heartbeat dir; got: %s", buf.String())
	}
}

// Issue #826 follow-up: the glob also matches each heartbeat's
// *.inflight.json companion (tool_use_ids, no pid field). It must
// never be judged by its own (absent) pid — it rides on its
// heartbeat sibling's liveness instead.
func TestJanitor_InflightSibling(t *testing.T) {
	writeInflight := func(t *testing.T, dir, base string) string {
		t.Helper()
		path := filepath.Join(dir, base+".inflight.json")
		if err := os.WriteFile(path, []byte(`{"tool_use_ids":["t1"]}`), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		return path
	}

	t.Run("dead-pid heartbeat removes its inflight sibling", func(t *testing.T) {
		dir := t.TempDir()
		hbPath := writeSidecar(t, dir, "orchard-claude-sess.json", 4242)
		inflightPath := writeInflight(t, dir, "orchard-claude-sess")

		logger, _ := bufLogger()
		j := NewSidecarJanitor(dir, fakeLiveness{4242: false}, logger)
		count := j.Sweep(context.Background())

		if count != 1 {
			t.Errorf("Sweep returned %d, want 1", count)
		}
		if _, err := os.Stat(hbPath); !os.IsNotExist(err) {
			t.Errorf("expected heartbeat removed, stat err: %v", err)
		}
		if _, err := os.Stat(inflightPath); !os.IsNotExist(err) {
			t.Errorf("expected inflight sibling removed, stat err: %v", err)
		}
	})

	t.Run("live-pid heartbeat keeps both", func(t *testing.T) {
		dir := t.TempDir()
		hbPath := writeSidecar(t, dir, "orchard-claude-sess.json", 4242)
		inflightPath := writeInflight(t, dir, "orchard-claude-sess")

		logger, _ := bufLogger()
		j := NewSidecarJanitor(dir, fakeLiveness{4242: true}, logger)
		count := j.Sweep(context.Background())

		if count != 0 {
			t.Errorf("Sweep returned %d, want 0", count)
		}
		if _, err := os.Stat(hbPath); err != nil {
			t.Errorf("expected heartbeat kept: %v", err)
		}
		if _, err := os.Stat(inflightPath); err != nil {
			t.Errorf("expected inflight sibling kept: %v", err)
		}
	})

	t.Run("orphan inflight with no heartbeat is kept", func(t *testing.T) {
		dir := t.TempDir()
		inflightPath := writeInflight(t, dir, "orchard-claude-sess")

		logger, _ := bufLogger()
		j := NewSidecarJanitor(dir, fakeLiveness{}, logger)
		count := j.Sweep(context.Background())

		if count != 0 {
			t.Errorf("Sweep returned %d, want 0", count)
		}
		if _, err := os.Stat(inflightPath); err != nil {
			t.Errorf("expected orphan inflight kept: %v", err)
		}
	})
}
