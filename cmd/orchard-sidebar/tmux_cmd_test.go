package main

import (
	"strings"
	"testing"
)

// TestTmuxCmdRoutesToSocket is the regression guard for #747 defect 1: a
// bare `tmux` exec resolves against whichever server owns orchard-sidebar's
// OWN pane — inside the outer-shell wrapper that's the outer server, never
// the inner one holding the sessions the sidebar actually reads/switches.
// tmuxCmd must target the inner server explicitly whenever
// ORCHARD_TMUX_SOCKET says which one that is, and must change nothing when
// it's unset (the sidebar's normal, unwrapped mode).
func TestTmuxCmdRoutesToSocket(t *testing.T) {
	t.Run("socket set: -L is prepended and TMUX stripped", func(t *testing.T) {
		t.Setenv("ORCHARD_TMUX_SOCKET", "inner")
		t.Setenv("TMUX", "/tmp/outer-socket,123,0")

		cmd := tmuxCmd("list-clients", "-F", "#{client_session}")

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
		t.Setenv("ORCHARD_TMUX_SOCKET", "")

		cmd := tmuxCmd("list-clients", "-F", "#{client_session}")

		want := []string{"tmux", "list-clients", "-F", "#{client_session}"}
		if !equalStrings(cmd.Args, want) {
			t.Errorf("Args = %v, want %v", cmd.Args, want)
		}
		if cmd.Env != nil {
			t.Errorf("Env = %v, want nil (inherit parent unchanged)", cmd.Env)
		}
	})
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
