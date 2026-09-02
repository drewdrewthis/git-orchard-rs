// worktree_tmux_test.go covers Worktree.tmuxPanes AC1 scenarios (#511):
//
//   - Exact cwd match (feature scenario @ line 25)
//   - cwd under path with trailing component (feature scenario @ line 32)
//   - Sibling path excluded — no false-prefix match (feature scenario @ line 39)
//   - Panes sorted deterministically by paneId ascending (feature scenario @ line 47)
//   - tmuxPanes is [TmuxPane!]! (non-nullable) in the schema (feature scenario @ line 53)
//
// The @integration scenarios (lines 25, 32, 47) that require cwd resolution
// via lsof are Darwin-only because the PS adapter's FetchCwds falls back to
// an empty map on Linux until the /proc path lands (ps.FetchCwds #platform).
// The @unit scenarios (lines 39, 53) run on all platforms.

package resolvers

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
	psprovider "github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	tmuxprovider "github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// ----------------------------------------------------------------------
// Shared fakes
// ----------------------------------------------------------------------

// tmuxTestRunner is a CommandRunner for the tmux adapter. It handles the
// four list-* commands and the alive probe. Rows are joined with
// tmuxprovider.FieldSep — the adapter's own constant — so the stub cannot
// drift from the wire format the way the hardcoded U+0001 copies did
// (#664, #712).
type tmuxTestRunner struct {
	// paneRows is the list of raw pane lines, FieldSep-separated:
	// session|windowIdx|paneId|title|command|pid|width|height|dead
	paneRows []string
	// sessionRows: session|created|attached|activity|windows|curwindow
	sessionRows []string
	// windowRows: session|index|name|active|panes|curpane
	windowRows []string
}

func (r *tmuxTestRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "tmux" {
		return nil, fmt.Errorf("unexpected command: %s", name)
	}
	// find the subcommand (first non-flag positional arg)
	sub := firstNonFlagTmuxArg(args)
	switch sub {
	case "info":
		return []byte("ok\n"), nil
	case "list-sessions":
		return []byte(strings.Join(r.sessionRows, "\n") + "\n"), nil
	case "list-windows":
		return []byte(strings.Join(r.windowRows, "\n") + "\n"), nil
	case "list-panes":
		return []byte(strings.Join(r.paneRows, "\n") + "\n"), nil
	case "list-clients":
		return []byte(""), nil
	default:
		return []byte(""), nil
	}
}

// firstNonFlagTmuxArg returns the first positional arg in a tmux command line,
// skipping -S / -L flag-value pairs.
func firstNonFlagTmuxArg(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-S" || args[i] == "-L" {
			i++ // skip value
			continue
		}
		return args[i]
	}
	return ""
}

// buildPaneRow builds a `tmux list-panes -a -F <listAllFormat>` row.
//
// listAllFormat emits tmuxprovider.ListAllFieldCount FieldSep-separated fields
// per pane row — every test row must include all of them, even filler.
// Field order (0-based):
//
//	0  session_name
//	1  session_created
//	2  session_attached
//	3  session_activity
//	4  session_windows
//	5  session_window_index
//	6  window_index
//	7  window_name
//	8  window_active
//	9  window_panes
//	10 window_active_pane
//	11 pane_id
//	12 pane_title
//	13 pane_current_command
//	14 pane_pid
//	15 pane_width
//	16 pane_height
//	17 pane_dead
//
// Renamed from paneRow to avoid a redeclaration with the paneRow helper in
// tmux_pane_process_test.go (#521 — sibling-PR collision between #508 and #516).
func buildPaneRow(session, paneID string, pid int) string {
	return buildPaneRowAt(session, paneID, pid, 1700000000)
}

// buildPaneRowAt is the same as buildPaneRow but lets the caller specify the
// session_activity unix timestamp. listAll's consolidation of list-sessions +
// list-panes into one call means session metadata (including activity) is
// embedded on every pane row; this helper lets per-session activity-based
// tests parameterise it without abandoning buildPaneRow's defaults.
func buildPaneRowAt(session, paneID string, pid int, activityUnix int64) string {
	return strings.Join([]string{
		session,                              // 0  session_name
		"1700000000",                         // 1  session_created
		"0",                                  // 2  session_attached
		fmt.Sprintf("%d", activityUnix),      // 3  session_activity
		"1",                                  // 4  session_windows
		"0",                                  // 5  session_window_index
		"0",                                  // 6  window_index
		"main",                               // 7  window_name
		"1",                                  // 8  window_active
		"1",                                  // 9  window_panes
		paneID,                               // 10 window_active_pane
		paneID,                               // 11 pane_id
		"title",                              // 12 pane_title
		"zsh",                                // 13 pane_current_command
		fmt.Sprintf("%d", pid),               // 14 pane_pid
		"200",                                // 15 pane_width
		"50",                                 // 16 pane_height
		"0",                                  // 17 pane_dead
	}, tmuxprovider.FieldSep)
}

// sessionRow builds a list-sessions row.
func sessionRow(name string, activityUnix int64) string {
	return strings.Join([]string{
		name,
		"1700000000",         // created
		"0",                  // attached
		fmt.Sprintf("%d", activityUnix), // last activity
		"1",                  // window count
		"0",                  // current window index
	}, tmuxprovider.FieldSep)
}

// windowRow builds a list-windows row.
func windowRow(session string) string {
	return strings.Join([]string{session, "0", "main", "1", "1", "%1"}, tmuxprovider.FieldSep)
}

// psTestRunner is a CommandRunner for the ps and lsof adapters.
// It maps pid → cwd for lsof calls and returns a minimal ps header for FetchAll.
type psTestRunner struct {
	// cwdByPid maps pid → cwd for fake lsof responses.
	cwdByPid map[int]string
}

const psHeader = "  PID  PPID USER             TT  %CPU    RSS                 STARTED COMMAND\n"

func (r *psTestRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	switch name {
	case "ps":
		// Return header-only so FetchAll succeeds with an empty process table.
		return []byte(psHeader), nil
	case "lsof":
		// lsof -a -d cwd -p <pid|pid,pid,...> -F n
		// macOS lsof exits non-zero if ANY requested pid is missing, but it
		// still emits the entries for the pids that were found. Mirror that
		// behaviour: emit found entries; if any pid was unresolved, return
		// the buffer with a non-nil error so the adapter's "len(raw) > 0
		// AND err != nil" branch is exercised (parse what came through).
		pids := parseLsofPids(args)
		var buf bytes.Buffer
		anyFound := false
		anyMissing := false
		for _, pid := range pids {
			if cwd, ok := r.cwdByPid[pid]; ok {
				fmt.Fprintf(&buf, "p%d\nn%s\n", pid, cwd)
				anyFound = true
			} else {
				anyMissing = true
			}
		}
		if !anyFound {
			return nil, fmt.Errorf("lsof: no entries for %v", pids)
		}
		if anyMissing {
			return buf.Bytes(), fmt.Errorf("lsof: partial — some pids missing")
		}
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("unexpected command: %s", name)
	}
}

// parseLsofPids extracts the pid argument(s) from `lsof -a -d cwd -p <pid|pid,pid,...> -F n`.
// macOS lsof accepts a single -p with a comma-separated list, which the
// real adapter uses to coalesce N requests into one shellout — the test
// runner mirrors that.
func parseLsofPids(args []string) []int {
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			parts := strings.Split(args[i+1], ",")
			out := make([]int, 0, len(parts))
			for _, p := range parts {
				var n int
				if _, err := fmt.Sscanf(p, "%d", &n); err == nil {
					out = append(out, n)
				}
			}
			return out
		}
	}
	return nil
}

// buildResolver wires a worktreeResolver backed by fake tmux and ps providers.
// The fake tmux runner serves the given pane rows; the fake ps runner maps
// pid → cwd for lsof calls.
//
// On non-Darwin platforms FetchCwds always returns an empty map (no /proc
// path wired), so callers that need cwd-based assertions must skip on non-Darwin
// via t.Skip in the test body.
func buildResolver(t *testing.T, tr *tmuxTestRunner, pidToCwd map[int]string) *worktreeResolver {
	t.Helper()

	hostID := "local"

	// Tmux provider
	tmuxAdapter := tmuxprovider.NewAdapter(tmuxprovider.HostID(hostID)).
		WithRunner(tr).
		WithSocket("/tmp/orchard-test-worktree-tmux.sock")
	tmuxProv := tmuxprovider.New(tmuxAdapter, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tmuxProv.Refresh(ctx); err != nil {
		t.Fatalf("tmux Refresh: %v", err)
	}

	// PS provider
	psRunner := &psTestRunner{cwdByPid: pidToCwd}
	psAdapter := psprovider.NewAdapter(hostID).WithRunner(psRunner)
	psProv := psprovider.New(psAdapter, nil)
	if err := psProv.Start(ctx); err != nil {
		t.Fatalf("ps Start: %v", err)
	}

	return &worktreeResolver{&Resolver{
		Tmux: tmuxProv,
		PS:   psProv,
	}}
}

// ----------------------------------------------------------------------
// AC1 — cwd exact match (feature scenario @ line 25)
// @integration @issue-511
// ----------------------------------------------------------------------

// TestTmuxPanesResolver_ExactCwdMatch verifies that a pane whose cwd exactly
// equals the worktree path is included in tmuxPanes.
func TestTmuxPanesResolver_ExactCwdMatch(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	const worktreePath = "/Users/me/repo/.worktrees/feat-x"
	const paneID = "%1"
	const fakePID = 10001

	tr := &tmuxTestRunner{
		sessionRows: []string{sessionRow("main", 1700000000)},
		windowRows:  []string{windowRow("main")},
		paneRows:    []string{buildPaneRow("main", paneID, fakePID)},
	}
	r := buildResolver(t, tr, map[int]string{
		fakePID: worktreePath, // cwd == worktree path exactly
	})

	wt := &graphql1.Worktree{
		ID:   "proj:feat-x",
		Path: worktreePath,
		Host: "local",
	}

	got, err := r.TmuxPanes(context.Background(), wt)
	if err != nil {
		t.Fatalf("TmuxPanes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("TmuxPanes = %d panes, want 1; got: %v", len(got), paneIDsOf(got))
	}
	if got[0].PaneID != paneID {
		t.Errorf("TmuxPanes[0].PaneID = %q, want %q", got[0].PaneID, paneID)
	}
}

// ----------------------------------------------------------------------
// AC1 — cwd under path (feature scenario @ line 32)
// @integration @issue-511
// ----------------------------------------------------------------------

// TestTmuxPanesResolver_CwdUnderPath verifies that a pane whose cwd sits
// under the worktree path (cwd has path + "/" as prefix) is included.
func TestTmuxPanesResolver_CwdUnderPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	const worktreePath = "/Users/me/repo/.worktrees/feat-x"
	const paneCwd = worktreePath + "/internal/server"
	const paneID = "%2"
	const fakePID = 10002

	tr := &tmuxTestRunner{
		sessionRows: []string{sessionRow("main", 1700000000)},
		windowRows:  []string{windowRow("main")},
		paneRows:    []string{buildPaneRow("main", paneID, fakePID)},
	}
	r := buildResolver(t, tr, map[int]string{
		fakePID: paneCwd,
	})

	wt := &graphql1.Worktree{
		ID:   "proj:feat-x",
		Path: worktreePath,
		Host: "local",
	}

	got, err := r.TmuxPanes(context.Background(), wt)
	if err != nil {
		t.Fatalf("TmuxPanes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("TmuxPanes = %d panes, want 1; got: %v", len(got), paneIDsOf(got))
	}
	if got[0].PaneID != paneID {
		t.Errorf("TmuxPanes[0].PaneID = %q, want %q", got[0].PaneID, paneID)
	}
}

// ----------------------------------------------------------------------
// AC1 — sibling path excluded (feature scenario @ line 39)
// @unit @issue-511
// ----------------------------------------------------------------------

// TestCwdMatchesWorktree_SiblingExcluded verifies the "exact OR path+/"
// match rule: a cwd that merely shares a prefix string with worktree.path
// but continues without a "/" separator must NOT match.
func TestCwdMatchesWorktree_SiblingExcluded(t *testing.T) {
	const path = "/Users/me/repo/.worktrees/feat-x"

	// Sibling path: shares prefix but no "/".
	if cwdMatchesWorktree(path, path+"extra") {
		t.Errorf("cwdMatchesWorktree(%q, %q) = true, want false (sibling path must not match)", path, path+"extra")
	}

	// Exact match must still pass.
	if !cwdMatchesWorktree(path, path) {
		t.Errorf("cwdMatchesWorktree(%q, %q) = false, want true (exact match must pass)", path, path)
	}

	// Under-path must still pass.
	if !cwdMatchesWorktree(path, path+"/sub/dir") {
		t.Errorf("cwdMatchesWorktree(%q, %q) = false, want true (sub-path must pass)", path, path+"/sub/dir")
	}
}

// ----------------------------------------------------------------------
// AC1 — sort order (feature scenario @ line 47)
// @unit @issue-511
// ----------------------------------------------------------------------

// TestTmuxPanesResolver_SortedByPaneId verifies that matching panes are
// returned ordered by paneId ascending (lex order — "%2" < "%5" < "%9").
func TestTmuxPanesResolver_SortedByPaneId(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	const worktreePath = "/Users/me/repo/.worktrees/feat-x"
	const fakePID5 = 10005
	const fakePID2 = 10002
	const fakePID9 = 10009

	tr := &tmuxTestRunner{
		sessionRows: []string{sessionRow("main", 1700000000)},
		windowRows:  []string{windowRow("main")},
		paneRows: []string{
			buildPaneRow("main", "%5", fakePID5),
			buildPaneRow("main", "%2", fakePID2),
			buildPaneRow("main", "%9", fakePID9),
		},
	}
	r := buildResolver(t, tr, map[int]string{
		fakePID5: worktreePath,
		fakePID2: worktreePath,
		fakePID9: worktreePath,
	})

	wt := &graphql1.Worktree{
		ID:   "proj:feat-x",
		Path: worktreePath,
		Host: "local",
	}

	got, err := r.TmuxPanes(context.Background(), wt)
	if err != nil {
		t.Fatalf("TmuxPanes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("TmuxPanes = %d panes, want 3; got: %v", len(got), paneIDsOf(got))
	}
	want := []string{"%2", "%5", "%9"}
	for i, id := range want {
		if got[i].PaneID != id {
			t.Errorf("TmuxPanes[%d].PaneID = %q, want %q (not sorted by paneId)", i, got[i].PaneID, id)
		}
	}
}

// ----------------------------------------------------------------------
// AC1 — non-nullable in schema (feature scenario @ line 53)
// @integration @issue-511
// ----------------------------------------------------------------------

// TestWorktreeTmuxPanes_SchemaIsNonNullable verifies that the Worktree.tmuxPanes
// field is declared as [TmuxPane!]! (non-null list of non-null TmuxPane) in
// the embedded schema SDL.
func TestWorktreeTmuxPanes_SchemaIsNonNullable(t *testing.T) {
	sdl := SchemaSDL()
	// The schema must contain the non-nullable declaration.
	needle := "tmuxPanes: [TmuxPane!]!"
	if !strings.Contains(sdl, needle) {
		t.Errorf("schema SDL does not contain %q — Worktree.tmuxPanes must be non-nullable ([TmuxPane!]!)", needle)
	}
}

// ----------------------------------------------------------------------
// AC2 — tmuxSession unique session (feature scenario @ line 63)
// @integration @issue-511
// ----------------------------------------------------------------------

// TestTmuxSessionResolver_SinglePane verifies that when exactly one pane
// matches, TmuxSession returns that pane's session (AC2: single-pane case).
// Also validates that lastActivityAt is correctly surfaced through the
// sub-resolver.
func TestTmuxSessionResolver_SinglePane(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	const worktreePath = "/Users/me/repo/.worktrees/feat-x"
	const sessionName = "feat-x"
	const fakePID = 20001
	// 2026-05-09T10:00:00Z as unix seconds.
	const activityUnix = int64(1746784800)

	tr := &tmuxTestRunner{
		sessionRows: []string{sessionRow(sessionName, activityUnix)},
		windowRows:  []string{windowRow(sessionName)},
		paneRows:    []string{buildPaneRowAt(sessionName, "%1", fakePID, activityUnix)},
	}
	r := buildResolver(t, tr, map[int]string{
		fakePID: worktreePath,
	})

	wt := &graphql1.Worktree{
		ID:   "proj:feat-x",
		Path: worktreePath,
		Host: "local",
	}

	sess, err := r.TmuxSession(context.Background(), wt)
	if err != nil {
		t.Fatalf("TmuxSession: %v", err)
	}
	if sess == nil {
		t.Fatal("TmuxSession = nil, want non-nil")
	}
	if sess.Name != sessionName {
		t.Errorf("TmuxSession.Name = %q, want %q", sess.Name, sessionName)
	}

	// Verify lastActivityAt via the sub-resolver.
	sessResolver := &tmuxSessionResolver{r.Resolver}
	lastAt, err := sessResolver.LastActivityAt(context.Background(), sess)
	if err != nil {
		t.Fatalf("LastActivityAt: %v", err)
	}
	if lastAt == nil {
		t.Fatal("LastActivityAt = nil, want non-nil")
	}
	// The feature spec says "2026-05-09T10:00:00Z"; round-trip through
	// RFC3339 parsing so we compare canonical form, not literal bytes.
	wantUnix := time.Unix(activityUnix, 0).UTC().Format(time.RFC3339)
	if *lastAt != wantUnix {
		t.Errorf("LastActivityAt = %q, want %q", *lastAt, wantUnix)
	}
}

// ----------------------------------------------------------------------
// AC2 — tmuxSession is nullable in the schema (feature scenario @ line 71)
// @integration @issue-511 (cross-platform)
// ----------------------------------------------------------------------

// TestWorktreeTmuxSession_SchemaIsNullable verifies that the
// Worktree.tmuxSession field is declared as nullable (TmuxSession, no "!")
// in the embedded schema SDL.
func TestWorktreeTmuxSession_SchemaIsNullable(t *testing.T) {
	sdl := SchemaSDL()
	// The field must be present without a trailing "!".
	needle := "tmuxSession: TmuxSession"
	if !strings.Contains(sdl, needle) {
		t.Errorf("schema SDL does not contain %q — Worktree.tmuxSession must be nullable", needle)
	}
	// Double-check: it must NOT be declared non-nullable.
	nonNullNeedle := "tmuxSession: TmuxSession!"
	if strings.Contains(sdl, nonNullNeedle) {
		t.Errorf("schema SDL contains %q — Worktree.tmuxSession must be nullable (no '!')", nonNullNeedle)
	}
}

// ----------------------------------------------------------------------
// AC3 — higher lastActivityAt wins (feature scenario @ line 81)
// @unit @issue-511
// ----------------------------------------------------------------------

// TestTmuxSessionResolver_HigherActivityWins verifies that when two sessions
// both have matching panes, TmuxSession returns the one with the higher
// lastActivityAt (AC3).
func TestTmuxSessionResolver_HigherActivityWins(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	const worktreePath = "/Users/me/repo/.worktrees/feat-x"
	const pidAlpha = 30001
	const pidBeta = 30002
	// alpha: 2026-05-09T09:00:00Z; beta: 2026-05-09T11:00:00Z
	const alphaActivity = int64(1746781200)
	const betaActivity = int64(1746788400)

	tr := &tmuxTestRunner{
		sessionRows: []string{
			sessionRow("alpha", alphaActivity),
			sessionRow("beta", betaActivity),
		},
		windowRows: []string{
			windowRow("alpha"),
			windowRow("beta"),
		},
		paneRows: []string{
			buildPaneRowAt("alpha", "%10", pidAlpha, alphaActivity),
			buildPaneRowAt("beta", "%11", pidBeta, betaActivity),
		},
	}
	r := buildResolver(t, tr, map[int]string{
		pidAlpha: worktreePath,
		pidBeta:  worktreePath,
	})

	wt := &graphql1.Worktree{
		ID:   "proj:feat-x",
		Path: worktreePath,
		Host: "local",
	}

	sess, err := r.TmuxSession(context.Background(), wt)
	if err != nil {
		t.Fatalf("TmuxSession: %v", err)
	}
	if sess == nil {
		t.Fatal("TmuxSession = nil, want non-nil")
	}
	if sess.Name != "beta" {
		t.Errorf("TmuxSession.Name = %q, want %q (higher lastActivityAt should win)", sess.Name, "beta")
	}
}

// ----------------------------------------------------------------------
// AC3 — name tie-break (feature scenario @ line 89)
// @unit @issue-511
// ----------------------------------------------------------------------

// TestTmuxSessionResolver_NameTieBreak verifies that when two sessions have
// identical lastActivityAt, the session with the lex-lower name wins (AC3).
func TestTmuxSessionResolver_NameTieBreak(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	const worktreePath = "/Users/me/repo/.worktrees/feat-x"
	const pidZebra = 40001
	const pidAlpha = 40002
	// Both sessions have the same lastActivityAt.
	const sameActivity = int64(1746788400)

	tr := &tmuxTestRunner{
		sessionRows: []string{
			sessionRow("zebra", sameActivity),
			sessionRow("alpha", sameActivity),
		},
		windowRows: []string{
			windowRow("zebra"),
			windowRow("alpha"),
		},
		paneRows: []string{
			buildPaneRowAt("zebra", "%20", pidZebra, sameActivity),
			buildPaneRowAt("alpha", "%21", pidAlpha, sameActivity),
		},
	}
	r := buildResolver(t, tr, map[int]string{
		pidZebra: worktreePath,
		pidAlpha: worktreePath,
	})

	wt := &graphql1.Worktree{
		ID:   "proj:feat-x",
		Path: worktreePath,
		Host: "local",
	}

	sess, err := r.TmuxSession(context.Background(), wt)
	if err != nil {
		t.Fatalf("TmuxSession: %v", err)
	}
	if sess == nil {
		t.Fatal("TmuxSession = nil, want non-nil")
	}
	if sess.Name != "alpha" {
		t.Errorf("TmuxSession.Name = %q, want %q (lex-lower name must win on tie)", sess.Name, "alpha")
	}
}

// ----------------------------------------------------------------------
// AC4 — no matches (feature scenario @ line 101)
// @unit @issue-511
// ----------------------------------------------------------------------

// TestTmuxPanesAndSession_NoMatch verifies that a worktree with no matching
// panes returns tmuxPanes=[] and tmuxSession=nil (AC4).
func TestTmuxPanesAndSession_NoMatch(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	const worktreePath = "/Users/me/repo/.worktrees/lonely"
	const fakePID = 50001

	tr := &tmuxTestRunner{
		sessionRows: []string{sessionRow("other", 1700000000)},
		windowRows:  []string{windowRow("other")},
		paneRows:    []string{buildPaneRow("other", "%30", fakePID)},
	}
	// The pane's cwd is somewhere else entirely.
	r := buildResolver(t, tr, map[int]string{
		fakePID: "/some/other/path",
	})

	wt := &graphql1.Worktree{
		ID:   "proj:lonely",
		Path: worktreePath,
		Host: "local",
	}

	panes, err := r.TmuxPanes(context.Background(), wt)
	if err != nil {
		t.Fatalf("TmuxPanes: %v", err)
	}
	if len(panes) != 0 {
		t.Errorf("TmuxPanes = %v, want []", paneIDsOf(panes))
	}

	sess, err := r.TmuxSession(context.Background(), wt)
	if err != nil {
		t.Fatalf("TmuxSession: %v", err)
	}
	if sess != nil {
		t.Errorf("TmuxSession = %v, want nil", sess)
	}
}

// ----------------------------------------------------------------------
// AC5 — null cwd silently skipped (feature scenario @ line 113)
// @unit @issue-511
// ----------------------------------------------------------------------

// TestTmuxPanesResolver_NullCwdSkipped verifies that a pane whose lsof
// returns an empty cwd is NOT treated as matching everything (AC5).
// The empty-cwd pane must be silently dropped; no error surfaced.
func TestTmuxPanesResolver_NullCwdSkipped(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	const worktreePath = "/Users/me/repo/.worktrees/feat-x"
	const fakePID = 60001

	tr := &tmuxTestRunner{
		sessionRows: []string{sessionRow("main", 1700000000)},
		windowRows:  []string{windowRow("main")},
		paneRows:    []string{buildPaneRow("main", "%40", fakePID)},
	}
	// Map the pane pid to an empty cwd — simulates null/unresolvable cwd.
	r := buildResolver(t, tr, map[int]string{
		fakePID: "", // empty cwd — should NOT match the worktree
	})

	wt := &graphql1.Worktree{
		ID:   "proj:feat-x",
		Path: worktreePath,
		Host: "local",
	}

	got, err := r.TmuxPanes(context.Background(), wt)
	if err != nil {
		t.Fatalf("TmuxPanes returned error %v, want nil (empty cwd must be silently skipped)", err)
	}
	if len(got) != 0 {
		t.Errorf("TmuxPanes = %v, want [] (pane with empty cwd must NOT match)", paneIDsOf(got))
	}
}

// ----------------------------------------------------------------------
// AC5 — PS error silently skipped (feature scenario @ line 121)
// @unit @issue-511
// ----------------------------------------------------------------------

// TestTmuxPanesResolver_PSErrorSkipped verifies that a pane whose foreground
// pid is unresolvable by the ps provider is silently skipped (AC5).
func TestTmuxPanesResolver_PSErrorSkipped(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	const worktreePath = "/Users/me/repo/.worktrees/feat-x"
	const badPID = 70001  // lsof will return an error for this pid
	const goodPID = 70002 // this one resolves fine

	tr := &tmuxTestRunner{
		sessionRows: []string{sessionRow("main", 1700000000)},
		windowRows:  []string{windowRow("main")},
		paneRows: []string{
			buildPaneRow("main", "%50", badPID),
			buildPaneRow("main", "%51", goodPID),
		},
	}
	// Only map the good pid; bad pid will cause lsof to return an error.
	r := buildResolver(t, tr, map[int]string{
		goodPID: worktreePath,
		// badPID is intentionally absent — psTestRunner.Run returns an error
	})

	wt := &graphql1.Worktree{
		ID:   "proj:feat-x",
		Path: worktreePath,
		Host: "local",
	}

	got, err := r.TmuxPanes(context.Background(), wt)
	if err != nil {
		t.Fatalf("TmuxPanes returned error %v, want nil (PS errors must be silently skipped)", err)
	}
	// Only the good pane should be returned.
	if len(got) != 1 {
		t.Fatalf("TmuxPanes = %v (len %d), want [%%51] (only good pane)", paneIDsOf(got), len(got))
	}
	if got[0].PaneID != "%51" {
		t.Errorf("TmuxPanes[0].PaneID = %q, want %%51", got[0].PaneID)
	}
}

// ----------------------------------------------------------------------
// AC6 — host attribution via pane.Key.Host (feature scenario @ line 132)
// @integration @issue-511
// ----------------------------------------------------------------------

// TestTmuxPanesResolver_HostAttribution verifies that pane attribution uses
// pane.Key.Host (pane.window.session.host), not the local daemon's host id.
// A pane on host "B" matches a worktree on host "B" even when the local
// daemon is host "A" (AC6).
func TestTmuxPanesResolver_HostAttribution(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	// Build a resolver whose tmux adapter is tagged as host "B".
	const worktreePath = "/home/me/repo"
	const fakePID = 80001

	tr := &tmuxTestRunner{
		sessionRows: []string{sessionRow("main", 1700000000)},
		windowRows:  []string{windowRow("main")},
		paneRows:    []string{buildPaneRow("main", "%60", fakePID)},
	}

	// Build the resolver with a non-"local" hostID to simulate host "B".
	hostID := "B"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmuxAdapter := tmuxprovider.NewAdapter(tmuxprovider.HostID(hostID)).
		WithRunner(tr).
		WithSocket("/tmp/orchard-test-host-b.sock")
	tmuxProv := tmuxprovider.New(tmuxAdapter, nil)
	if err := tmuxProv.Refresh(ctx); err != nil {
		t.Fatalf("tmux Refresh: %v", err)
	}

	psRunner := &psTestRunner{cwdByPid: map[int]string{fakePID: worktreePath}}
	psAdapter := psprovider.NewAdapter(hostID).WithRunner(psRunner)
	psProv := psprovider.New(psAdapter, nil)
	if err := psProv.Start(ctx); err != nil {
		t.Fatalf("ps Start: %v", err)
	}

	r := &worktreeResolver{&Resolver{Tmux: tmuxProv, PS: psProv}}

	// Worktree on host "B" — attribution must use pane.Key.Host.
	wt := &graphql1.Worktree{
		ID:   "proj:repo",
		Path: worktreePath,
		Host: "B", // AC6: resolver reads pane.Key.Host, not local daemon host
	}

	got, err := r.TmuxPanes(ctx, wt)
	if err != nil {
		t.Fatalf("TmuxPanes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("TmuxPanes = %v (len %d), want [%%60]", paneIDsOf(got), len(got))
	}
	if got[0].PaneID != "%60" {
		t.Errorf("TmuxPanes[0].PaneID = %q, want %%60 (attribution via pane.Key.Host)", got[0].PaneID)
	}
}

// ----------------------------------------------------------------------
// AC6 — cross-host non-match (feature scenario @ line 141)
// @integration @issue-511
// ----------------------------------------------------------------------

// TestTmuxPanesResolver_CrossHostNoMatch verifies that a pane on host "B"
// does NOT match a worktree on host "A", even when the path matches (AC6).
func TestTmuxPanesResolver_CrossHostNoMatch(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	const worktreePath = "/Users/me/repo"
	const fakePID = 90001

	// Pane is on host "B" (the tmux adapter is tagged as "B").
	tr := &tmuxTestRunner{
		sessionRows: []string{sessionRow("main", 1700000000)},
		windowRows:  []string{windowRow("main")},
		paneRows:    []string{buildPaneRow("main", "%70", fakePID)},
	}

	hostB := "B"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmuxAdapter := tmuxprovider.NewAdapter(tmuxprovider.HostID(hostB)).
		WithRunner(tr).
		WithSocket("/tmp/orchard-test-cross-host.sock")
	tmuxProv := tmuxprovider.New(tmuxAdapter, nil)
	if err := tmuxProv.Refresh(ctx); err != nil {
		t.Fatalf("tmux Refresh: %v", err)
	}

	psRunner := &psTestRunner{cwdByPid: map[int]string{fakePID: worktreePath}}
	psAdapter := psprovider.NewAdapter(hostB).WithRunner(psRunner)
	psProv := psprovider.New(psAdapter, nil)
	if err := psProv.Start(ctx); err != nil {
		t.Fatalf("ps Start: %v", err)
	}

	r := &worktreeResolver{&Resolver{Tmux: tmuxProv, PS: psProv}}

	// Worktree is on host "A" — the pane on host "B" must NOT match.
	wt := &graphql1.Worktree{
		ID:   "proj:repo",
		Path: worktreePath,
		Host: "A", // different from the pane's host "B"
	}

	got, err := r.TmuxPanes(ctx, wt)
	if err != nil {
		t.Fatalf("TmuxPanes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("TmuxPanes = %v, want [] (cross-host pane must NOT match a host-A worktree)", paneIDsOf(got))
	}
}

// ----------------------------------------------------------------------
// AC8 — tmuxSession schema doc (feature scenario @ line 185)
// @unit @issue-511
// ----------------------------------------------------------------------

// TestWorktreeTmuxSession_SchemaDoc verifies that the Worktree.tmuxSession
// field doc string describes the most-recently-active selection and
// references issue #511 (AC8).
func TestWorktreeTmuxSession_SchemaDoc(t *testing.T) {
	sdl := SchemaSDL()

	// The field must be present in the SDL.
	if !strings.Contains(sdl, "tmuxSession: TmuxSession") {
		t.Errorf("schema SDL does not contain 'tmuxSession: TmuxSession'")
	}

	// The description must reference issue #511.
	if !strings.Contains(sdl, "#511") {
		t.Errorf("schema SDL does not reference '#511' near tmuxSession")
	}

	// The description must mention the most-recently-active semantics.
	if !strings.Contains(sdl, "most-recently-active") {
		t.Errorf("schema SDL does not describe most-recently-active semantics for tmuxSession")
	}
}

// ----------------------------------------------------------------------
// AC7 — Integration test: 4 worktrees × 6 panes (feature scenario @ line 152)
// @integration @issue-511
// ----------------------------------------------------------------------

// TestWorktreeTmuxJoin_Integration_AC7 is the definitive end-to-end join test:
// 4 worktrees and 6 panes are fed through the real resolver (same fake-provider
// harness used by other tests), and every assignment is checked.
//
// Pane assignments:
//
//	p1 (%101) — cwd "/repo/.worktrees/A"        → wt-A (exact match)
//	p2 (%102) — cwd "/repo/.worktrees/A/sub"    → wt-A (prefix match)
//	p3 (%103) — cwd "/repo/.worktrees/B"        → wt-B (exact match)
//	p4 (%104) — cwd "/elsewhere"                → no match
//	p5 (%105) — cwd "/repo"                     → no match (parent, not prefix)
//	p6 (%106) — cwd "" (null)                   → silently skipped (AC5)
//
// Session assignment (distinct activities so tmuxSession tie-break is exercised):
//
//	p1 lives in session "sess-p1" (lastActivityAt = T+100s — earlier)
//	p2 lives in session "sess-p2" (lastActivityAt = T+200s — later, wins)
//	p3 lives in session "sess-p3"
//	p4/p5/p6 live in session "sess-other"
//
// Placeholder-resolver failure contract: a stub returning (nil, nil) from both
// TmuxPanes and TmuxSession would fail the wt-A pane-count assertion,
// the wt-B pane assertion, and the wt-A tmuxSession non-nil assertion — at
// least three of the six assertions below.
//
// Darwin-only: cwd resolution via lsof is not wired on Linux yet.
func TestWorktreeTmuxJoin_Integration_AC7(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	// ── paths ────────────────────────────────────────────────────────────
	const (
		pathA = "/repo/.worktrees/A"
		pathB = "/repo/.worktrees/B"
		pathC = "/repo/.worktrees/C"
		pathD = "/repo/.worktrees/D"

		// p1 and p2 are in separate sessions with distinct activities so
		// wt-A.tmuxSession = sess-p2 (the more-recently-active one).
		activityP1 = int64(1746784800 + 100) // T+100s — earlier
		activityP2 = int64(1746784800 + 200) // T+200s — later, should win

		// Fake pids — distinct for every pane.
		pidP1 = 110001
		pidP2 = 110002
		pidP3 = 110003
		pidP4 = 110004
		pidP5 = 110005
		pidP6 = 110006
	)

	// ── fake tmux harness ────────────────────────────────────────────────
	tr := &tmuxTestRunner{
		sessionRows: []string{
			sessionRow("sess-p1", activityP1),
			sessionRow("sess-p2", activityP2),
			sessionRow("sess-p3", 1746784800),
			sessionRow("sess-other", 1700000000),
		},
		windowRows: []string{
			windowRow("sess-p1"),
			windowRow("sess-p2"),
			windowRow("sess-p3"),
			windowRow("sess-other"),
		},
		paneRows: []string{
			buildPaneRowAt("sess-p1", "%101", pidP1, activityP1),
			buildPaneRowAt("sess-p2", "%102", pidP2, activityP2),
			buildPaneRowAt("sess-p3", "%103", pidP3, 1746784800),
			buildPaneRowAt("sess-other", "%104", pidP4, 1700000000),
			buildPaneRowAt("sess-other", "%105", pidP5, 1700000000),
			buildPaneRowAt("sess-other", "%106", pidP6, 1700000000),
		},
	}

	// ── fake cwd map ──────────────────────────────────────────────────────
	// p6 maps to "" to simulate a null/unresolvable cwd (AC5 contract).
	r := buildResolver(t, tr, map[int]string{
		pidP1: pathA,
		pidP2: pathA + "/sub",
		pidP3: pathB,
		pidP4: "/elsewhere",
		pidP5: "/repo",
		pidP6: "", // null cwd — silently skipped
	})

	ctx := context.Background()

	// ── wt-A: expects [%101, %102] sorted by paneId, tmuxSession = sess-p2 ──
	wtA := &graphql1.Worktree{ID: "proj:wt-A", Path: pathA, Host: "local"}

	panesA, err := r.TmuxPanes(ctx, wtA)
	if err != nil {
		t.Fatalf("wt-A TmuxPanes: %v", err)
	}
	if len(panesA) != 2 {
		t.Fatalf("wt-A TmuxPanes = %v (len %d), want [%%101 %%102]", paneIDsOf(panesA), len(panesA))
	}
	if panesA[0].PaneID != "%101" {
		t.Errorf("wt-A TmuxPanes[0].PaneID = %q, want %%101 (sorted by paneId asc)", panesA[0].PaneID)
	}
	if panesA[1].PaneID != "%102" {
		t.Errorf("wt-A TmuxPanes[1].PaneID = %q, want %%102 (sorted by paneId asc)", panesA[1].PaneID)
	}

	sessA, err := r.TmuxSession(ctx, wtA)
	if err != nil {
		t.Fatalf("wt-A TmuxSession: %v", err)
	}
	if sessA == nil {
		t.Fatal("wt-A TmuxSession = nil, want non-nil (p2 in sess-p2 has higher lastActivityAt)")
	}
	// sess-p2 has activityP2 > activityP1, so it must win.
	if sessA.Name != "sess-p2" {
		t.Errorf("wt-A TmuxSession.Name = %q, want %q (most-recently-active session of p1/p2)", sessA.Name, "sess-p2")
	}

	// ── wt-B: expects [%103] ────────────────────────────────────────────
	wtB := &graphql1.Worktree{ID: "proj:wt-B", Path: pathB, Host: "local"}

	panesB, err := r.TmuxPanes(ctx, wtB)
	if err != nil {
		t.Fatalf("wt-B TmuxPanes: %v", err)
	}
	if len(panesB) != 1 {
		t.Fatalf("wt-B TmuxPanes = %v (len %d), want [%%103]", paneIDsOf(panesB), len(panesB))
	}
	if panesB[0].PaneID != "%103" {
		t.Errorf("wt-B TmuxPanes[0].PaneID = %q, want %%103", panesB[0].PaneID)
	}

	// ── wt-C: expects [] and nil session ────────────────────────────────
	wtC := &graphql1.Worktree{ID: "proj:wt-C", Path: pathC, Host: "local"}

	panesC, err := r.TmuxPanes(ctx, wtC)
	if err != nil {
		t.Fatalf("wt-C TmuxPanes: %v", err)
	}
	if len(panesC) != 0 {
		t.Errorf("wt-C TmuxPanes = %v, want [] (no panes match /repo/.worktrees/C)", paneIDsOf(panesC))
	}

	sessC, err := r.TmuxSession(ctx, wtC)
	if err != nil {
		t.Fatalf("wt-C TmuxSession: %v", err)
	}
	if sessC != nil {
		t.Errorf("wt-C TmuxSession = %v, want nil", sessC)
	}

	// ── wt-D: expects [] (empty, not nil) ───────────────────────────────
	wtD := &graphql1.Worktree{ID: "proj:wt-D", Path: pathD, Host: "local"}

	panesD, err := r.TmuxPanes(ctx, wtD)
	if err != nil {
		t.Fatalf("wt-D TmuxPanes: %v", err)
	}
	if len(panesD) != 0 {
		t.Errorf("wt-D TmuxPanes = %v, want [] (no panes match /repo/.worktrees/D)", paneIDsOf(panesD))
	}
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// paneIDsOf extracts pane IDs for diagnostic messages.
func paneIDsOf(panes []*graphql1.TmuxPane) []string {
	ids := make([]string, len(panes))
	for i, p := range panes {
		ids[i] = p.PaneID
	}
	return ids
}
