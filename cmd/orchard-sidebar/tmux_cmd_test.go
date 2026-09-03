package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// TestInnerCmdRoutesToSocket is the regression guard for #747 defect 1: a bare
// `tmux` exec resolves against whichever server owns orchard-sidebar's OWN
// pane — inside the outer-shell wrapper that's the outer server, never the
// inner one holding the sessions the sidebar actually reads/switches. innerCmd
// must target the inner server explicitly whenever the environment says which
// one that is, and must change nothing when it doesn't (the sidebar's normal,
// unwrapped mode).
func TestInnerCmdRoutesToSocket(t *testing.T) {
	t.Run("socket set: -L is prepended and TMUX stripped", func(t *testing.T) {
		setTmuxEnv(t, tmuxEnv{inner: "inner"})
		t.Setenv("TMUX", "/tmp/outer-socket,123,0")

		cmd := env.innerCmd("list-clients", "-F", "#{client_session}")

		want := []string{"tmux", "-L", "inner", "list-clients", "-F", "#{client_session}"}
		if !equalStrings(cmd.Args, want) {
			t.Errorf("Args = %v, want %v", cmd.Args, want)
		}
		if cmd.Env == nil {
			t.Fatal("Env is nil, want TMUX stripped from an explicit env list")
		}
		for _, e := range cmd.Env {
			if strings.HasPrefix(e, "TMUX=") {
				t.Errorf("Env still carries %q, want TMUX stripped", e)
			}
		}
	})

	t.Run("socket unset: args and env are untouched", func(t *testing.T) {
		setTmuxEnv(t, tmuxEnv{})

		cmd := env.innerCmd("list-clients", "-F", "#{client_session}")

		want := []string{"tmux", "list-clients", "-F", "#{client_session}"}
		if !equalStrings(cmd.Args, want) {
			t.Errorf("Args = %v, want %v", cmd.Args, want)
		}
		if cmd.Env != nil {
			t.Errorf("Env = %v, want nil (inherit parent unchanged)", cmd.Env)
		}
	})
}

// TestOuterCmdBypassesInnerRouting is the regression guard for #747 defect 3:
// handBackFocus must always reach the OUTER server, never the inner one
// innerCmd targets when a socket is set -- an outer pane id sent inwards names
// a different, unrelated pane there. outerCmd must be unaffected by the inner
// socket and by TMUX even when both are set, which is the real
// wrapped-and-running state every outerPane caller runs in.
func TestOuterCmdBypassesInnerRouting(t *testing.T) {
	setTmuxEnv(t, tmuxEnv{inner: "inner", outer: "%1"})
	t.Setenv("TMUX", "/tmp/outer-socket,123,0")

	args, ok := handBackFocusArgs()
	if !ok {
		t.Fatal("handBackFocusArgs declined with an outer pane set")
	}
	cmd := env.outerCmd(args...)

	want := []string{"tmux", "select-pane", "-t", "%1"}
	if !equalStrings(cmd.Args, want) {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if cmd.Env != nil {
		t.Errorf("Env = %v, want nil (inherit parent unchanged, TMUX NOT stripped)", cmd.Env)
	}
}

// Unwrapped (or wrapped but not yet told its outer pane) there is nothing to
// hand focus back to, and the sidebar must not guess a pane id.
func TestHandBackFocusDeclinesWithoutAnOuterPane(t *testing.T) {
	setTmuxEnv(t, tmuxEnv{inner: "inner", client: "/dev/ttys003"})
	if args, ok := handBackFocusArgs(); ok {
		t.Errorf("handBackFocusArgs = %v, want a refusal with no outer pane", args)
	}
}

// The pane-targeting commands take an outerPane and nothing else: an inner
// session name cannot reach them, which is the type-level half of the #747
// defect-1 fix.
func TestOuterPaneArgs(t *testing.T) {
	if got := resizePaneArgs(outerPane("%0"), 52); !equalStrings(got,
		[]string{"resize-pane", "-t", "%0", "-x", "52"}) {
		t.Errorf("resizePaneArgs = %v", got)
	}
	if got := setPaneOptionArgs(outerPane("%0"), collapsedOption, "1"); !equalStrings(got,
		[]string{"set-option", "-w", "-t", "%0", "@sidebar_collapsed", "1"}) {
		t.Errorf("setPaneOptionArgs = %v", got)
	}
	if got := setPaneOptionArgs(outerPane("%0"), widthOption, "52"); !equalStrings(got,
		[]string{"set-option", "-w", "-t", "%0", "main-pane-width", "52"}) {
		t.Errorf("width option args = %v", got)
	}
}

// A half-configured wrapper is otherwise silent: switches are refused and
// collapse does nothing, with nothing on screen saying why.
func TestTmuxEnvProblem(t *testing.T) {
	cases := []struct {
		name string
		env  tmuxEnv
		want string
	}{
		{"unwrapped", tmuxEnv{}, ""},
		{"unwrapped in a pane", tmuxEnv{self: "%0"}, ""},
		{"fully wrapped", tmuxEnv{inner: "in", client: "/dev/ttys1", outer: "%1", self: "%0"}, ""},
		{"wrapped without a client tty", tmuxEnv{inner: "in", self: "%0"},
			"no ORCHARD_TMUX_CLIENT — switching disabled"},
		{"wrapped outside a pane", tmuxEnv{inner: "in", client: "/dev/ttys1", outer: "%1"},
			"no TMUX_PANE — resize and collapse disabled"},
		{"outer pane but no pane of our own", tmuxEnv{outer: "%1"},
			"no TMUX_PANE — resize and collapse disabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.env.problem(); got != c.want {
				t.Errorf("problem() = %q, want %q", got, c.want)
			}
		})
	}
}

// The banner is the only place a broken environment shows up, so it has to
// reach the pane — not just the log.
func TestEnvProblemShowsInTheHeader(t *testing.T) {
	setTmuxEnv(t, tmuxEnv{inner: "in", self: "%0"})
	m := &model{rows: rowsForHeight(2), stateDirOK: true}
	m.Update(tea.WindowSizeMsg{Width: 42, Height: 30})
	if !strings.Contains(ansi.Strip(m.View()), "ORCHARD_TMUX_CLIENT") {
		t.Errorf("the header does not name the broken environment:\n%s", ansi.Strip(m.View()))
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The sidebar holds the alt screen for its whole life, so a diagnostic written
// to stderr lands IN its pane, on top of the UI, and stays there until
// something repaints that exact cell. A few failed switch-clients shredded the
// live sidebar into unreadable strips of half-drawn cards. Diagnostics go to a
// file; the pane is for the UI.
func TestDiagnosticsGoToTheLogFileNotThePane(t *testing.T) {
	dir := stateHome(t)
	resetLog(t)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	real := os.Stderr
	os.Stderr = w
	logf("switch-client -c %s: %s", "/dev/ttys006", "can't find client")
	os.Stderr = real
	w.Close()
	spew, _ := io.ReadAll(r)
	if len(spew) > 0 {
		t.Errorf("a diagnostic reached the pane: %q", spew)
	}

	b, err := os.ReadFile(filepath.Join(dir, "sidebar.log"))
	if err != nil {
		t.Fatalf("no log file was written: %v", err)
	}
	if !strings.Contains(string(b), "can't find client") {
		t.Errorf("log line does not carry the failure: %q", b)
	}
	// and it appends rather than truncating: a log with one line in it is a
	// log that lost the failure before the one you are chasing
	logf("second")
	b, _ = os.ReadFile(filepath.Join(dir, "sidebar.log"))
	if n := strings.Count(string(b), "\n"); n != 2 {
		t.Errorf("log has %d lines after two writes, want 2: %q", n, b)
	}
}

// An uncapped append-only log is a slow disk leak on the one machine least
// able to notice it: a wedged tmux fails an exec on every tick.
func TestLogFileIsCapped(t *testing.T) {
	dir := stateHome(t)
	resetLog(t)

	line := strings.Repeat("x", 4096)
	for i := 0; i < logMax/len(line)+8; i++ {
		logf("%s", line)
	}
	fi, err := os.Stat(filepath.Join(dir, "sidebar.log"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > logMax {
		t.Errorf("log grew to %d bytes, cap is %d", fi.Size(), logMax)
	}
	if fi.Size() == 0 {
		t.Error("log is empty after the wrap: the newest lines are the ones worth keeping")
	}
}

// resetLog drops the held file handle so the next logf reopens under whatever
// XDG_STATE_HOME the test set.
func resetLog(t *testing.T) {
	t.Helper()
	logMu.Lock()
	if logDest != nil {
		_ = logDest.Close()
	}
	logDest, logSize = nil, 0
	logMu.Unlock()
	t.Cleanup(func() {
		logMu.Lock()
		if logDest != nil {
			_ = logDest.Close()
		}
		logDest, logSize = nil, 0
		logMu.Unlock()
	})
}
