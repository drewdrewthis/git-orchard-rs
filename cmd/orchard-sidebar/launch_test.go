package main

import (
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

// The command is one argument. Splitting it on spaces would break every launch
// with a flag in it, which is most of them.
func TestNewSessionArgsKeepTheCommandWhole(t *testing.T) {
	got := newSessionArgs("/w/x", "claude --resume abc", "x")
	want := []string{"new-session", "-d", "-s", "x", "-c", "/w/x", "claude --resume abc"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("newSessionArgs = %v, want %v", got, want)
	}
	// no command: tmux opens the default shell, and an empty trailing argument
	// would be read as a command that isn't there
	if got := newSessionArgs("/w/x", "   ", "x"); len(got) != 6 {
		t.Errorf("empty command produced %v", got)
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
