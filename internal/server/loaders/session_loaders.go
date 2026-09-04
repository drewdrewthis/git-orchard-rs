// session_loaders.go — the SessionByPid lookup axis (ADR-022) that resolves a
// ClaudeInstance's sessionUuid from Claude Code's ~/.claude/sessions/<pid>.json
// registry. One node (ClaudeInstance), one axis (SessionByPid, arity one: a pid
// maps to at most one live REPL). Kept out of loaders.go to hold that file's
// per-edge helpers to a readable size.
package loaders

import (
	"fmt"

	"github.com/graph-gophers/dataloader/v7"

	claudesessions "github.com/drewdrewthis/orchardist/internal/server/providers/claudesessions"
)

// ClaudeSessionByPid is the narrow read surface the SessionByPid loader needs
// from the claudesessions provider. *claudesessions.Provider satisfies it
// automatically; defined as an interface so tests can inject an in-memory fake
// without touching the filesystem.
type ClaudeSessionByPid interface {
	SessionByPid(pid int) (claudesessions.Session, bool)
}

// SessionPidKey is the composite key for the SessionByPid loader. Host rides
// along so the key is host-scoped even though the local registry provider only
// answers for the local host — the resolver applies the tmux.LocalHostID guard
// before ever loading a remote key.
type SessionPidKey struct {
	Host string
	Pid  int
}

// String renders a SessionPidKey for logging and cache-key readability.
func (k SessionPidKey) String() string {
	return fmt.Sprintf("ClaudeInstance:%s:%d", k.Host, k.Pid)
}

// loadSessionsByPid batches SessionPidKey -> *claudesessions.Session. A nil
// Data (never an error) means "no live registry entry for that pid" — mirroring
// the Process loader's nil-not-found convention — so the resolver can fall
// through to its cwd join without treating absence as failure.
func loadSessionsByPid(providers *ProvidersBundle, keys []SessionPidKey) []*dataloader.Result[*claudesessions.Session] {
	out := make([]*dataloader.Result[*claudesessions.Session], len(keys))
	reg := providers.ClaudeSessions
	if reg == nil {
		for i := range out {
			out[i] = &dataloader.Result[*claudesessions.Session]{Data: nil}
		}
		return out
	}
	for i, k := range keys {
		if s, ok := reg.SessionByPid(k.Pid); ok {
			sc := s
			out[i] = &dataloader.Result[*claudesessions.Session]{Data: &sc}
			continue
		}
		out[i] = &dataloader.Result[*claudesessions.Session]{Data: nil}
	}
	return out
}

// SessionByPidBatchCount returns the number of SessionByPid loader batch
// invocations since this Loaders was constructed. Used by n+1 tests.
func (l *Loaders) SessionByPidBatchCount() int { return l.sessionByPidBatches.value() }
