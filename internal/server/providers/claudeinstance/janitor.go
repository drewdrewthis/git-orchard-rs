package claudeinstance

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
)

// SidecarJanitor removes stale orchard-claude-*.json heartbeat files
// from heartbeatDir whose recorded pid is provably dead. Run at
// daemon startup.
//
// Issue #826: the janitor used to key on THIS daemon's own tmux
// snapshot — a session name absent from that snapshot was deleted as
// "orphan". But a daemon can run cross-socket, or with tmux
// unreachable/empty at startup, in which case its snapshot contains
// none of another live instance's sessions; that made every OTHER
// instance's sidecar look orphaned and get swept while still live.
// The fix: judge liveness by the sidecar's own recorded OS pid via
// LivenessChecker, never by cross-referencing this daemon's tmux
// state. A missing/zero pid (legacy sidecar) or malformed JSON can
// never prove death, so it is kept — fail safe, not fail sweep.
//
// The janitor exists for cohabitation: after issue #603 the daemon
// stopped reading these sidecars, but the hook may still be writing
// them until the codex-side cleanup ships (AC #4). The hook deletes
// its own files on SessionEnd, but a Claude session killed via
// SIGKILL or tmux kill-session never fires SessionEnd, so orphans
// accumulate. This is a startup janitor only — it does NOT run
// continuously.
type SidecarJanitor struct {
	heartbeatDir string
	liveness     LivenessChecker
	logger       *slog.Logger
}

// sidecarPid is the minimal shape needed to make a delete decision;
// the rest of the sidecar's fields are irrelevant to the janitor.
type sidecarPid struct {
	Pid int `json:"pid"`
}

// NewSidecarJanitor constructs a SidecarJanitor. heartbeatDir should
// be the resolved heartbeat directory (use ResolveDir() when the
// caller doesn't have a custom override). A nil liveness defaults to
// OSLivenessChecker.
func NewSidecarJanitor(
	heartbeatDir string,
	liveness LivenessChecker,
	logger *slog.Logger,
) *SidecarJanitor {
	if logger == nil {
		logger = slog.Default()
	}
	if liveness == nil {
		liveness = OSLivenessChecker{}
	}
	return &SidecarJanitor{
		heartbeatDir: heartbeatDir,
		liveness:     liveness,
		logger:       logger,
	}
}

// Sweep removes orphan files. Returns the number removed. Errors are
// logged and swallowed — janitor failures must not block daemon
// startup.
func (j *SidecarJanitor) Sweep(ctx context.Context) int {
	entries, err := os.ReadDir(j.heartbeatDir)
	if err != nil {
		if !os.IsNotExist(err) {
			j.logger.Error("sidecar janitor: failed to read heartbeat dir; skipping sweep",
				"dir", j.heartbeatDir, "err", err)
		}
		return 0
	}

	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if ok, _ := filepath.Match("orchard-claude-*.json", name); !ok {
			continue
		}
		path := filepath.Join(j.heartbeatDir, name)

		body, err := os.ReadFile(path)
		if err != nil {
			j.logger.Warn("sidecar janitor: failed to read sidecar; skipping",
				"file", path, "err", err)
			continue
		}

		var sc sidecarPid
		if err := json.Unmarshal(body, &sc); err != nil {
			j.logger.Warn("sidecar janitor: malformed sidecar JSON; skipping",
				"file", path, "err", err)
			continue
		}

		if sc.Pid <= 0 || j.liveness.IsAlive(sc.Pid) {
			continue
		}

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			j.logger.Warn("sidecar janitor: failed to remove orphan file",
				"file", path, "err", err)
			continue
		}
		j.logger.Info("sidecar janitor: removed orphan sidecar",
			"file", filepath.Base(path), "pid", sc.Pid)
		removed++
	}

	j.logger.Info("sidecar janitor swept orphan files", "count", removed, "dir", j.heartbeatDir)
	return removed
}
