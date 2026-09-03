package main

import (
	"strings"
	"testing"
)

// A `-L` socket does not stop tmux loading the user's ~/.tmux.conf, so an
// outer invocation without `-f` silently inherits their real config.
func TestOuterArgs_AlwaysCarriesTheConfig(t *testing.T) {
	got := strings.Join(outerArgs("sock", "/c.conf", "has-session", "-t", "shell"), " ")
	if got != "-L sock -f /c.conf has-session -t shell" {
		t.Errorf("outerArgs = %q", got)
	}
}

// The inner server is the user's own and must keep loading the user's config.
func TestInnerArgs_NeverCarriesAConfig(t *testing.T) {
	got := strings.Join(innerArgs("sock", "list-sessions"), " ")
	if got != "-L sock list-sessions" {
		t.Errorf("innerArgs = %q", got)
	}
}

func TestEveryOuterInvocationDuringBootPassesDashF(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_tty}"), "/dev/ttys004").
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_id}"), "%3")
	w := testWrapper(f)

	if err := w.boot("work"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := w.focusInner(); err != nil {
		t.Fatalf("focusInner: %v", err)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "-L outer-test -f /conf/outer.conf ") {
			t.Errorf("outer invocation without -f: %q", c)
		}
	}
}

// Sending to 0.0 before the split targets the pre-split sole pane, which the
// split then renumbers to 0.1 — both commands land in one pane and the real
// 0.0 gets nothing.
func TestBoot_SplitsBeforeSendingAnyKeys(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_tty}"), "/dev/ttys004").
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_id}"), "%3")
	w := testWrapper(f)

	if err := w.boot("work"); err != nil {
		t.Fatalf("boot: %v", err)
	}

	split, firstKeys := -1, -1
	for i, c := range f.calls {
		if strings.Contains(c, "split-window") && split < 0 {
			split = i
		}
		if strings.Contains(c, "send-keys") && firstKeys < 0 {
			firstKeys = i
		}
	}
	if split < 0 || firstKeys < 0 {
		t.Fatalf("boot did not split and send keys; calls: %v", f.calls)
	}
	if split > firstKeys {
		t.Errorf("split-window ran at %d, after the first send-keys at %d; calls: %v", split, firstKeys, f.calls)
	}
}

// The tty handed to the sidebar is pane 0.1's, and it is only the inner
// client's tty once that client's attach has been sent.
func TestBoot_ResolvesTheInnerTTYAfterSendingTheAttach(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_tty}"), "/dev/ttys004").
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_id}"), "%3")
	w := testWrapper(f)

	if err := w.boot("work"); err != nil {
		t.Fatalf("boot: %v", err)
	}

	attachIdx, ttyIdx := -1, -1
	for i, c := range f.calls {
		if strings.Contains(c, paneInner) && strings.Contains(c, "send-keys") {
			attachIdx = i
		}
		if strings.Contains(c, "#{pane_tty}") && ttyIdx < 0 {
			ttyIdx = i
		}
	}
	if attachIdx < 0 || ttyIdx < 0 || ttyIdx < attachIdx {
		t.Errorf("pane_tty read at %d, inner attach sent at %d; the read must come second. calls: %v", ttyIdx, attachIdx, f.calls)
	}
}

// @scenario --width sets the sidebar pane's initial column count
func TestBoot_SizesTheSessionAndPinsTheSidebarWidth(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_tty}"), "/dev/ttys004").
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_id}"), "%3")
	w := testWrapper(f, func(o *Options) { o.Width = 55 })

	if err := w.boot("work"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if !f.called("split-window -h -b -l 55") {
		t.Errorf("--width 55 did not reach split-window; calls: %v", f.calls)
	}
	if !f.called("new-session -d -s " + outerSessionName + " -x ") {
		t.Errorf("new-session was not given an explicit size; calls: %v", f.calls)
	}
}

// @scenario The inner attach clears TMUX before connecting
//
// Without TMUX= tmux hard-refuses to nest and the pane sits at a dead prompt.
func TestInnerAttachCommand_ClearsTMUX(t *testing.T) {
	got := innerAttachCommand("inner", "work")
	if !strings.HasPrefix(got, "TMUX= ") {
		t.Errorf("innerAttachCommand = %q; want a literal TMUX= prefix", got)
	}
	if got != "TMUX= tmux -L inner attach -t work" {
		t.Errorf("innerAttachCommand = %q", got)
	}
}

func TestSidebarCommand_CarriesTheWholeEnvContract(t *testing.T) {
	got := sidebarCommand("/opt/bin/orchard-sidebar", "inner", "/dev/ttys004", "%3")
	want := "ORCHARD_TMUX_SOCKET=inner ORCHARD_TMUX_CLIENT=/dev/ttys004 ORCHARD_OUTER_PANE=%3 /opt/bin/orchard-sidebar"
	if got != want {
		t.Errorf("sidebarCommand = %q; want %q", got, want)
	}
}

func TestPlaceholderCommand_WatchesTheInnerServer(t *testing.T) {
	got := placeholderCommand("inner")
	if !strings.Contains(got, "tmux -L inner list-windows -a") {
		t.Errorf("placeholderCommand = %q", got)
	}
}

func TestShellQuote_LeavesOrdinaryNamesAloneAndQuotesTheRest(t *testing.T) {
	cases := map[string]string{
		"work":             "work",
		"/dev/ttys004":     "/dev/ttys004",
		"orchard-shell-t2": "orchard-shell-t2",
		// A pane id is shell-safe; quoting it would only obscure the pane command.
		"%3":         "%3",
		"my session": "'my session'",
		"it's":       `'it'\''s'`,
		"":           "''",
		"a;rm -rf /": "'a;rm -rf /'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q; want %q", in, got, want)
		}
	}
}

// A session name with a space must survive the round trip through send-keys.
func TestInnerAttachCommand_QuotesAwkwardSessionNames(t *testing.T) {
	got := innerAttachCommand("inner", "my session")
	if got != "TMUX= tmux -L inner attach -t 'my session'" {
		t.Errorf("innerAttachCommand = %q", got)
	}
}
