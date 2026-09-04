// Package claudesessions is the read side of the ClaudeSessionRegistry node
// (ADR-022): the live-REPL registry Claude Code writes to
// ~/.claude/sessions/<pid>.json, one file per running REPL, mapping a pid to
// its sessionId (and recorded cwd). Its single lookup axis is SessionByPid —
// arity one: exactly one live session per pid.
//
// Wiring per ADR-022 (provider → resolver, no dataloader): the provider exposes
// SessionByPid; the pane→ClaudeInstance resolver (internal/server/resolvers/
// pane_claude.go) calls it directly to attribute each pane's sessionUuid by the
// pane's own resolved live pid, instead of the lossy cwd join that collapsed
// every REPL sharing a worktree cwd onto one shared sessionUuid (#743). No
// dataloader batches this: the resolver reads one registry file per pane on the
// request goroutine, mirroring the existing per-request cwd index.
package claudesessions

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

// Session is one live-REPL registry entry. Cwd is the cwd Claude Code recorded
// at REPL start; the resolver cross-checks it against the pane's resolved cwd
// to reject pid reuse (a recycled pid whose file names a different worktree).
type Session struct {
	Pid         int
	SessionUUID string
	Cwd         string
}

// Provider reads the ~/.claude/sessions registry. It is stateless: every
// SessionByPid call reads the pid's file fresh, so a REPL that started or
// exited since the last request is reflected immediately without a watcher.
type Provider struct {
	root   string
	logger *slog.Logger
}

// New constructs a Provider rooted at root (typically ~/.claude/sessions or the
// CLAUDE_SESSIONS_ROOT override). root need not exist at construction time —
// SessionByPid degrades to "not found" when the directory or file is absent, so
// a daemon that boots before any REPL registers does not fail. An optional
// logger may be supplied; the first non-nil one wins, else slog.Default().
func New(root string, logger ...*slog.Logger) *Provider {
	l := slog.Default()
	for _, cand := range logger {
		if cand != nil {
			l = cand
			break
		}
	}
	return &Provider{root: root, logger: l}
}

// Root returns the configured sessions root. Useful for diagnostics and tests.
func (p *Provider) Root() string { return p.root }

// sessionFile is the on-disk shape of <root>/<pid>.json. Unknown fields
// (startedAt, version, kind, tmux, name, agent, status, ...) are ignored by
// encoding/json, so a Claude Code format bump never breaks decoding.
type sessionFile struct {
	Pid       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

// SessionByPid returns the live-REPL registry entry for pid. The second return
// is false — never an error — when the registry directory is missing, the file
// is absent, the JSON is malformed, or the required pid/sessionId fields are
// missing. This lets the resolver treat a broken/absent registry as "no answer"
// and fall back to the cwd/nil path (#743, AC-6) rather than failing the query.
func (p *Provider) SessionByPid(pid int) (Session, bool) {
	if p == nil || p.root == "" || pid <= 0 {
		return Session{}, false
	}
	path := filepath.Join(p.root, strconv.Itoa(pid)+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Session{}, false // missing dir/file, or unreadable
	}
	// On-disk "sessionId", struct field SessionUUID, and GraphQL sessionUuid
	// are the same value under three spellings — no transform happens here.
	var sf sessionFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		p.logger.Debug("claudesessions: malformed registry file, ignoring", "path", path, "err", err)
		return Session{}, false
	}
	if sf.Pid == 0 || sf.SessionID == "" {
		return Session{}, false // missing required fields
	}
	if sf.Pid != pid {
		// Filename is authoritative: a corrupt write or a renamed/reused file
		// whose body names a different pid than the one we looked up must not
		// be trusted — returning it here would attribute another REPL's
		// sessionUuid to this pid.
		p.logger.Debug("claudesessions: pid/filename mismatch, ignoring", "path", path, "wantPid", pid, "filePid", sf.Pid)
		return Session{}, false
	}
	return Session{Pid: sf.Pid, SessionUUID: sf.SessionID, Cwd: sf.Cwd}, true
}
