// Package claudeinstance is the pane-first ClaudeInstance read path (ADR-022
// Phase 4). The heartbeat-based Provider/Composer/Watcher subsystem was
// deleted in Phase 5; state derivation now goes through DeriveInstanceState
// (state_derive.go) which reads jsonl snapshots via SnapshotReader.
//
// Remaining surface:
//   - DeriveInstanceState + DeriveState (state_derive.go)
//   - ClassifyState + JsonlStateSnapshot (jsonl_state.go)
//   - SnapshotReader + FsSnapshotReader (jsonl.go)
//   - SidecarJanitor (janitor.go) — startup orphan-cleaner
package claudeinstance

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// HeartbeatStaleAfter is the freshness window used by DeriveInstanceState
// when no explicit StaleAfter is supplied.
const HeartbeatStaleAfter = 30 * time.Second

// ResolveDir applies the heartbeat directory env-var chain:
// ORCHARD_HEARTBEAT_DIR wins; then TMPDIR; then /tmp.
// Used by SidecarJanitor (janitor.go) and by server.go at startup.
func ResolveDir() string {
	if v := os.Getenv("ORCHARD_HEARTBEAT_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("TMPDIR"); v != "" {
		return v
	}
	return "/tmp"
}

// LivenessChecker reports whether a pid is still alive on the host.
// Production uses OSLivenessChecker; tests inject a stub map.
type LivenessChecker interface {
	IsAlive(pid int) bool
}

// OSLivenessChecker uses the standard signal-0 trick to ask the kernel
// whether a pid is alive without sending a real signal.
type OSLivenessChecker struct{}

// IsAlive returns true when sending signal 0 to pid succeeds, or when
// the kernel answers EPERM. Sidecars are host-local but not
// necessarily same-user (e.g. a sidecar owned by another account, or
// launchd/pid 1): EPERM means "a process with this pid exists and
// answered the kernel" — it is alive, just not signalable by us.
// Misreading EPERM as dead would delete a live sidecar (#826). Any other
// error is reported as not-alive; the janitor's own missing/zero-pid guard
// is the fail-safe layer above this.
// Returns false for pid<=0.
func (OSLivenessChecker) IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	sigErr := proc.Signal(syscall.Signal(0))
	return sigErr == nil || errors.Is(sigErr, syscall.EPERM)
}
