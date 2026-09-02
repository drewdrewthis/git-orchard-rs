// Regression + guard test for #612: the tmux field resolvers must not
// clone the provider's whole cached graph.
//
// Symptom: a cold lens load took ~60s on a busy host. Every tmux field
// resolver reached the cache through Provider.Snapshot(), which allocates
// four fresh maps and copies every session, window, pane and client into
// them. A `pane.window.session` traversal over ~40 worktrees therefore
// paid hundreds of whole-graph clones to answer one-shot keyed lookups.
//
// Guard: drive the tmux field-resolver surface against a seeded provider
// and assert Provider.SnapshotCalls() does not move. R3 forbids Snapshot()
// in a field resolver; O9 makes the hot-path allocation auditable.

package resolvers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
	tmuxprovider "github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// Fixture shape: 3 sessions x 2 windows x 2 panes = 12 panes, plus one
// attached client per session. Big enough that a per-field clone is
// unambiguous in the benchmark, small enough to stay a unit test.
const (
	fanoutSessions       = 3
	fanoutWindowsPerSess = 2
	fanoutPanesPerWindow = 2
	fanoutHost           = "local"
)

// fanoutTmuxRunner serves the three sub-commands FetchAll issues:
// list-sessions (the alive probe), list-panes -a, and list-clients.
type fanoutTmuxRunner struct {
	listAllOutput string
	clientsOutput string
}

func (r *fanoutTmuxRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "tmux" {
		return nil, fmt.Errorf("fanoutTmuxRunner: unexpected command %q", name)
	}
	switch firstNonFlagTmuxArg(args) {
	case "list-panes":
		return []byte(r.listAllOutput), nil
	case "list-clients":
		return []byte(r.clientsOutput), nil
	default:
		// list-sessions / info: exit 0 with no output = alive server.
		return []byte(""), nil
	}
}

// fanoutSessionName / fanoutPaneID name the fixture's nodes so assertions
// can address a specific one without re-deriving the layout.
func fanoutSessionName(s int) string { return fmt.Sprintf("sess-%d", s) }

func fanoutPaneID(s, w, p int) string {
	return fmt.Sprintf("%%%d", 1+p+fanoutPanesPerWindow*(w+fanoutWindowsPerSess*s))
}

// fanoutListAllRows renders the 18-field `list-panes -a` rows for the
// whole fixture, in the field order adapter.listAllFormat declares.
func fanoutListAllRows() string {
	var rows []string
	for s := 0; s < fanoutSessions; s++ {
		for w := 0; w < fanoutWindowsPerSess; w++ {
			for p := 0; p < fanoutPanesPerWindow; p++ {
				rows = append(rows, strings.Join([]string{
					fanoutSessionName(s),               // 0  session_name
					"1700000000",                       // 1  session_created
					"1",                                // 2  session_attached
					strconv.Itoa(1700000900 + s),       // 3  session_activity
					strconv.Itoa(fanoutWindowsPerSess), // 4  session_windows
					"0",                                // 5  session_window_index
					strconv.Itoa(w),                    // 6  window_index
					fmt.Sprintf("win-%d", w),           // 7  window_name
					map[bool]string{true: "1", false: "0"}[w == 0], // 8 window_active
					strconv.Itoa(fanoutPanesPerWindow),             // 9  window_panes
					fanoutPaneID(s, w, 0),                          // 10 window_active_pane
					fanoutPaneID(s, w, p),                          // 11 pane_id
					fmt.Sprintf("title-%d", p),                     // 12 pane_title
					"zsh",                                          // 13 pane_current_command
					strconv.Itoa(1000 + p),                         // 14 pane_pid
					"200",                                          // 15 pane_width
					"50",                                           // 16 pane_height
					"0",                                            // 17 pane_dead
				}, fieldSepTest))
			}
		}
	}
	return strings.Join(rows, "\n") + "\n"
}

// fanoutClientRows renders one 9-field `list-clients` row per session,
// each attached to that session's window 0 / pane 0.
func fanoutClientRows() string {
	var rows []string
	for s := 0; s < fanoutSessions; s++ {
		rows = append(rows, strings.Join([]string{
			fmt.Sprintf("client-%d", s),     // 0 client_name
			fanoutSessionName(s),            // 1 client_session
			fmt.Sprintf("/dev/ttys00%d", s), // 2 client_tty
			"xterm-256color",                // 3 client_termname
			"1700000000",                    // 4 client_created
			"1700000900",                    // 5 client_activity
			"0",                             // 6 client_readonly
			"0",                             // 7 window_index
			fanoutPaneID(s, 0, 0),           // 8 pane_id
		}, fieldSepTest))
	}
	return strings.Join(rows, "\n") + "\n"
}

// buildFanoutProvider returns a provider whose cache is hydrated from the
// fixture — no tmux daemon, no socket, no exec.
func buildFanoutProvider(tb testing.TB) *tmuxprovider.Provider {
	tb.Helper()
	runner := &fanoutTmuxRunner{
		listAllOutput: fanoutListAllRows(),
		clientsOutput: fanoutClientRows(),
	}
	adapter := tmuxprovider.NewAdapter(tmuxprovider.HostID(fanoutHost)).
		WithRunner(runner).
		WithSocket("/tmp/orchard-test-snapshot-fanout.sock")
	prov := tmuxprovider.New(adapter, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := prov.Refresh(ctx); err != nil {
		tb.Fatalf("tmux Refresh: %v", err)
	}
	return prov
}

// walkTmuxFields drives every tmux field resolver that answers a keyed or
// intra-session question, returning how many pane->window->session chains
// resolved end to end. Errors and missing nodes fail the test, so a zero
// SnapshotCalls() count can never be earned by resolving nothing.
func walkTmuxFields(tb testing.TB, r *Resolver) int {
	tb.Helper()
	ctx := context.Background()

	paneR := &tmuxPaneResolver{r}
	winR := &tmuxWindowResolver{r}
	sessR := &tmuxSessionResolver{r}
	clientR := &tmuxClientResolver{r}

	chains := 0
	for s := 0; s < fanoutSessions; s++ {
		for w := 0; w < fanoutWindowsPerSess; w++ {
			for p := 0; p < fanoutPanesPerWindow; p++ {
				paneObj := &graphql1.TmuxPane{ID: "TmuxPane:" + fanoutHost + ":" + fanoutPaneID(s, w, p)}

				title, err := paneR.Title(ctx, paneObj)
				if err != nil {
					tb.Fatalf("pane.title: %v", err)
				}
				if want := fmt.Sprintf("title-%d", p); title != want {
					tb.Fatalf("pane.title = %q, want %q", title, want)
				}

				winObj, err := paneR.Window(ctx, paneObj)
				if err != nil {
					tb.Fatalf("pane.window: %v", err)
				}
				if winObj == nil {
					tb.Fatalf("pane.window = nil for pane %s", paneObj.ID)
				}
				wantWin := "TmuxWindow:" + fanoutHost + ":" + fanoutSessionName(s) + ":" + strconv.Itoa(w)
				if winObj.ID != wantWin {
					tb.Fatalf("pane.window.id = %q, want %q", winObj.ID, wantWin)
				}

				sessObj, err := winR.Session(ctx, winObj)
				if err != nil {
					tb.Fatalf("window.session: %v", err)
				}
				if sessObj == nil {
					tb.Fatalf("window.session = nil for window %s", winObj.ID)
				}
				wantSess := "TmuxSession:" + fanoutHost + ":" + fanoutSessionName(s)
				if sessObj.ID != wantSess {
					tb.Fatalf("window.session.id = %q, want %q", sessObj.ID, wantSess)
				}
				chains++

				// Collection edges off the window and session.
				panes, err := winR.Panes(ctx, winObj)
				if err != nil {
					tb.Fatalf("window.panes: %v", err)
				}
				if len(panes) != fanoutPanesPerWindow {
					tb.Fatalf("window.panes = %d panes, want %d", len(panes), fanoutPanesPerWindow)
				}
				windows, err := sessR.Windows(ctx, sessObj)
				if err != nil {
					tb.Fatalf("session.windows: %v", err)
				}
				if len(windows) != fanoutWindowsPerSess {
					tb.Fatalf("session.windows = %d windows, want %d", len(windows), fanoutWindowsPerSess)
				}
				attached, err := sessR.AttachedClients(ctx, sessObj)
				if err != nil {
					tb.Fatalf("session.attachedClients: %v", err)
				}
				if len(attached) != 1 {
					tb.Fatalf("session.attachedClients = %d clients, want 1", len(attached))
				}
				watching, err := paneR.WatchingClients(ctx, paneObj)
				if err != nil {
					tb.Fatalf("pane.watchingClients: %v", err)
				}
				wantWatching := 0
				if w == 0 && p == 0 {
					wantWatching = 1
				}
				if len(watching) != wantWatching {
					tb.Fatalf("pane %s watchingClients = %d, want %d", paneObj.ID, len(watching), wantWatching)
				}

				// Keyed "current" edges.
				curWin, err := sessR.CurrentWindow(ctx, sessObj)
				if err != nil {
					tb.Fatalf("session.currentWindow: %v", err)
				}
				if curWin == nil {
					tb.Fatalf("session.currentWindow = nil for %s", sessObj.ID)
				}
				curPane, err := winR.CurrentPane(ctx, winObj)
				if err != nil {
					tb.Fatalf("window.currentPane: %v", err)
				}
				if curPane == nil {
					tb.Fatalf("window.currentPane = nil for %s", winObj.ID)
				}
				if want := fanoutPaneID(s, w, 0); curPane.PaneID != want {
					tb.Fatalf("window.currentPane = %q, want %q", curPane.PaneID, want)
				}
			}
		}

		// Client edges.
		clientObj := &graphql1.TmuxClient{ID: fmt.Sprintf("TmuxClient:%s:client-%d", fanoutHost, s)}
		clientSess, err := clientR.Session(ctx, clientObj)
		if err != nil {
			tb.Fatalf("client.session: %v", err)
		}
		if clientSess == nil || clientSess.Name != fanoutSessionName(s) {
			tb.Fatalf("client.session = %+v, want session %q", clientSess, fanoutSessionName(s))
		}
		if _, err := clientR.CurrentWindow(ctx, clientObj); err != nil {
			tb.Fatalf("client.currentWindow: %v", err)
		}
		if _, err := clientR.CurrentPane(ctx, clientObj); err != nil {
			tb.Fatalf("client.currentPane: %v", err)
		}
	}
	return chains
}

// TestTmuxFieldResolvers_NoSnapshotClone_Issue612 asserts that walking the
// tmux field-resolver surface never calls Provider.Snapshot() — the
// whole-graph clone behind the ~60s cold lens load.
//
// FAILS on unpatched code (every lookup calls Snapshot(); the count runs
// into the dozens for this fixture and into the hundreds on a real host).
// PASSES once the resolvers read through narrow keyed/filtered accessors.
func TestTmuxFieldResolvers_NoSnapshotClone_Issue612(t *testing.T) {
	prov := buildFanoutProvider(t)
	r := &Resolver{Tmux: prov}

	before := prov.SnapshotCalls()
	chains := walkTmuxFields(t, r)

	wantChains := fanoutSessions * fanoutWindowsPerSess * fanoutPanesPerWindow
	if chains != wantChains {
		t.Fatalf("resolved %d pane->window->session chains, want %d", chains, wantChains)
	}

	if got := prov.SnapshotCalls() - before; got != 0 {
		t.Errorf(
			"issue #612 regression: %d pane->window->session traversals made %d Snapshot() calls, want 0.\n"+
				"  Snapshot() clones every session, window, pane and client map. A field resolver\n"+
				"  must read through a narrow keyed/filtered accessor instead (RULES.md R3, O9).",
			chains, got,
		)
	}
}

// TestWorktreeTmuxResolvers_NoSnapshotClone_Issue612 covers the other half
// of the #612 fanout: Worktree.tmuxPanes / Worktree.tmuxSession cloned the
// whole graph once per worktree, and the reporting host had ~40 worktrees.
//
// Panes here resolve no cwd (r.PS is nil), so the join legitimately yields
// nothing — the assertion under test is the clone count, and the sibling
// worktree_tmux_test.go suite covers join correctness with a live ps stub.
func TestWorktreeTmuxResolvers_NoSnapshotClone_Issue612(t *testing.T) {
	prov := buildFanoutProvider(t)
	r := &worktreeResolver{&Resolver{Tmux: prov}}
	ctx := context.Background()

	before := prov.SnapshotCalls()
	const worktrees = 40
	for i := 0; i < worktrees; i++ {
		obj := &graphql1.Worktree{
			ID:   fmt.Sprintf("Worktree:%s:/repo/wt-%d", fanoutHost, i),
			Host: fanoutHost,
			Path: fmt.Sprintf("/repo/wt-%d", i),
		}
		if _, err := r.TmuxPanes(ctx, obj); err != nil {
			t.Fatalf("worktree.tmuxPanes: %v", err)
		}
		if _, err := r.TmuxSession(ctx, obj); err != nil {
			t.Fatalf("worktree.tmuxSession: %v", err)
		}
	}

	if got := prov.SnapshotCalls() - before; got != 0 {
		t.Errorf(
			"issue #612 regression: %d worktrees made %d Snapshot() calls, want 0.\n"+
				"  Worktree.tmuxPanes/.tmuxSession must read the candidate panes through a\n"+
				"  narrow accessor, not clone the whole tmux graph per worktree (RULES.md R3, O9).",
			worktrees, got,
		)
	}
}

// TestTmuxCollectionResolvers_NoSnapshotClone_Issue612 covers the list
// resolvers. Each read one map and cloned four to get it — one wasted
// clone per request rather than per field, but on the same cold-lens
// path, and the lens query opens with tmuxServer { sessions }.
//
// Snapshot() has one legitimate caller left in the read path (the web
// dashboard in server.go, which genuinely reads several maps at once).
// If a resolver here ever needs it back, change this test deliberately.
func TestTmuxCollectionResolvers_NoSnapshotClone_Issue612(t *testing.T) {
	prov := buildFanoutProvider(t)
	r := &Resolver{Tmux: prov}
	ctx := context.Background()

	before := prov.SnapshotCalls()

	qr := &queryResolver{r}
	sessions, err := qr.TmuxSessions(ctx, nil)
	if err != nil {
		t.Fatalf("Query.tmuxSessions: %v", err)
	}
	if len(sessions) != fanoutSessions {
		t.Fatalf("Query.tmuxSessions = %d sessions, want %d", len(sessions), fanoutSessions)
	}

	panes, err := qr.TmuxPanes(ctx, nil)
	if err != nil {
		t.Fatalf("Query.tmuxPanes: %v", err)
	}
	wantPanes := fanoutSessions * fanoutWindowsPerSess * fanoutPanesPerWindow
	if len(panes) != wantPanes {
		t.Fatalf("Query.tmuxPanes = %d panes, want %d", len(panes), wantPanes)
	}

	serverR := &tmuxServerResolver{r}
	srvSessions, err := serverR.Sessions(ctx, nil, nil)
	if err != nil {
		t.Fatalf("tmuxServer.sessions: %v", err)
	}
	if len(srvSessions) != fanoutSessions {
		t.Fatalf("tmuxServer.sessions = %d sessions, want %d", len(srvSessions), fanoutSessions)
	}

	srvClients, err := serverR.Clients(ctx, nil)
	if err != nil {
		t.Fatalf("tmuxServer.clients: %v", err)
	}
	if len(srvClients) != fanoutSessions {
		t.Fatalf("tmuxServer.clients = %d clients, want %d", len(srvClients), fanoutSessions)
	}

	if got := prov.SnapshotCalls() - before; got != 0 {
		t.Errorf(
			"issue #612 regression: the tmux collection resolvers made %d Snapshot() calls, want 0.\n"+
				"  Each reads one store; Snapshot() clones all four (RULES.md O9).",
			got,
		)
	}
}

// BenchmarkTmuxPaneWindowSessionChain measures the per-traversal allocation
// cost of the field-resolver surface. It is the O9 audit number quoted in
// the #612 PR: before the fix each field pays a whole-graph map clone.
func BenchmarkTmuxPaneWindowSessionChain(b *testing.B) {
	prov := buildFanoutProvider(b)
	r := &Resolver{Tmux: prov}
	ctx := context.Background()
	paneR := &tmuxPaneResolver{r}
	winR := &tmuxWindowResolver{r}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		paneObj := &graphql1.TmuxPane{ID: "TmuxPane:" + fanoutHost + ":" + fanoutPaneID(0, 0, 0)}
		winObj, err := paneR.Window(ctx, paneObj)
		if err != nil || winObj == nil {
			b.Fatalf("pane.window: %v (obj=%v)", err, winObj)
		}
		sessObj, err := winR.Session(ctx, winObj)
		if err != nil || sessObj == nil {
			b.Fatalf("window.session: %v (obj=%v)", err, sessObj)
		}
	}
}
