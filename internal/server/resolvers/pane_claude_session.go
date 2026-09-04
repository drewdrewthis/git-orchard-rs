package resolvers

import (
	"path/filepath"

	"github.com/drewdrewthis/orchardist/internal/server/providers/claudeprojects"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudesessions"
	"github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// sameDir reports whether a and b name the same directory after resolving
// symlinks. On macOS, ps-derived cwds (lsof) come back through
// /private/tmp/... (the resolved real path) while Claude Code's own session
// registry records the launch path (/tmp/...) — a raw string compare treats
// those as different worktrees and the pid-reuse guard in resolveSessionUUID
// wrongly declines a legitimate match. EvalSymlinks failing (e.g. the path no
// longer exists) falls back to the raw string so a transient stat error never
// turns into a silent nil instead of a mismatch's honest false.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	if errA != nil {
		ra = a
	}
	rb, errB := filepath.EvalSymlinks(b)
	if errB != nil {
		rb = b
	}
	return ra == rb
}

// paneSessionRegistry is the read-side contract resolveSessionUUID needs from
// the claudesessions provider: one live-REPL registry entry by pid. Kept as an
// interface so the resolver logic is unit-testable with an in-memory fake.
type paneSessionRegistry interface {
	SessionByPid(pid int) (claudesessions.Session, bool)
}

// cwdMatch is the cwd→sessionUuid join enriched with an ambiguity count. The
// pre-#743 index was map[string]string (last-wins), which is exactly why two
// REPLs sharing a worktree cwd collapsed onto one sessionUuid. Counting lets the
// fallback return "" (a truthful nil) instead of a shared wrong guess when a cwd
// maps to two or more conversations.
type cwdMatch struct {
	sessionUUID string
	count       int
}

// buildCwdIndex builds the ambiguity-aware cwd→session index once per request
// from the conversation list. Duplicate cwds increment count so the caller can
// distinguish an unambiguous single conversation from a colliding cwd.
func buildCwdIndex(convs []claudeprojects.Conversation) map[string]cwdMatch {
	idx := make(map[string]cwdMatch, len(convs))
	for _, conv := range convs {
		if conv.Cwd == nil || *conv.Cwd == "" {
			continue
		}
		m := idx[*conv.Cwd]
		m.count++
		m.sessionUUID = conv.ID.SessionUUID
		idx[*conv.Cwd] = m
	}
	return idx
}

// resolveSessionUUID chooses a pane's sessionUuid (#743). Resolution order:
//
//  1. Registry-first: when the pane is local, has a resolved live pid, and that
//     pid has a registry entry whose recorded cwd matches the pane's resolved
//     cwd, use that entry's sessionId. Keying on the pane's OWN live pid is what
//     distinguishes two REPLs sharing one worktree cwd — and it excludes stale
//     files (a dead pid is never the pane's resolved pid) and pid reuse (the cwd
//     cross-check rejects a recycled pid naming a different worktree).
//  2. cwd fallback: the cwd→session join, but only when unambiguous (exactly one
//     conversation for that cwd).
//  3. Otherwise "": a truthful nil beats a shared wrong sessionUuid.
//
// Remote panes (host != tmux.LocalHostID) skip step 1 entirely — a local
// registry file can never describe a REPL on another host.
func resolveSessionUUID(host string, pid int, cwd string, reg paneSessionRegistry, cwdIndex map[string]cwdMatch) string {
	if cwd == "" {
		return ""
	}
	if host == tmux.LocalHostID && pid > 0 && reg != nil {
		if s, ok := reg.SessionByPid(pid); ok && s.SessionUUID != "" && sameDir(s.Cwd, cwd) {
			return s.SessionUUID
		}
	}
	if m, ok := cwdIndex[cwd]; ok && m.count == 1 {
		return m.sessionUUID
	}
	return ""
}
