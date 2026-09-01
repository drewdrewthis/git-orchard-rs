package main

import "testing"

// TestSwitchClientArgs is the regression guard for #747 defect 2: on a
// SHARED inner tmux server, a plain `switch-client -t <session>` lets tmux
// pick an arbitrary attached client to move — in the wild this hijacked the
// user's unrelated attached terminal. ORCHARD_TMUX_CLIENT (this sidebar's
// own client tty) scopes the switch via -c; when wrapped (ORCHARD_TMUX_SOCKET
// set) but not yet told which client is its own, switching must refuse
// rather than risk moving a foreign client.
func TestSwitchClientArgs(t *testing.T) {
	t.Run("client set: -c <tty> is included", func(t *testing.T) {
		t.Setenv("ORCHARD_TMUX_CLIENT", "/dev/ttys003")
		t.Setenv("ORCHARD_TMUX_SOCKET", "inner")

		args, ok := switchClientArgs("work")

		if !ok {
			t.Fatal("ok = false, want true (client is scoped)")
		}
		want := []string{"switch-client", "-c", "/dev/ttys003", "-t", "work"}
		if !equalStrings(args, want) {
			t.Errorf("args = %v, want %v", args, want)
		}
	})

	t.Run("client set, socket unset: still scoped via -c", func(t *testing.T) {
		t.Setenv("ORCHARD_TMUX_CLIENT", "/dev/ttys003")
		t.Setenv("ORCHARD_TMUX_SOCKET", "")

		args, ok := switchClientArgs("work")

		if !ok {
			t.Fatal("ok = false, want true")
		}
		want := []string{"switch-client", "-c", "/dev/ttys003", "-t", "work"}
		if !equalStrings(args, want) {
			t.Errorf("args = %v, want %v", args, want)
		}
	})

	t.Run("socket set, client unset: refuses rather than switching unscoped", func(t *testing.T) {
		t.Setenv("ORCHARD_TMUX_CLIENT", "")
		t.Setenv("ORCHARD_TMUX_SOCKET", "inner")

		args, ok := switchClientArgs("work")

		if ok {
			t.Errorf("ok = true, args = %v — want refusal on a foreign socket with no client scope", args)
		}
		if args != nil {
			t.Errorf("args = %v, want nil on refusal", args)
		}
	})

	t.Run("legacy: neither set, unchanged", func(t *testing.T) {
		t.Setenv("ORCHARD_TMUX_CLIENT", "")
		t.Setenv("ORCHARD_TMUX_SOCKET", "")

		args, ok := switchClientArgs("work")

		if !ok {
			t.Fatal("ok = false, want true (legacy path)")
		}
		want := []string{"switch-client", "-t", "work"}
		if !equalStrings(args, want) {
			t.Errorf("args = %v, want %v", args, want)
		}
	})
}

// TestPickClient is the fetchClientSession half of the #747 defect 2 guard:
// with ORCHARD_TMUX_CLIENT scoping in play, a bystander client's more recent
// activity must never outrank this sidebar's own client.
func TestPickClient(t *testing.T) {
	t.Run("unscoped: most recent activity wins (legacy)", func(t *testing.T) {
		out := "100 42 /dev/ttys001 other\n200 42 /dev/ttys002 mine\n"

		name, width := pickClient(out, "")

		if name != "mine" || width != 42 {
			t.Errorf("pickClient(_, \"\") = (%q, %d), want (\"mine\", 42)", name, width)
		}
	})

	t.Run("scoped: tty match wins even when a bystander is more recently active", func(t *testing.T) {
		out := "200 42 /dev/ttys001 other\n100 42 /dev/ttys002 mine\n"

		name, width := pickClient(out, "/dev/ttys002")

		if name != "mine" || width != 42 {
			t.Errorf("pickClient(_, tty) = (%q, %d), want (\"mine\", 42) despite lower activity", name, width)
		}
	})

	t.Run("scoped: bystander is excluded outright, not just outranked", func(t *testing.T) {
		out := "999 42 /dev/ttys001 other\n"

		name, _ := pickClient(out, "/dev/ttys002")

		if name != "" {
			t.Errorf("pickClient picked bystander %q for an unmatched tty scope", name)
		}
	})

	t.Run("session names may contain spaces", func(t *testing.T) {
		out := "100 42 /dev/ttys002 my session name\n"

		name, _ := pickClient(out, "/dev/ttys002")

		if name != "my session name" {
			t.Errorf("name = %q, want %q", name, "my session name")
		}
	})
}
