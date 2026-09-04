package resolvers_test

// End-to-end test for Worktree.tmuxPanes + Worktree.tmuxSession — issue #511.
//
// What's "E2E" here:
//   - Two minimal on-disk git layouts are synthesised (no `git` binary).
//   - A fake tmux runner serves two panes: %201 (cwd = wt-live path) and
//     %202 (cwd = /tmp/elsewhere — no worktree match).
//   - A fake PS runner maps each pane's foreground pid to a cwd.
//   - The resolvers, DataLoader middleware, and gqlgen schema are all live.
//   - We POST the query over HTTP and assert the JSON response shape.
//
// Scenario (feature file line ~196):
//
//	Given the daemon has two worktrees: wt-live and wt-empty
//	And a tmux pane %201 has its cwd == wt-live's path (via fake PS runner)
//	And pane %202's cwd is /tmp/elsewhere (no match)
//	When we query { projects { worktrees { id path tmuxPanes { paneId window { session { name } } } tmuxSession { name lastActivityAt } } } }
//	Then wt-live.tmuxPanes has length ≥ 1 and contains %201
//	And  wt-live.tmuxSession.name == live session name
//	And  wt-live.tmuxSession.lastActivityAt is non-null
//	And  wt-empty.tmuxPanes == []
//	And  wt-empty.tmuxSession == null

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"

	gqlgen "github.com/drewdrewthis/orchardist/internal/server/graphql"
	"github.com/drewdrewthis/orchardist/internal/server/loaders"
	configprovider "github.com/drewdrewthis/orchardist/internal/server/providers/config"
	gitprovider "github.com/drewdrewthis/orchardist/internal/server/providers/git"
	psprovider "github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	tmuxprovider "github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
	"github.com/drewdrewthis/orchardist/internal/server/resolvers"
)

// ----------------------------------------------------------------------
// Fake providers — local to this package (resolvers_test cannot use
// the unexported fakes in worktree_tmux_test.go).
// ----------------------------------------------------------------------

// e2eTmuxRunner is a CommandRunner for the tmux adapter. It serves
// list-sessions, list-windows, list-panes, and the alive probe. Rows are
// joined with tmuxprovider.FieldSep — the adapter's own constant — so the
// stub cannot drift from the wire format (#664, #712).
type e2eTmuxRunner struct {
	paneRows    []string
	sessionRows []string
	windowRows  []string
}

func (r *e2eTmuxRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "tmux" {
		return nil, fmt.Errorf("unexpected command: %s", name)
	}
	sub := e2eFirstNonFlagTmuxArg(args)
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

// e2eFirstNonFlagTmuxArg returns the first positional arg, skipping -S / -L pairs.
func e2eFirstNonFlagTmuxArg(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-S" || args[i] == "-L" {
			i++ // skip value
			continue
		}
		return args[i]
	}
	return ""
}

// e2ePaneRow builds a list-panes row in the tmux adapter's listAllFormat
// (tmuxprovider.ListAllFieldCount fields, FieldSep-separated). The adapter
// consolidated list-sessions, list-windows, and list-panes into a single
// `tmux list-panes -a -F <listAllFormat>` call (#511 follow-up); session
// metadata is emitted on every pane row.
func e2ePaneRow(session, paneID string, pid int) string {
	return e2ePaneRowAt(session, paneID, pid, 1700000000)
}

// e2ePaneRowAt is e2ePaneRow with parameterized session_activity (the field
// the tmux adapter uses to drive lastActivityAt → tmuxSession most-recent
// selection).
func e2ePaneRowAt(session, paneID string, pid int, activityUnix int64) string {
	return strings.Join([]string{
		session,                               // 0  session_name
		"1700000000",                          // 1  session_created
		"0",                                   // 2  session_attached
		fmt.Sprintf("%d", activityUnix),       // 3  session_activity
		"1",                                   // 4  session_windows
		"0",                                   // 5  session_window_index
		"0",                                   // 6  window_index
		"main",                                // 7  window_name
		"1",                                   // 8  window_active
		"1",                                   // 9  window_panes
		paneID,                                // 10 window_active_pane
		paneID,                                // 11 pane_id
		"title",                               // 12 pane_title
		"zsh",                                 // 13 pane_current_command
		fmt.Sprintf("%d", pid),                // 14 pane_pid
		"200",                                 // 15 pane_width
		"50",                                  // 16 pane_height
		"0",                                   // 17 pane_dead
	}, tmuxprovider.FieldSep)
}

// e2eSessionRow builds a list-sessions row.
// Fields: name, created, attached, activity, windows, curwindow
func e2eSessionRow(name string, activityUnix int64) string {
	return strings.Join([]string{
		name,
		"1700000000",
		"0",
		fmt.Sprintf("%d", activityUnix),
		"1",
		"0",
	}, tmuxprovider.FieldSep)
}

// e2eWindowRow builds a list-windows row.
// Fields: session, index, name, active, panes, curpane
func e2eWindowRow(session string) string {
	return strings.Join([]string{session, "0", "main", "1", "1", "%1"}, tmuxprovider.FieldSep)
}

// e2ePsRunner is a CommandRunner for the ps and lsof adapters.
// It maps pid → cwd for lsof calls and returns a minimal ps header.
type e2ePsRunner struct {
	cwdByPid map[int]string
}

const e2ePsHeader = "  PID  PPID USER             TT  %CPU    RSS                 STARTED COMMAND\n"

func (r *e2ePsRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	switch name {
	case "ps":
		return []byte(e2ePsHeader), nil
	case "lsof":
		// Production batches cwd lookups: fetchCwdsDarwin issues a single
		// `lsof -a -d cwd -p pid1,pid2,... -F n` and expects one p<pid>\nn<cwd>
		// block per pid. The stub MUST emit a block for every requested pid,
		// not just the first — otherwise, when two panes' cwd Loads coalesce
		// into one batch, only one pid resolves and which one is nondeterministic
		// (map iteration order), flaking the worktree↔pane join (issue #773).
		pids := e2eParseLsofPids(args)
		var b strings.Builder
		anyMissing := false
		for _, pid := range pids {
			if cwd, ok := r.cwdByPid[pid]; ok {
				fmt.Fprintf(&b, "p%d\nn%s\n", pid, cwd)
			} else {
				anyMissing = true
			}
		}
		if b.Len() == 0 {
			// Real lsof exits non-zero when no pid matches; fetchCwdsDarwin
			// treats (err, empty output) as "nulls for all keys".
			return nil, fmt.Errorf("lsof: no entries for pids %v", pids)
		}
		if anyMissing {
			return []byte(b.String()), fmt.Errorf("lsof: partial output, some pids missing: %v", pids)
		}
		return []byte(b.String()), nil
	default:
		return nil, fmt.Errorf("unexpected command: %s", name)
	}
}

// e2eParseLsofPids extracts every pid from `lsof -a -d cwd -p pid1,pid2,... -F n`.
// Production joins the batch's pids with commas, so the stub must parse them all.
func e2eParseLsofPids(args []string) []int {
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			out := make([]int, 0, 4)
			for _, s := range strings.Split(args[i+1], ",") {
				var n int
				if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
					out = append(out, n)
				}
			}
			return out
		}
	}
	return nil
}

// ----------------------------------------------------------------------
// Git layout helpers — minimal on-disk layout the git provider can read.
// ----------------------------------------------------------------------

// buildMinimalGitLayout writes a minimal bare-enough git dir under dir
// so that gitprovider.Provider.AddProject succeeds and returns a main
// worktree whose Path equals dir.
//
// Layout produced (under dir):
//
//	dir/
//	  .git/
//	    HEAD      — "ref: refs/heads/main\n" (unborn OK — bare=true)
func buildMinimalGitLayout(t *testing.T, dir string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir .git in %s: %v", dir, err)
	}
	head := "ref: refs/heads/main\n"
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(head), 0o644); err != nil {
		t.Fatalf("write HEAD in %s: %v", dir, err)
	}
}

// ----------------------------------------------------------------------
// Static projects lister (re-declared here; resolvers_test cannot share
// the one in worktree_dashboard_e2e_test.go because both are in the same
// package — they are the same type, so we reuse staticReposListerE2E
// already declared in that file).
// ----------------------------------------------------------------------
// NOTE: staticReposListerE2E is already declared in
// worktree_dashboard_e2e_test.go (same package). We reuse it directly.

// ----------------------------------------------------------------------
// E2E test — feature file line 196
// ----------------------------------------------------------------------

// TestWorktreeTmuxJoin_E2E_LiveQuery proves the full schema → resolver →
// provider wiring end-to-end for Worktree.tmuxPanes and
// Worktree.tmuxSession (#511).
//
// Darwin-only: cwd resolution via lsof is not wired on Linux yet.
func TestWorktreeTmuxJoin_E2E_LiveQuery(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cwd resolution via lsof is darwin-only (Linux /proc path not yet wired)")
	}

	// ── 1. Build minimal on-disk git layouts ──────────────────────────────
	//
	// livePath  — the worktree where the fake shell sits.
	// emptyPath — another worktree with no shell.
	livePath := t.TempDir()
	emptyPath := t.TempDir()

	buildMinimalGitLayout(t, livePath)
	buildMinimalGitLayout(t, emptyPath)

	// ── 2. Register both as separate projects in the git provider ─────────
	gitProv := gitprovider.NewProvider(nil)
	t.Cleanup(gitProv.Stop)

	const liveProjectID = "proj-live"
	const emptyProjectID = "proj-empty"

	if err := gitProv.AddProject(gitprovider.Project{ID: liveProjectID, Dir: livePath}); err != nil {
		t.Fatalf("AddProject live: %v", err)
	}
	if err := gitProv.AddProject(gitprovider.Project{ID: emptyProjectID, Dir: emptyPath}); err != nil {
		t.Fatalf("AddProject empty: %v", err)
	}

	// ── 3. Static repos lister ────────────────────────────────────────────
	repos := &staticReposListerE2E{
		records: []configprovider.Repo{
			{ID: configprovider.RepoID(liveProjectID), Slug: liveProjectID, Path: livePath},
			{ID: configprovider.RepoID(emptyProjectID), Slug: emptyProjectID, Path: emptyPath},
		},
	}

	// ── 4. Fake tmux harness ──────────────────────────────────────────────
	//
	// live-session has pane %201 whose foreground pid resolves to livePath.
	// other-session has pane %202 whose cwd is /tmp/elsewhere (no match).
	const liveSession = "live-session"
	const livePaneID = "%201"
	const livePID = 201001
	const otherSession = "other-session"
	const elsewherePaneID = "%202"
	const elsewherePID = 202001
	const liveActivity = int64(1746784800) // fixed timestamp for assertions

	tr := &e2eTmuxRunner{
		sessionRows: []string{
			e2eSessionRow(liveSession, liveActivity),
			e2eSessionRow(otherSession, 1700000000),
		},
		windowRows: []string{
			e2eWindowRow(liveSession),
			e2eWindowRow(otherSession),
		},
		paneRows: []string{
			e2ePaneRow(liveSession, livePaneID, livePID),
			e2ePaneRow(otherSession, elsewherePaneID, elsewherePID),
		},
	}

	const hostID = "local"

	tmuxAdapter := tmuxprovider.NewAdapter(tmuxprovider.HostID(hostID)).
		WithRunner(tr).
		WithSocket("/tmp/orchard-test-tmux-e2e.sock")
	tmuxProv := tmuxprovider.New(tmuxAdapter, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := tmuxProv.Refresh(ctx); err != nil {
		t.Fatalf("tmux Refresh: %v", err)
	}

	// ── 5. Fake PS runner ─────────────────────────────────────────────────
	//
	// livePID resolves to livePath (pane %201 sits in the live worktree).
	// elsewherePID resolves to /tmp/elsewhere (no worktree match).
	psRunner := &e2ePsRunner{
		cwdByPid: map[int]string{
			livePID:      livePath,
			elsewherePID: "/tmp/elsewhere",
		},
	}
	psAdapter := psprovider.NewAdapter(hostID).WithRunner(psRunner)
	psProv := psprovider.New(psAdapter, nil)
	if err := psProv.Start(ctx); err != nil {
		t.Fatalf("ps Start: %v", err)
	}

	// ── 6. Wire resolver ──────────────────────────────────────────────────
	r := resolvers.New(time.Now()).
		WithGit(gitProv).
		WithTmux(tmuxProv).
		WithPS(psProv).
		WithRepos(repos)

	gqlSrv := handler.New(gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: r}))
	gqlSrv.AddTransport(transport.POST{})

	ts := httptest.NewServer(loaders.Middleware(r.LoaderBundle(), gqlSrv))
	t.Cleanup(ts.Close)

	// ── 7. Fire the query ─────────────────────────────────────────────────
	const q = `{
		repos {
			worktrees {
				id
				path
				tmuxPanes {
					paneId
					window {
						session {
							name
						}
					}
				}
				tmuxSession {
					name
					lastActivityAt
				}
			}
		}
	}`

	body, _ := json.Marshal(map[string]string{"query": q})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("graphql request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// ── 8. Assert HTTP 200 ────────────────────────────────────────────────
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("http status = %d, want 200", resp.StatusCode)
	}

	// ── 9. Decode response ────────────────────────────────────────────────
	type tmuxPaneShape struct {
		PaneID string `json:"paneId"`
		Window *struct {
			Session *struct {
				Name string `json:"name"`
			} `json:"session"`
		} `json:"window"`
	}
	type tmuxSessionShape struct {
		Name           string  `json:"name"`
		LastActivityAt *string `json:"lastActivityAt"`
	}
	type worktreeShape struct {
		ID          string            `json:"id"`
		Path        string            `json:"path"`
		TmuxPanes   []tmuxPaneShape   `json:"tmuxPanes"`
		TmuxSession *tmuxSessionShape `json:"tmuxSession"`
	}
	type projectShape struct {
		Worktrees []worktreeShape `json:"worktrees"`
	}
	var out struct {
		Data struct {
			Repos []projectShape `json:"repos"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// ── 10. No GraphQL errors ─────────────────────────────────────────────
	if len(out.Errors) > 0 {
		t.Fatalf("graphql errors: %+v", out.Errors)
	}

	// ── 11. Collect all worktrees from both projects ───────────────────────
	var allWorktrees []worktreeShape
	for _, proj := range out.Data.Repos {
		allWorktrees = append(allWorktrees, proj.Worktrees...)
	}

	if len(allWorktrees) == 0 {
		t.Fatal("expected at least two worktrees across projects, got none")
	}

	// ── 12. Find wt-live and wt-empty by path ─────────────────────────────
	var wtLive, wtEmpty *worktreeShape
	for i := range allWorktrees {
		wt := &allWorktrees[i]
		switch wt.Path {
		case livePath:
			wtLive = wt
		case emptyPath:
			wtEmpty = wt
		}
	}

	if wtLive == nil {
		paths := make([]string, 0, len(allWorktrees))
		for _, wt := range allWorktrees {
			paths = append(paths, fmt.Sprintf("%q", wt.Path))
		}
		t.Fatalf("no worktree with path %q; got paths: %v", livePath, paths)
	}
	if wtEmpty == nil {
		paths := make([]string, 0, len(allWorktrees))
		for _, wt := range allWorktrees {
			paths = append(paths, fmt.Sprintf("%q", wt.Path))
		}
		t.Fatalf("no worktree with path %q; got paths: %v", emptyPath, paths)
	}

	// ── 13. Assert wt-live: tmuxPanes ≥ 1 and contains %201 ──────────────
	if len(wtLive.TmuxPanes) == 0 {
		t.Fatalf("wt-live.tmuxPanes = [], want at least one pane (%%201)")
	}

	foundLivePane := false
	var liveSessionNameFromPane string
	for _, pane := range wtLive.TmuxPanes {
		if pane.PaneID == livePaneID {
			foundLivePane = true
			if pane.Window != nil && pane.Window.Session != nil {
				liveSessionNameFromPane = pane.Window.Session.Name
			}
			break
		}
	}
	if !foundLivePane {
		paneIDs := make([]string, 0, len(wtLive.TmuxPanes))
		for _, p := range wtLive.TmuxPanes {
			paneIDs = append(paneIDs, p.PaneID)
		}
		t.Errorf("wt-live.tmuxPanes does not contain %%201; got panes: %v", paneIDs)
	}

	// ── 14. Assert wt-live: tmuxSession non-null and name matches pane's session ──
	if wtLive.TmuxSession == nil {
		t.Fatal("wt-live.tmuxSession = null, want non-null")
	}

	if liveSessionNameFromPane != "" && wtLive.TmuxSession.Name != liveSessionNameFromPane {
		t.Errorf("wt-live.tmuxSession.name = %q, want %q (pane's session name)",
			wtLive.TmuxSession.Name, liveSessionNameFromPane)
	}
	if wtLive.TmuxSession.Name != liveSession {
		t.Errorf("wt-live.tmuxSession.name = %q, want %q", wtLive.TmuxSession.Name, liveSession)
	}

	// ── 15. Assert wt-live: lastActivityAt is non-null ────────────────────
	if wtLive.TmuxSession.LastActivityAt == nil {
		t.Error("wt-live.tmuxSession.lastActivityAt = null, want non-null")
	}

	// ── 16. Assert wt-empty: tmuxPanes == [] ──────────────────────────────
	if len(wtEmpty.TmuxPanes) != 0 {
		paneIDs := make([]string, 0, len(wtEmpty.TmuxPanes))
		for _, p := range wtEmpty.TmuxPanes {
			paneIDs = append(paneIDs, p.PaneID)
		}
		t.Errorf("wt-empty.tmuxPanes = %v, want [] (no shell sits in the empty worktree)", paneIDs)
	}

	// ── 17. Assert wt-empty: tmuxSession == null ──────────────────────────
	if wtEmpty.TmuxSession != nil {
		t.Errorf("wt-empty.tmuxSession = {name: %q}, want null", wtEmpty.TmuxSession.Name)
	}
}
