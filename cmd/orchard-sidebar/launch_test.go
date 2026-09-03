package main

import (
	"os/exec"
	"strings"
	"testing"
)

// stubTaken replaces the tmux lookup for the duration of a test: building a
// launch model must never shell out to whatever tmux server the test machine
// happens to be running.
func stubTaken(t *testing.T, names ...string) {
	t.Helper()
	prev := takenSessions
	taken := map[string]bool{}
	for _, n := range names {
		taken[n] = true
	}
	takenSessions = func() map[string]bool { return taken }
	t.Cleanup(func() { takenSessions = prev })
}

// tmux refuses a duplicate session name outright, which reads as "the button
// did nothing". Collisions resolve the way a tmux user expects instead.
func TestUniqueNameDedupesAndSanitises(t *testing.T) {
	taken := map[string]bool{"api": true, "api-2": true}
	cases := []struct{ base, want string }{
		{"web", "web"},
		{"api", "api-3"},
		{"my project", "my-project"},
		// dots and colons are tmux target syntax: a session called "v1.2"
		// makes every later -t ambiguous
		{"v1.2", "v1-2"},
		{"a:b", "a-b"},
		{"  ", "session"},
	}
	for _, c := range cases {
		if got := uniqueName(c.base, taken); got != c.want {
			t.Errorf("uniqueName(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

// The session-creating call carries NO command (#783): the command is the
// pane's process otherwise, and its exit kills pane → window → session. The
// session must open on the user's shell so the pane survives the command.
func TestNewSessionArgsCarryNoCommand(t *testing.T) {
	got := newSessionArgs("/w/x", "x")
	want := []string{"new-session", "-d", "-s", "x", "-c", "/w/x"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("newSessionArgs = %v, want %v", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "claude") || strings.Contains(a, "--") {
			t.Errorf("new-session carried a command argument: %v", got)
		}
	}
}

// The command is delivered as a literal send-keys (-l), then Enter separately
// — mirroring the daemon's own launchSession — so tmux never parses the
// command text as key names. An empty command delivers nothing (ok == false),
// so the session just opens on its shell.
func TestSendCommandArgsDeliverTheWholeCommand(t *testing.T) {
	got, ok := sendCommandArgs("x", "claude --resume abc")
	if !ok {
		t.Fatalf("sendCommandArgs reported nothing to send for a real command")
	}
	want := [][]string{
		{"send-keys", "-t", "x", "-l", "claude --resume abc"},
		{"send-keys", "-t", "x", "Enter"},
	}
	if len(got) != len(want) {
		t.Fatalf("sendCommandArgs = %v, want %v", got, want)
	}
	for i := range want {
		if strings.Join(got[i], "|") != strings.Join(want[i], "|") {
			t.Errorf("sendCommandArgs[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if _, ok := sendCommandArgs("x", "   "); ok {
		t.Errorf("empty command should deliver nothing")
	}
}

// A popup inherits the outer server's environment, not the sidebar pane's. If
// the socket and client don't ride along on the command line, the modal builds
// the session on the wrong tmux server — silently, since both exist.
func TestPopupArgsCarryTheServerEnv(t *testing.T) {
	setTmuxEnv(t, tmuxEnv{self: "%1", inner: "default", client: "/dev/ttys006"})
	dir := t.TempDir()
	got := strings.Join(popupArgs("/opt/orchard-sidebar", dir), " ")
	for _, want := range []string{
		"display-popup -E -w 80% -h 60%",
		"-t %1",
		"-e ORCHARD_TMUX_SOCKET=default",
		"-e ORCHARD_TMUX_CLIENT=/dev/ttys006",
		"-e ORCHARD_LAUNCH_DIR=" + dir,
		"-d " + dir,
		"'/opt/orchard-sidebar' launch",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("popupArgs missing %q:\n%s", want, got)
		}
	}
	// a directory that has since been removed must not reach -d, which would
	// fail the popup instead of opening it on a fallback
	if got := strings.Join(popupArgs("/opt/x", "/w/gone-away"), " "); strings.Contains(got, "-d ") {
		t.Errorf("popupArgs passed -d for a missing dir: %s", got)
	}
}

// TestShellQuoteRoundTrips checks the quoted string survives the shell tmux
// actually runs it through — `sh -c 'printf %s ...'`, the same shape
// popupArgs feeds the popup — for the two path shapes that break naive
// quoting: an embedded single quote and embedded spaces.
func TestShellQuoteRoundTrips(t *testing.T) {
	for _, path := range []string{
		"/Users/o'brien/worktree",
		"/home/dev/my project/worktree",
	} {
		quoted := shellQuote(path)
		out, err := exec.Command("sh", "-c", "printf %s "+quoted).Output()
		if err != nil {
			t.Fatalf("shellQuote(%q) = %s: sh rejected it: %v", path, quoted, err)
		}
		if got := string(out); got != path {
			t.Errorf("shellQuote(%q) = %s, round-tripped to %q, want %q", path, quoted, got, path)
		}
	}
}
