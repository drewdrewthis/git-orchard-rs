// E2E coverage for TmuxPane.process resolver — issue #463.
//
// Boots a daemon via httptest with stub tmux and ps runners.
// The tmux runner surfaces one pane whose currentPid is 4242.
// The ps runner answers both the `ps` FetchAll call (so the ps
// provider knows the process exists) and the `lsof` FetchCwds
// call (so the resolver can return the cwd).
//
// This test FAILS on the current stub at schema.resolvers.go:695-697
// (`return nil, nil`) and PASSES once the resolver is wired.
// It is the AC6 regression guard for #463.
package resolvers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/server"
	psprovider "github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	tmuxprovider "github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

const (
	paneProcessTestPid = 4242
	paneProcessTestCwd = "/tmp/orchard-pane-process-test"
)

// stubTmuxRunnerWithPane returns a CommandRunner that answers tmux
// commands. The adapter consolidated to a single `tmux list-panes -a -F
// <listAllFormat>` call (#511 follow-up); the 18-field row carries
// session + window + pane metadata.
// info / list-clients return innocuous results.
func stubTmuxRunnerWithPane() *stubRunner {
	const fs = tmuxprovider.FieldSep // the adapter's own constant, never a copy
	onlyState := func(name string, args ...string) ([]byte, error) {
		switch firstNonFlagArg(args) {
		case "info":
			return []byte("ok\n"), nil

		case "list-panes":
			// 18-field listAllFormat: session_name, session_created,
			// session_attached, session_activity, session_windows,
			// session_window_index, window_index, window_name,
			// window_active, window_panes, window_active_pane, pane_id,
			// pane_title, pane_current_command, pane_pid, pane_width,
			// pane_height, pane_dead.
			line := strings.Join([]string{
				"alpha", "1700000000", "0", "1700000010", "1", "0",
				"0", "main", "1", "1", "%1",
				"%1", "bash", "claude",
				fmt.Sprintf("%d", paneProcessTestPid),
				"200", "50", "0",
			}, fs) + "\n"
			return []byte(line), nil

		case "list-clients":
			return []byte(""), nil
		}
		return []byte(""), nil
	}
	return newStubRunner(onlyState)
}

// stubPsRunnerWithProcess returns a CommandRunner that handles:
//
//   - `ps -ax -o pid,ppid,user,tty,%cpu,rss,lstart,command`
//     → returns one row for pid 4242, command "claude".
//
//   - `lsof -a -d cwd -p 4242 -F n`
//     → returns the -F field lines for pid 4242 and the canned cwd
//     value used throughout this test.
//
// All other invocations return empty output (no error) so the provider
// degrades gracefully rather than failing the test.
func stubPsRunnerWithProcess() *stubRunner {
	only := func(name string, args ...string) ([]byte, error) {
		switch name {
		case "ps":
			header := "  PID  PPID USER             TT  %CPU    RSS                 STARTED COMMAND\n"
			// lstart layout: "Mon Jan _2 15:04:05 2006" — we use a well-formed fixed date.
			row := fmt.Sprintf(" %d     1 testuser         ??   0.0    100 Mon Jan  1 00:00:00 2024 claude\n",
				paneProcessTestPid)
			return []byte(header + row), nil

		case "lsof":
			// Verify this is the cwd lsof call for our pid.
			pidStr := fmt.Sprintf("%d", paneProcessTestPid)
			wantedPid := false
			for _, a := range args {
				if a == pidStr {
					wantedPid = true
					break
				}
			}
			if !wantedPid {
				return []byte(""), nil
			}
			// -F n output: "p<pid>\n" then "n<cwd>\n"
			out := fmt.Sprintf("p%d\nn%s\n", paneProcessTestPid, paneProcessTestCwd)
			return []byte(out), nil
		}
		return []byte(""), nil
	}
	return newStubRunner(only)
}

// startPaneProcessDaemon boots a daemon with one tmux pane (pid 4242)
// and a ps adapter that knows about that process + its cwd. Returns the
// httptest server URL.
func startPaneProcessDaemon(t *testing.T) string {
	t.Helper()

	tmuxStub := stubTmuxRunnerWithPane()
	tmuxAdapter := tmuxprovider.NewAdapter(e2eHostID).
		WithRunner(tmuxStub).
		WithSocket("/tmp/orchard-pane-process-test.sock")
	tmuxProv := tmuxprovider.New(tmuxAdapter, nil)

	psStub := stubPsRunnerWithProcess()
	psAdapter := psprovider.NewAdapter(e2eHostID).WithRunner(psStub)
	psProv := psprovider.New(psAdapter, nil)

	srv := server.New("",
		nil,
		server.WithTmux(tmuxProv),
		server.WithPS(psProv),
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := srv.StartHostProvider(ctx); err != nil {
		t.Fatalf("start host provider: %v", err)
	}
	// Start populates the initial snapshot via an implicit Refresh.
	if err := tmuxProv.Start(ctx); err != nil {
		t.Fatalf("start tmux: %v", err)
	}
	if err := psProv.Start(ctx); err != nil {
		t.Fatalf("start ps: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/graphql", srv.GraphQLHandler())
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

// TestPaneProcessTraversal_PopulatesProcessAndCwd verifies that a pane
// with a live currentPid resolves through pane.process.cwd correctly.
// Regression test for #463 — fails on the `return nil, nil` stub at
// schema.resolvers.go:695-697; passes once the resolver is wired.
func TestPaneProcessTraversal_PopulatesProcessAndCwd(t *testing.T) {
	baseURL := startPaneProcessDaemon(t)

	const q = `{
		tmuxSessions {
			windows {
				panes {
					process {
						pid
						command
						cwd
					}
				}
			}
		}
	}`

	body, _ := json.Marshal(map[string]string{"query": q})
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, baseURL+"/graphql", bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("graphql request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("http status = %d, want 200", resp.StatusCode)
	}

	var out struct {
		Data struct {
			TmuxSessions []struct {
				Windows []struct {
					Panes []struct {
						Process *struct {
							Pid     int64   `json:"pid"`
							Command string  `json:"command"`
							Cwd     *string `json:"cwd"`
						} `json:"process"`
					} `json:"panes"`
				} `json:"windows"`
			} `json:"tmuxSessions"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(out.Errors) > 0 {
		errs, _ := json.MarshalIndent(out.Errors, "", "  ")
		t.Fatalf("graphql errors: %s", errs)
	}

	sessions := out.Data.TmuxSessions
	if len(sessions) == 0 {
		t.Fatal("expected at least one tmux session, got none")
	}
	windows := sessions[0].Windows
	if len(windows) == 0 {
		t.Fatal("expected at least one window in session[0], got none")
	}
	panes := windows[0].Panes
	if len(panes) == 0 {
		t.Fatal("expected at least one pane in window[0], got none")
	}

	proc := panes[0].Process
	// This assertion is the one that FAILS on the current nil stub.
	if proc == nil {
		t.Fatalf("pane.process is nil — resolver is not wired (issue #463 stub)")
	}

	if proc.Pid != paneProcessTestPid {
		t.Errorf("pane.process.pid = %d, want %d", proc.Pid, paneProcessTestPid)
	}
	if proc.Command != "claude" {
		t.Errorf("pane.process.command = %q, want %q", proc.Command, "claude")
	}

	// cwd is macOS-only (lsof path). AC7 in issue #463 requires Linux to
	// expose a populated process node but null cwd until /proc/<pid>/cwd
	// support lands; assert both directions so a regression on either
	// platform shows up here.
	if runtime.GOOS == "darwin" {
		if proc.Cwd == nil {
			t.Errorf("pane.process.cwd is nil on darwin, want %q", paneProcessTestCwd)
		} else if !strings.EqualFold(*proc.Cwd, paneProcessTestCwd) {
			t.Errorf("pane.process.cwd = %q, want %q", *proc.Cwd, paneProcessTestCwd)
		}
	} else {
		if proc.Cwd != nil {
			t.Errorf("pane.process.cwd = %q on %s, want nil (Linux fallback unimplemented)", *proc.Cwd, runtime.GOOS)
		}
	}
}
