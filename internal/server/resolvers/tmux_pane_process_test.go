package resolvers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
	psprovider "github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	tmuxprovider "github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// --------------------------------------------------------------------------
// Fake tmux CommandRunner
// --------------------------------------------------------------------------

// fakeTmuxRunner drives the tmux adapter without a real tmux server.
// It serves the minimum set of tmux sub-commands FetchAll needs.
type fakeTmuxRunner struct {
	hostID string
	// paneLines are already-formatted pane rows in the FieldSep-separated
	// listAllFormat that listAll expects.
	paneLines []string
}

// fieldSepTest is the adapter's own separator, not a copy of it. #662 changed
// the real separator from \x01 to \t (tmux 3.x discovery fix) without updating
// these fakes, so every seeded pane row silently failed the 18-field parse and
// the providers came up empty — the tests here asserted against an empty
// provider. Binding to tmuxprovider.FieldSep makes that class of drift
// impossible; the Go CI job added with #712 makes it loud if it recurs.
const fieldSepTest = tmuxprovider.FieldSep

func (f *fakeTmuxRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "tmux" {
		return nil, fmt.Errorf("fakeTmuxRunner: unexpected command %q", name)
	}
	// Skip any leading -S <socket> flags to get the sub-command.
	sub := ""
	for _, a := range args {
		if a == "info" || a == "list-sessions" || a == "list-windows" ||
			a == "list-panes" || a == "list-clients" || a == "display-message" {
			sub = a
			break
		}
	}
	switch sub {
	case "info":
		return []byte("ok"), nil
	case "list-sessions":
		// Return one dummy session so windows/panes parse correctly.
		line := "test-session" + fieldSepTest + "1746000000" + fieldSepTest +
			"0" + fieldSepTest + "1746000000" + fieldSepTest + "1" + fieldSepTest + "0"
		return []byte(line + "\n"), nil
	case "list-windows":
		// One window for session "test-session", index 0.
		line := "test-session" + fieldSepTest + "0" + fieldSepTest +
			"test-window" + fieldSepTest + "1" + fieldSepTest + "1" + fieldSepTest + "%0"
		return []byte(line + "\n"), nil
	case "list-panes":
		if len(f.paneLines) == 0 {
			return []byte{}, nil
		}
		return []byte(strings.Join(f.paneLines, "\n") + "\n"), nil
	case "list-clients":
		return []byte{}, nil
	case "display-message":
		return []byte("1\n"), nil
	default:
		return nil, fmt.Errorf("fakeTmuxRunner: unhandled tmux sub-command (args=%v)", args)
	}
}

// paneRow builds a single pane line in the 18-field format `tmux list-panes
// -a -F <listAllFormat>` emits (tmux/adapter.go:406). sessionName, paneID,
// and pid are the only fields the tests assert on; the rest get safe defaults.
// listAll consolidated list-sessions + list-panes into one call (#511 follow-up),
// so every pane row carries its session metadata too.
func paneRow(sessionName, paneID string, pid int) string {
	return paneRowWithCommand(sessionName, paneID, pid, "zsh")
}

// paneRowWithCommand is paneRow with an explicit pane_current_command — the
// field tmux fills with the pane's FOREGROUND process, which differs from the
// pane's root process (pane_pid) whenever a session is launched via a shell
// wrapper (#706).
func paneRowWithCommand(sessionName, paneID string, pid int, currentCommand string) string {
	return strings.Join([]string{
		sessionName,                   // 0  session_name
		"1700000000",                  // 1  session_created
		"0",                           // 2  session_attached
		"1700000000",                  // 3  session_activity
		"1",                           // 4  session_windows
		"0",                           // 5  session_window_index
		"0",                           // 6  window_index
		"main",                        // 7  window_name
		"1",                           // 8  window_active
		"1",                           // 9  window_panes
		paneID,                        // 10 window_active_pane
		paneID,                        // 11 pane_id
		"",                            // 12 pane_title
		currentCommand,                // 13 pane_current_command
		fmt.Sprintf("%d", pid),        // 14 pane_pid
		"80",                          // 15 pane_width
		"24",                          // 16 pane_height
		"0",                           // 17 pane_dead
	}, fieldSepTest)
}

// --------------------------------------------------------------------------
// Fake ps CommandRunner
// --------------------------------------------------------------------------

// psRow holds a single process row for the fake ps runner. ppid is optional:
// when 0 it defaults to 1 (init), so single-process rows need not set it;
// descendant-tree tests (#711) set it to build a real bash -> claude parentage.
type psRow struct {
	pid  int
	ppid int
	cmd  string
}

// fakePsRunnerProcess drives the ps adapter with a fixed single-process list.
type fakePsRunnerProcess struct {
	hostID string
	rows   []psRow
}

const psHeaderTest = "PID PPID USER TTY %CPU RSS STARTED COMMAND"

func (f *fakePsRunnerProcess) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "ps" {
		return nil, fmt.Errorf("fakePsRunner: unexpected command %q", name)
	}
	lines := []string{psHeaderTest}
	for _, r := range f.rows {
		ppid := r.ppid
		if ppid == 0 {
			ppid = 1
		}
		// Columns: PID PPID USER TTY %CPU RSS STARTED COMMAND
		lines = append(lines, fmt.Sprintf(
			"%d %d root ?? 0.0 1024 Sun May  4 10:00:00 2026 %s",
			r.pid, ppid, r.cmd,
		))
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// --------------------------------------------------------------------------
// Test helpers
// --------------------------------------------------------------------------

const testHost = "local"

// buildTmuxProvider creates a tmux.Provider seeded with the given pane rows
// and starts it (initial FetchAll from the fake runner).
func buildTmuxProvider(t *testing.T, rows []string) *tmuxprovider.Provider {
	t.Helper()
	runner := &fakeTmuxRunner{hostID: testHost, paneLines: rows}
	adapter := tmuxprovider.NewAdapter(tmuxprovider.HostID(testHost)).WithRunner(runner)
	p := tmuxprovider.New(adapter, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("tmux provider Start: %v", err)
	}
	return p
}

// buildPsProvider creates a ps.Provider seeded with the given processes
// and starts it.
func buildPsProvider(t *testing.T, procs []psRow) *psprovider.Provider {
	t.Helper()
	runner := &fakePsRunnerProcess{hostID: testHost, rows: procs}
	adapter := psprovider.NewAdapter(testHost).WithRunner(runner)
	p := psprovider.New(adapter, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("ps provider Start: %v", err)
	}
	return p
}

// paneGQLID constructs the TmuxPane GraphQL node ID the resolver expects.
func paneGQLID(host, paneID string) string {
	return "TmuxPane:" + host + ":" + paneID
}

// --------------------------------------------------------------------------
// Tests for tmuxPaneResolver.Process (AC 3 of issue #468)
// --------------------------------------------------------------------------

// TestTmuxPaneProcess_MatchingPid verifies that a pane with CurrentPid 88631
// backed by a ps provider containing that pid returns a non-nil *graphql1.Process
// whose Pid field equals 88631.
func TestTmuxPaneProcess_MatchingPid(t *testing.T) {
	const pid = 88631
	const paneID = "%42"

	tmuxProv := buildTmuxProvider(t, []string{
		paneRow("test-session", paneID, pid),
	})
	psProv := buildPsProvider(t, []psRow{{pid: pid, cmd: "node"}})

	r := &tmuxPaneResolver{&Resolver{Tmux: tmuxProv, PS: psProv}}
	obj := &graphql1.TmuxPane{ID: paneGQLID(testHost, paneID)}

	got, err := r.Process(context.Background(), obj)
	if err != nil {
		t.Fatalf("Process() unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("Process() returned nil, want non-nil *graphql1.Process")
	}
	if got.Pid != int64(pid) {
		t.Errorf("Process().Pid = %d, want %d", got.Pid, pid)
	}
}

// TestTmuxPaneProcess_NoMatchingPid verifies that a pane with CurrentPid 99999
// backed by a ps provider that does NOT have that pid returns (nil, nil).
func TestTmuxPaneProcess_NoMatchingPid(t *testing.T) {
	const pid = 99999
	const paneID = "%43"

	tmuxProv := buildTmuxProvider(t, []string{
		paneRow("test-session", paneID, pid),
	})
	// ps provider has NO processes — pid 99999 won't be found.
	psProv := buildPsProvider(t, nil)

	r := &tmuxPaneResolver{&Resolver{Tmux: tmuxProv, PS: psProv}}
	obj := &graphql1.TmuxPane{ID: paneGQLID(testHost, paneID)}

	got, err := r.Process(context.Background(), obj)
	if err != nil {
		t.Fatalf("Process() unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("Process() = %+v, want nil (no matching pid)", got)
	}
}

// TestTmuxPaneProcess_ZeroPid verifies that a pane with CurrentPid == 0
// returns (nil, nil) without consulting the ps provider.
func TestTmuxPaneProcess_ZeroPid(t *testing.T) {
	const paneID = "%44"

	// Pane seeded with pid=0.
	tmuxProv := buildTmuxProvider(t, []string{
		paneRow("test-session", paneID, 0),
	})
	// PS is nil — if the resolver touches it we'll get a nil-deref panic,
	// proving the short-circuit is working.
	r := &tmuxPaneResolver{&Resolver{Tmux: tmuxProv, PS: nil}}
	obj := &graphql1.TmuxPane{ID: paneGQLID(testHost, paneID)}

	got, err := r.Process(context.Background(), obj)
	if err != nil {
		t.Fatalf("Process() unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("Process() = %+v, want nil (zero pid)", got)
	}
}
