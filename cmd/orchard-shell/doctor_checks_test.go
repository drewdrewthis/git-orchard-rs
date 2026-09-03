package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// @scenario orchard shell doctor — tmux version check
//
// AC8: `tmux -V` >= 3.4 passes; older fails; tmux absent fails rather than
// panicking.
func TestEvaluateTmuxVersion(t *testing.T) {
	tests := []struct {
		name   string
		output string
		err    error
		want   checkStatus
	}{
		{"current point release", "tmux 3.6a", nil, statusPass},
		{"exactly the minimum", "tmux 3.4", nil, statusPass},
		{"newer major", "tmux 4.0", nil, statusPass},
		{"nightly build", "tmux next-3.4", nil, statusPass},
		{"older point release", "tmux 3.3", nil, statusFail},
		{"much older", "tmux 2.9", nil, statusFail},
		{"tmux not found", "", errors.New("exec: \"tmux\": executable file not found in $PATH"), statusFail},
		{"unparseable output", "not a version string", nil, statusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateTmuxVersion(tt.output, tt.err)
			if got.Status != tt.want {
				t.Errorf("evaluateTmuxVersion(%q, %v) = %v; want %v", tt.output, tt.err, got.Status, tt.want)
			}
			if got.ID != "tmux" {
				t.Errorf("ID = %q; want tmux", got.ID)
			}
			if got.Status == statusFail && got.Remedy == "" {
				t.Error("fail status carries no remedy")
			}
		})
	}
}

func TestCheckTmuxVersion_ReadsFromInjectedTmux(t *testing.T) {
	f := newFakeTmux().reply("-V", "tmux 3.6a")
	env := doctorEnv{tmux: f.exec}
	if got := checkTmuxVersion(env); got.Status != statusPass {
		t.Errorf("Status = %v; want pass", got.Status)
	}
}

// AC8: run from inside an existing tmux session, $TMUX reports warn, not
// fail.
func TestCheckTmuxNesting(t *testing.T) {
	t.Run("TMUX unset passes", func(t *testing.T) {
		t.Setenv("TMUX", "")
		if got := checkTmuxNesting(); got.Status != statusPass {
			t.Errorf("Status = %v; want pass", got.Status)
		}
	})
	t.Run("TMUX set warns, does not fail", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
		got := checkTmuxNesting()
		if got.Status != statusWarn {
			t.Errorf("Status = %v; want warn", got.Status)
		}
	})
}

func TestCheckInnerSocket(t *testing.T) {
	t.Run("server with sessions passes", func(t *testing.T) {
		f := newFakeTmux().reply(innerCallStr("list-sessions"), "0 work\n0 scratch")
		env := doctorEnv{tmux: f.exec}
		got := checkInnerSocket(env)
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass", got.Status)
		}
	})
	t.Run("no server fails with a remedy", func(t *testing.T) {
		f := newFakeTmux().fail(innerCallStr("list-sessions"), "no server running on default")
		env := doctorEnv{tmux: f.exec}
		got := checkInnerSocket(env)
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail", got.Status)
		}
		if !strings.Contains(got.Remedy, "orchard new") {
			t.Errorf("Remedy = %q; want it to mention orchard new", got.Remedy)
		}
	})
}

// innerCallStr renders the argv checkInnerSocket uses, for registering fake
// replies (defaultInnerSocket, unlike fake_tmux_test.go's innerCall helper
// which is pinned to "inner-test").
func innerCallStr(args ...string) string {
	return strings.Join(innerArgs(defaultInnerSocket, args...), " ")
}

// outerCallStr renders the argv checkOuterSocket uses, for registering fake
// replies against defaultOuterSocket and a fixed test conf path.
func outerCallStr(conf string, args ...string) string {
	return strings.Join(outerArgs(defaultOuterSocket, conf, args...), " ")
}

func TestCheckOuterSocket(t *testing.T) {
	const conf = "/fake/outer.conf"

	t.Run("conf resolution failure fails", func(t *testing.T) {
		env := doctorEnv{confErr: errors.New("boom")}
		got := checkOuterSocket(env)
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail", got.Status)
		}
	})

	t.Run("no session yet passes", func(t *testing.T) {
		f := newFakeTmux().fail(outerCallStr(conf, "has-session", "-t", outerSessionName), "can't find session")
		env := doctorEnv{tmux: f.exec, conf: conf}
		got := checkOuterSocket(env)
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass", got.Status)
		}
	})

	t.Run("collapsed one-pane session warns as rebuild-needed", func(t *testing.T) {
		f := newFakeTmux().
			reply(outerCallStr(conf, "has-session", "-t", outerSessionName), "").
			reply(outerCallStr(conf, "list-panes", "-t", outerSessionName+":0", "-F", "#{pane_index} #{pane_dead} #{pane_tty}"),
				"0 0 /dev/ttys013")
		env := doctorEnv{tmux: f.exec, conf: conf}
		got := checkOuterSocket(env)
		if got.Status != statusWarn {
			t.Errorf("Status = %v; want warn", got.Status)
		}
		if !strings.Contains(got.Remedy, "orchard shell") {
			t.Errorf("Remedy = %q; want it to mention orchard shell", got.Remedy)
		}
	})

	t.Run("dead inner client warns as respawn-needed", func(t *testing.T) {
		f := newFakeTmux().
			reply(outerCallStr(conf, "has-session", "-t", outerSessionName), "").
			reply(outerCallStr(conf, "list-panes", "-t", outerSessionName+":0", "-F", "#{pane_index} #{pane_dead} #{pane_tty}"),
				"0 0 /dev/ttys004\n1 0 /dev/ttys005").
			fail(innerCallStr("list-clients", "-F", "#{client_tty}"), "no server running")
		env := doctorEnv{tmux: f.exec, conf: conf}
		got := checkOuterSocket(env)
		if got.Status != statusWarn {
			t.Errorf("Status = %v; want warn", got.Status)
		}
		if !strings.Contains(got.Remedy, "orchard shell") {
			t.Errorf("Remedy = %q; want it to mention orchard shell", got.Remedy)
		}
	})

	t.Run("healthy wrapper passes", func(t *testing.T) {
		f := newFakeTmux().
			reply(outerCallStr(conf, "has-session", "-t", outerSessionName), "").
			reply(outerCallStr(conf, "list-panes", "-t", outerSessionName+":0", "-F", "#{pane_index} #{pane_dead} #{pane_tty}"),
				"0 0 /dev/ttys004\n1 0 /dev/ttys005").
			reply(innerCallStr("list-clients", "-F", "#{client_tty}"), "/dev/ttys005")
		env := doctorEnv{tmux: f.exec, conf: conf}
		got := checkOuterSocket(env)
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass", got.Status)
		}
	})
}

func TestEvaluateSystemd(t *testing.T) {
	tests := []struct {
		name   string
		output string
		err    error
		want   checkStatus
	}{
		{"active", "active", nil, statusPass},
		{"inactive", "inactive", errors.New("tmux systemctl --user is-active orchard: exit status 3"), statusFail},
		{"systemctl not found", "", &exec.Error{Name: "systemctl", Err: exec.ErrNotFound}, statusWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateSystemd(tt.output, tt.err)
			if got.Status != tt.want {
				t.Errorf("evaluateSystemd(%q, %v) = %v; want %v", tt.output, tt.err, got.Status, tt.want)
			}
			if tt.want == statusFail && !strings.Contains(got.Remedy, "systemctl --user start orchard") {
				t.Errorf("Remedy = %q; want it to contain the exact remedy command", got.Remedy)
			}
		})
	}
}

func TestCheckSystemd_NonLinuxPassesWithoutRunningAnything(t *testing.T) {
	calls := 0
	env := doctorEnv{
		goos: "darwin",
		run: func(ctx context.Context, name string, args ...string) (string, error) {
			calls++
			return "", nil
		},
	}
	got := checkSystemd(context.Background(), env)
	if got.Status != statusPass {
		t.Errorf("Status = %v; want pass", got.Status)
	}
	if calls != 0 {
		t.Errorf("checkSystemd ran a command on non-linux goos; want zero calls")
	}
}

func TestCheckSystemd_LinuxRunsSystemctl(t *testing.T) {
	env := doctorEnv{
		goos: "linux",
		run: func(ctx context.Context, name string, args ...string) (string, error) {
			if name != "systemctl" {
				t.Errorf("ran %q; want systemctl", name)
			}
			return "active", nil
		},
	}
	got := checkSystemd(context.Background(), env)
	if got.Status != statusPass {
		t.Errorf("Status = %v; want pass", got.Status)
	}
}

func TestCheckPath(t *testing.T) {
	tests := []struct {
		name    string
		self    string
		pathEnv string
		want    checkStatus
	}{
		{"install dir on PATH", "/opt/orchard/orchard-shell", "/usr/bin:/opt/orchard:/bin", statusPass},
		{"install dir missing from PATH", "/opt/orchard/orchard-shell", "/usr/bin:/bin", statusFail},
		{"self unresolved warns", "", "/usr/bin", statusWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkPath(tt.self, tt.pathEnv)
			if got.Status != tt.want {
				t.Errorf("checkPath(%q, %q) = %v; want %v", tt.self, tt.pathEnv, got.Status, tt.want)
			}
		})
	}
}
