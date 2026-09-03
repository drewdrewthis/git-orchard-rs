package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The slow lane's enrichment join, and the one judgment that decides the
// daemon is really gone. Both are about a lane being LATE rather than wrong,
// which is the failure the sidebar sees most often.

// A session whose name the daemon didn't join to a worktree still gets branch
// data when its cwd is exactly a worktree's path (two sessions sharing one
// checkout: the daemon's tmuxSession join is 1:1, the second session loses).
func TestJoinFallsBackToCwdPathMatch(t *testing.T) {
	m := &model{
		wtBySession: map[string]wtInfo{"named": {Branch: "feat/x"}},
		repoBySess:  map[string]string{"named": "r1"},
		wtByPath:    map[string]wtInfo{"/w/titw": {Branch: "main"}},
		repoByPath:  map[string]string{"/w/titw": "titw"},
		rows: []row{
			{session: "named", cwd: "/w/titw"},          // name join wins over cwd
			{session: "orphan", cwd: "/w/titw/"},        // trailing slash still matches
			{session: "nested", cwd: "/w/titw/sub/dir"}, // subdir: no match (nested worktrees make prefix joins wrong)
			{session: "nocwd"},
		},
	}
	m.join()
	if got := m.rows[0].branch; got != "feat/x" {
		t.Errorf("name-joined row branch = %q, want feat/x (cwd fallback must not override)", got)
	}
	if got := m.rows[1].branch; got != "main" {
		t.Errorf("cwd-matched row branch = %q, want main", got)
	}
	if got := m.rows[1].repo; got != "titw" {
		t.Errorf("cwd-matched row repo = %q, want titw", got)
	}
	if got := m.rows[2].branch; got != "" {
		t.Errorf("subdir row branch = %q, want empty (exact match only)", got)
	}
	if got := m.rows[3].branch; got != "" {
		t.Errorf("cwd-less row branch = %q, want empty", got)
	}
}

// The cwd-fallback join reads row.cwd, which only the hook overlay supplies —
// so every handler that rebuilds or overlays rows must run applyHooks() before
// join(). A session known only to the hook lane (daemon has no row for it)
// exercises the ordering in each handler: with join first (or missing), the
// hook-appended row never gets its branch.
func TestJoinRunsAfterHooksInEveryHandler(t *testing.T) {
	wt := map[string]wtInfo{"/w/titw": {Branch: "main"}}
	repo := map[string]string{"/w/titw": "titw"}
	hooks := map[string]hookState{"hookonly": {state: "idle", cwd: "/w/titw"}}
	base := func() *model {
		return &model{wtByPath: wt, repoByPath: repo, hooksBySess: hooks}
	}
	check := func(t *testing.T, name string, m *model) {
		t.Helper()
		for _, r := range m.rows {
			if r.session == "hookonly" {
				if r.branch != "main" || r.repo != "titw" {
					t.Errorf("%s: hook-only row branch=%q repo=%q, want main/titw", name, r.branch, r.repo)
				}
				return
			}
		}
		t.Errorf("%s: hook-only row missing entirely", name)
	}

	m := base()
	m.Update(fastDataMsg{rows: []row{{session: "daemon"}}})
	check(t, "fastDataMsg success", m)

	m = base()
	m.rows = []row{{session: "daemon"}}
	m.fastAt = time.Now() // transient failure: rows held
	m.Update(fastDataMsg{err: errors.New("spike")})
	check(t, "fastDataMsg failure", m)

	m = base()
	m.hooksBySess = nil
	m.Update(hookDataMsg{bySession: hooks, dirOK: true})
	check(t, "hookDataMsg", m)

	m = base()
	m.Update(tmuxSubMsg{sessions: []tmuxSession{{Name: "daemon"}}})
	check(t, "tmuxSubMsg", m)
}

// The offline banner and the row hold must express the same judgment: while a
// transient fast-lane error holds the rows, the header cannot simultaneously
// claim the daemon is offline — that contradiction lands exactly at the
// switch-moment spike the hold was built for.
func TestOfflineBannerHonorsTheHoldWindow(t *testing.T) {
	m := &model{width: 42}
	m.Update(fastDataMsg{rows: []row{{session: "a", state: "idle"}}})
	m.Update(fastDataMsg{err: errors.New("context deadline exceeded")})
	if strings.Contains(viewOf(m), "DAEMON OFFLINE") {
		t.Error("banner shown during a transient error while the rows are held")
	}
	m.fastAt = time.Now().Add(-daemonGone - time.Second)
	if !strings.Contains(viewOf(m), "DAEMON OFFLINE") {
		t.Error("daemon unreachable past the hold window but no banner")
	}
	m.subAt = time.Now() // a live push lane is itself proof the daemon is up
	if strings.Contains(viewOf(m), "DAEMON OFFLINE") {
		t.Error("banner shown while the push lane is live")
	}
}
