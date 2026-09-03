package main

import (
	"strings"
	"testing"
)

func TestDetachBlocked(t *testing.T) {
	if detachBlocked(false) == "" {
		t.Error("detach on the sole work pane must be refused")
	}
	if detachBlocked(true) != "" {
		t.Error("detach with a split open must be allowed")
	}
}

// M-w detaches split pane's client, re-pins layout, drops to single-pane state.
func TestDetachSplitClosesPane(t *testing.T) {
	splitEnv(t)
	var detached clientTTY
	prev := detachClient
	detachClient = func(tty clientTTY) { detached = tty }
	t.Cleanup(func() { detachClient = prev })

	m := splitModel()
	m.splitOpen = true
	m.alt = workPaneRef{outer: "%2", client: "/dev/ttys002"}
	m.workOverride = m.alt // pretend focus was on the split pane
	m.detachSplit()

	if detached != "/dev/ttys002" {
		t.Errorf("detached %q, want the split pane's client", detached)
	}
	if m.splitOpen || m.alt != (workPaneRef{}) {
		t.Errorf("split state not cleared: open=%v alt=%+v", m.splitOpen, m.alt)
	}
	if m.workOverride != (workPaneRef{}) {
		t.Error("workOverride not reset — the sidebar would keep driving a closed pane")
	}
}

// M-w on sole work pane is refused: detaching only client drops user's terminal.
func TestDetachSplitRefusedOnSolePane(t *testing.T) {
	splitEnv(t)
	called := false
	prev := detachClient
	detachClient = func(clientTTY) { called = true }
	t.Cleanup(func() { detachClient = prev })

	m := splitModel() // splitOpen defaults false
	m.detachSplit()

	if called {
		t.Error("the sole work pane's client was detached")
	}
	if !strings.Contains(m.statusText(), "sole work pane") {
		t.Errorf("status = %q, want the refusal reason", m.statusText())
	}
}

// Switch and hand-back follow the focused work pane via m.workOverride; default
// to the original env pane.
func TestActiveTargetsFollowWorkOverride(t *testing.T) {
	splitEnv(t)
	m := splitModel()
	if m.activeClient() != "/dev/ttys001" || m.activeOuter() != "%1" {
		t.Fatalf("default active target = (%q,%q), want the env pane", m.activeClient(), m.activeOuter())
	}
	m.workOverride = workPaneRef{outer: "%2", client: "/dev/ttys002"}
	if m.activeClient() != "/dev/ttys002" || m.activeOuter() != "%2" {
		t.Errorf("override active target = (%q,%q), want the split pane", m.activeClient(), m.activeOuter())
	}
	args, ok := switchClientArgs("beta", m.activeClient())
	if !ok || !equalStrings(args, []string{"switch-client", "-c", "/dev/ttys002", "-t", "beta"}) {
		t.Errorf("switchClientArgs = %v (ok=%v), want the split client scope", args, ok)
	}
	hb, ok := handBackFocusArgs(m.activeOuter())
	if !ok || !equalStrings(hb, []string{"select-pane", "-t", "%2"}) {
		t.Errorf("handBackFocusArgs = %v (ok=%v), want the split pane", hb, ok)
	}
}

// The focus-follow wiring: a client read naming the split pane's tty retargets
// the active client/outer at it (via Update → retargetWork), and detaching the
// split returns them to the original env pane.
func TestClientReadRetargetsWorkAndDetachRestores(t *testing.T) {
	splitEnv(t)
	prev := detachClient
	detachClient = func(clientTTY) {}
	t.Cleanup(func() { detachClient = prev })

	m := splitModel()
	m.splitOpen = true
	m.alt = workPaneRef{outer: "%2", client: "/dev/ttys002"}

	// a clientSessMsg carrying the split pane's tty moves the target there
	m.Update(clientSessMsg{name: "beta", tty: "/dev/ttys002", gen: m.clientGen})
	if m.activeClient() != "/dev/ttys002" || m.activeOuter() != "%2" {
		t.Fatalf("active target = (%q,%q), want the split pane after a focus read", m.activeClient(), m.activeOuter())
	}

	// M-w detaches the split and hands the sidebar back to the original pane
	m.detachSplit()
	if m.activeClient() != "/dev/ttys001" || m.activeOuter() != "%1" {
		t.Errorf("active target = (%q,%q), want the original pane after detach", m.activeClient(), m.activeOuter())
	}
}

// pickWork takes most-recently-active of wrapper's OWN work clients, never bystanders.
func TestPickWorkFollowsMostActiveWorkClient(t *testing.T) {
	allow := []clientTTY{"/dev/ttys001", "/dev/ttys002"}
	out := "100 /dev/ttys001 alpha\n200 /dev/ttys002 beta\n999 /dev/ttys009 bystander\n"
	if name, tty := pickWork(out, allow); name != "beta" || tty != "/dev/ttys002" {
		t.Errorf("pickWork = (%q,%q), want (beta, /dev/ttys002)", name, tty)
	}
	// focus moves back to the first pane: its activity now leads
	out = "300 /dev/ttys001 alpha\n200 /dev/ttys002 beta\n"
	if name, tty := pickWork(out, allow); name != "alpha" || tty != "/dev/ttys001" {
		t.Errorf("pickWork after refocus = (%q,%q), want (alpha, /dev/ttys001)", name, tty)
	}
}
