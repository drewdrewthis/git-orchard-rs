package main

import "testing"

// TestSwitchClientArgs is the regression guard for #747 defect 2: on a
// SHARED inner tmux server, a plain `switch-client -t <session>` lets tmux
// pick an arbitrary attached client to move — in the wild this hijacked the
// user's unrelated attached terminal. The client tty scopes the switch via
// -c; when wrapped but not yet told which client is its own, switching must
// refuse rather than risk moving a foreign client.
func TestSwitchClientArgs(t *testing.T) {
	cases := []struct {
		name string
		env  tmuxEnv
		want []string
		ok   bool
	}{
		{
			name: "client set: -c <tty> is included",
			env:  tmuxEnv{client: "/dev/ttys003", inner: "inner"},
			want: []string{"switch-client", "-c", "/dev/ttys003", "-t", "work"},
			ok:   true,
		},
		{
			name: "client set, socket unset: still scoped via -c",
			env:  tmuxEnv{client: "/dev/ttys003"},
			want: []string{"switch-client", "-c", "/dev/ttys003", "-t", "work"},
			ok:   true,
		},
		{
			name: "socket set, client unset: refuses rather than switching unscoped",
			env:  tmuxEnv{inner: "inner"},
			ok:   false,
		},
		{
			name: "legacy: neither set, unchanged",
			env:  tmuxEnv{},
			want: []string{"switch-client", "-t", "work"},
			ok:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setTmuxEnv(t, c.env)
			args, ok := switchClientArgs("work")
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (args %v)", ok, c.ok, args)
			}
			if !ok {
				if args != nil {
					t.Errorf("args = %v, want nil on refusal", args)
				}
				return
			}
			if !equalStrings(args, c.want) {
				t.Errorf("args = %v, want %v", args, c.want)
			}
		})
	}
}

// The refusal must reach the CALLER on the launch path, not just the log: a
// launch that created the session and quietly skipped the switch reported
// success while leaving the user where they were (review finding 3).
func TestLaunchSurfacesTheSwitchRefusal(t *testing.T) {
	setTmuxEnv(t, tmuxEnv{inner: "inner"}) // wrapped, no client tty
	if err := switchClientTo("work"); err == nil {
		t.Fatal("switchClientTo returned nil on a refusal")
	}
}

// TestPickClient is the fetchClientSession half of the #747 defect 2 guard:
// with tty scoping in play, a bystander client's more recent activity must
// never outrank this sidebar's own client.
func TestPickClient(t *testing.T) {
	cases := []struct {
		name, out string
		want      string
		tty       clientTTY
	}{
		{
			name: "unscoped: most recent activity wins (legacy)",
			out:  "100 /dev/ttys001 other\n200 /dev/ttys002 mine\n",
			want: "mine",
		},
		{
			name: "scoped: tty match wins even when a bystander is more recently active",
			out:  "200 /dev/ttys001 other\n100 /dev/ttys002 mine\n",
			tty:  "/dev/ttys002",
			want: "mine",
		},
		{
			name: "scoped: bystander is excluded outright, not just outranked",
			out:  "999 /dev/ttys001 other\n",
			tty:  "/dev/ttys002",
			want: "",
		},
		{
			name: "session names may contain spaces",
			out:  "100 /dev/ttys002 my session name\n",
			tty:  "/dev/ttys002",
			want: "my session name",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickClient(c.out, c.tty); got != c.want {
				t.Errorf("pickClient = %q, want %q", got, c.want)
			}
		})
	}
}
