package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// noDriftSleep replaces the drift-poll sleep with a no-op so waitClientAttached's
// retry loop runs instantly — the drift tests exercise the read outcomes, not
// wall-clock timing.
func noDriftSleep(t *testing.T) {
	t.Helper()
	prev := driftSleep
	driftSleep = func(time.Duration) {}
	t.Cleanup(func() { driftSleep = prev })
}

// swapTmuxReaders overrides the two injectable tmux read seams the resolver
// uses (runTmuxOutput on the inner server, runOuterOut on the outer) and
// restores them after the test. inner/outer are the canned stdout; a "" error
// signals success, a non-nil one a failed exec.
func swapTmuxReaders(t *testing.T, inner string, innerErr error, outer string, outerErr error) {
	t.Helper()
	prevIn, prevOut := runTmuxOutput, runOuterOut
	runTmuxOutput = func(args ...string) (string, error) { return inner, innerErr }
	runOuterOut = func(args ...string) (string, error) { return outer, outerErr }
	t.Cleanup(func() { runTmuxOutput, runOuterOut = prevIn, prevOut })
}

// @scenario A healthy env switches with the wrapper's own client unchanged
// @scenario Outer shell restarted, click still switches
// @scenario No inner client can be resolved, the click shows a footer error
// @scenario list-clients errors or returns no clients, the click shows a footer error
//
// The resolver decision table (#787 AC1): env ok / env stale + 0.1 tty match /
// no match / list-clients error / list-clients empty / unwrapped.
func TestResolveClientTTY(t *testing.T) {
	errRead := &tmuxReadErr{}
	cases := []struct {
		name     string
		env      tmuxEnv
		want     clientTTY
		innerOut string
		innerErr error
		outerOut string
		wantTTY  clientTTY
		wantOK   bool
	}{
		{
			name:     "env ok: live client used unchanged",
			env:      tmuxEnv{inner: "in", client: "/dev/ttys001"},
			want:     "/dev/ttys001",
			innerOut: "/dev/ttys001\n/dev/ttys009\n",
			wantTTY:  "/dev/ttys001",
			wantOK:   true,
		},
		{
			name:     "env stale, 0.1 tty is live: fall back to it",
			env:      tmuxEnv{inner: "in", client: "/dev/ttysDEAD"},
			want:     "/dev/ttysDEAD",
			innerOut: "/dev/ttys001\n/dev/ttys009\n",
			outerOut: "0 %0 /dev/ttys000\n1 %1 /dev/ttys009\n",
			wantTTY:  "/dev/ttys009",
			wantOK:   true,
		},
		{
			name:     "env stale, 0.1 tty not a live client: refuse (never any/only client)",
			env:      tmuxEnv{inner: "in", client: "/dev/ttysDEAD"},
			want:     "/dev/ttysDEAD",
			innerOut: "/dev/ttys001\n",
			outerOut: "0 %0 /dev/ttys000\n1 %1 /dev/ttys777\n",
			wantOK:   false,
		},
		{
			name:     "list-clients errors: refuse",
			env:      tmuxEnv{inner: "in", client: "/dev/ttys001"},
			want:     "/dev/ttys001",
			innerErr: errRead,
			wantOK:   false,
		},
		{
			name:     "list-clients empty: refuse",
			env:      tmuxEnv{inner: "in", client: "/dev/ttys001"},
			want:     "/dev/ttys001",
			innerOut: "\n",
			wantOK:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setTmuxEnv(t, c.env)
			swapTmuxReaders(t, c.innerOut, c.innerErr, c.outerOut, nil)
			got, ok := resolveClientTTY(c.want)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (tty %q)", ok, c.wantOK, got)
			}
			if ok && got != c.wantTTY {
				t.Errorf("tty = %q, want %q", got, c.wantTTY)
			}
		})
	}
}

// Unwrapped (legacy) mode has one server and no inner client to validate, so
// the resolver returns the caller's client verbatim and does no tmux read.
func TestResolveClientTTYUnwrappedSkipsIO(t *testing.T) {
	setTmuxEnv(t, tmuxEnv{})
	prevIn, prevOut := runTmuxOutput, runOuterOut
	runTmuxOutput = func(args ...string) (string, error) {
		t.Fatal("read the inner server in unwrapped mode")
		return "", nil
	}
	runOuterOut = func(args ...string) (string, error) {
		t.Fatal("read the outer server in unwrapped mode")
		return "", nil
	}
	t.Cleanup(func() { runTmuxOutput, runOuterOut = prevIn, prevOut })

	got, ok := resolveClientTTY("")
	if !ok || got != "" {
		t.Errorf("resolveClientTTY(\"\") = %q,%v; want \"\",true", got, ok)
	}
}

// @scenario No inner client can be resolved, the click shows a footer error
//
// The footer error must reach the model, not just the log — the whole point of
// #787 is replacing the silent failure with a visible signal.
func TestSwitchClientBoundSurfacesFooterError(t *testing.T) {
	stateHome(t)
	resetLog(t)
	setTmuxEnv(t, tmuxEnv{inner: "in", client: "/dev/ttys001"})
	swapTmuxReaders(t, "", &tmuxReadErr{}, "", nil) // list-clients errors -> no client

	m := &model{}
	m.switchClientBound("work", true)

	if m.status != clientNotFoundStatus {
		t.Errorf("footer status = %q, want %q", m.status, clientNotFoundStatus)
	}
}

// @scenario The hand-back pane guard rejects a bad outer pane and falls back to pane 0.1
//
// The guard (#787 AC2): only a %N pane id that is not the sidebar's own pane is
// accepted; a tty, a window address, or pane 0.0 itself is refused.
func TestHandBackFocusArgsGuard(t *testing.T) {
	setTmuxEnv(t, tmuxEnv{inner: "in", self: "%0"})
	cases := []struct {
		name  string
		outer outerPane
		ok    bool
	}{
		{"good pane id", "%1", true},
		{"our own pane", "%0", false},
		{"a tty path", "/dev/ttys001", false},
		{"a window address", "0.1", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := handBackFocusArgs(c.outer)
			if ok != c.ok {
				t.Errorf("handBackFocusArgs(%q) ok = %v, want %v", c.outer, ok, c.ok)
			}
		})
	}
}

// @scenario A stale launcher env shows a one-time outdated hint at startup
//
// The drift check (#787 AC3): a non-%N outer pane or an unattached client tty
// is stale; a healthy env is not.
func TestEnvDriftStatus(t *testing.T) {
	noDriftSleep(t)
	live := "/dev/ttys001\n"
	cases := []struct {
		name     string
		env      tmuxEnv
		inner    string
		innerErr error
		wantOK   bool
	}{
		{"healthy", tmuxEnv{inner: "in", client: "/dev/ttys001", outer: "%1", self: "%0"}, live, nil, false},
		{"non-%N outer pane", tmuxEnv{inner: "in", client: "/dev/ttys001", outer: "0.1", self: "%0"}, live, nil, true},
		{"client tty not attached", tmuxEnv{inner: "in", client: "/dev/ttysDEAD", outer: "%1", self: "%0"}, live, nil, true},
		{"unwrapped: never drifts", tmuxEnv{self: "%0"}, live, nil, false},
		// @scenario list-clients errors on every poll attempt: the tty can never
		// be confirmed attached, so the check reads exactly like an unattached
		// client — the stale-launcher hint (#787).
		{"list-clients errors every attempt: hint", tmuxEnv{inner: "in", client: "/dev/ttys001", outer: "%1", self: "%0"}, "", &tmuxReadErr{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stateHome(t)
			resetLog(t)
			setTmuxEnv(t, c.env)
			swapTmuxReaders(t, c.inner, c.innerErr, "", nil)
			s, ok := envDriftStatus()
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (status %q)", ok, c.wantOK, s)
			}
			if ok && s != launcherOutdatedStatus {
				t.Errorf("status = %q, want %q", s, launcherOutdatedStatus)
			}
		})
	}
}

// @scenario The hand-back pane guard rejects a bad outer pane and falls back to pane 0.1
//
// End-to-end (#787 AC2): the wrapper handed a bad ORCHARD_OUTER_PANE (here the
// sidebar's own pane), so handBackFocus resolves outer pane 0.1 via
// outerInnerPane and issues select-pane against the resolved %N id — and logs
// the fallback once per process, not once per click.
func TestHandBackFocusFallsBackToInnerPane(t *testing.T) {
	dir := stateHome(t)
	resetLog(t)
	setTmuxEnv(t, tmuxEnv{inner: "in", self: "%0", outer: "%0"}) // outer == self: unusable
	outerPaneGuard.reset()

	var outerCalls [][]string
	prevOuter, prevOuterOut := runOuter, runOuterOut
	runOuter = func(args ...string) { outerCalls = append(outerCalls, args) }
	// Pane index 1 (%7) is this wrapper's inner attach; %0 is the sidebar itself.
	runOuterOut = func(args ...string) (string, error) {
		return "0 %0 /dev/ttys000\n1 %7 /dev/ttys009\n", nil
	}
	t.Cleanup(func() { runOuter, runOuterOut = prevOuter, prevOuterOut })

	handBackFocus(env.outer)
	handBackFocus(env.outer)

	want := selectPaneArgs("%7")
	if len(outerCalls) != 2 {
		t.Fatalf("runOuter called %d times, want 2 (one hand-back each)", len(outerCalls))
	}
	for i, got := range outerCalls {
		if !equalStrings(got, want) {
			t.Errorf("call %d = %v, want %v (resolved 0.1 pane id)", i, got, want)
		}
	}

	b, err := os.ReadFile(filepath.Join(dir, "sidebar.log"))
	if err != nil {
		t.Fatalf("no log file written: %v", err)
	}
	if n := strings.Count(string(b), "falling back to outer pane 0.1"); n != 1 {
		t.Errorf("fallback logged %d times, want 1 (once per process): %q", n, b)
	}
}

// @scenario Outer shell restarted, click still switches
//
// After the resolver falls back from a stale ORCHARD_TMUX_CLIENT to outer pane
// 0.1's live tty, it memoizes that tty into env.client so the j/k browse path —
// which reads env.client through activeClient without resolving — tracks the
// fallback instead of re-failing `switch-client -c <stale>` on every keypress.
func TestResolveClientTTYMemoizesFallback(t *testing.T) {
	setTmuxEnv(t, tmuxEnv{inner: "in", client: "/dev/ttysDEAD"})
	swapTmuxReaders(t, "/dev/ttys009\n", nil, "0 %0 /dev/ttys000\n1 %1 /dev/ttys009\n", nil)

	got, ok := resolveClientTTY("/dev/ttysDEAD")
	if !ok || got != "/dev/ttys009" {
		t.Fatalf("resolveClientTTY = %q,%v; want /dev/ttys009,true", got, ok)
	}
	if env.client != "/dev/ttys009" {
		t.Errorf("env.client = %q after fallback, want memoized /dev/ttys009", env.client)
	}
}

// @scenario A stale launcher env shows a one-time outdated hint at startup
//
// The drift check must not judge the client-attached leg at t0: pane 0.1's
// inner attach connects a beat after Init, so a single read sees zero clients
// and every healthy launch would false-positive. waitClientAttached retries;
// the hint fires only when the tty never appears within the poll window (#787).
func TestEnvDriftSettlesBeforeWarning(t *testing.T) {
	noDriftSleep(t)
	healthyEnv := tmuxEnv{inner: "in", client: "/dev/ttys001", outer: "%1", self: "%0"}

	t.Run("tty appears after a few reads: no hint", func(t *testing.T) {
		stateHome(t)
		resetLog(t)
		setTmuxEnv(t, healthyEnv)
		calls := 0
		prev := runTmuxOutput
		runTmuxOutput = func(...string) (string, error) {
			calls++
			if calls < 3 {
				return "", nil // inner attach not connected yet
			}
			return "/dev/ttys001\n", nil
		}
		t.Cleanup(func() { runTmuxOutput = prev })

		if s, ok := envDriftStatus(); ok {
			t.Errorf("drift fired on a settling healthy env: %q", s)
		}
	})

	t.Run("tty never appears: hint", func(t *testing.T) {
		stateHome(t)
		resetLog(t)
		setTmuxEnv(t, healthyEnv)
		prev := runTmuxOutput
		runTmuxOutput = func(...string) (string, error) { return "", nil }
		t.Cleanup(func() { runTmuxOutput = prev })

		s, ok := envDriftStatus()
		if !ok || s != launcherOutdatedStatus {
			t.Errorf("envDriftStatus = %q,%v; want hint", s, ok)
		}
	})

	t.Run("driftCheck wraps the verdict in a driftMsg", func(t *testing.T) {
		stateHome(t)
		resetLog(t)
		setTmuxEnv(t, healthyEnv)
		prev := runTmuxOutput
		runTmuxOutput = func(...string) (string, error) { return "", nil }
		t.Cleanup(func() { runTmuxOutput = prev })

		msg, ok := driftCheck().(driftMsg)
		if !ok {
			t.Fatalf("driftCheck() returned %T, want driftMsg", driftCheck())
		}
		if !msg.show || msg.status != launcherOutdatedStatus {
			t.Errorf("driftMsg = %+v, want show hint", msg)
		}
	})
}

// tmuxReadErr is a stand-in error for a failed tmux read.
type tmuxReadErr struct{}

func (*tmuxReadErr) Error() string { return "tmux read failed" }
