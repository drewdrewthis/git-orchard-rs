package main

import (
	"errors"
	"strings"
	"testing"
)

func TestSortSessionsByRecency_NewestAttachedFirst(t *testing.T) {
	got := sortSessionsByRecency("100 old\n300 newest\n200 middle\n")
	want := []string{"newest", "middle", "old"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sortSessionsByRecency = %v; want %v", got, want)
	}
}

// A never-attached session reports 0 and must sort last rather than being
// dropped or landing at the front.
func TestSortSessionsByRecency_NeverAttachedSortsLast(t *testing.T) {
	got := sortSessionsByRecency("0 fresh\n500 used\n")
	if len(got) != 2 || got[0] != "used" || got[1] != "fresh" {
		t.Errorf("sortSessionsByRecency = %v; want [used fresh]", got)
	}
}

// A tmux without #{session_last_attached} renders it empty, leaving the name
// alone on the line. A name with a space in it must survive that path whole.
func TestSortSessionsByRecency_ToleratesANameOnlyLine(t *testing.T) {
	got := sortSessionsByRecency("solo\n")
	if len(got) != 1 || got[0] != "solo" {
		t.Errorf("sortSessionsByRecency = %v; want [solo] — a session must never be dropped", got)
	}
	got = sortSessionsByRecency("my session\n")
	if len(got) != 1 || got[0] != "my session" {
		t.Errorf("sortSessionsByRecency = %v; want [\"my session\"] — an unstamped name must not be truncated", got)
	}
	got = sortSessionsByRecency("400 my session\n")
	if len(got) != 1 || got[0] != "my session" {
		t.Errorf("sortSessionsByRecency = %v; want [\"my session\"] — only the stamp is split off", got)
	}
}

func TestResolveSession_DefaultsToMostRecentlyAttached(t *testing.T) {
	f := newFakeTmux().reply(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"),
		"100 a\n900 b\n")
	w := testWrapper(f)

	got, err := w.resolveSession()
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if got != "b" {
		t.Errorf("resolveSession() = %q; want the most recently attached session b", got)
	}
}

func TestResolveSession_ExplicitSessionWins(t *testing.T) {
	f := newFakeTmux().reply(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"),
		"100 a\n900 b\n")
	w := testWrapper(f, func(o *Options) { o.Session = "a" })

	got, err := w.resolveSession()
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if got != "a" {
		t.Errorf("resolveSession() = %q; want a", got)
	}
}

// @scenario Requesting a missing inner session lists what exists
//
// AC3: `orchard shell --session nope` with sessions a,b exits 2 and prints
// both names.
func TestResolveSession_MissingSessionNamesWhatExists(t *testing.T) {
	f := newFakeTmux().reply(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"),
		"100 a\n200 b\n")
	w := testWrapper(f, func(o *Options) { o.Session = "nope" })

	_, err := w.resolveSession()
	if err == nil {
		t.Fatal("resolveSession succeeded for a session that does not exist")
	}
	var missing *sessionMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("error is %T; want *sessionMissingError", err)
	}
	for _, name := range []string{"nope", "a", "b"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %q", err, name)
		}
	}
	if got := exitCodeFor(err); got != exitSessionMissing {
		t.Errorf("exit code = %d; want %d", got, exitSessionMissing)
	}
}

// @scenario No inner server prints the orchard new hint
//
// AC3: an inner socket with no server exits non-zero, names the socket, and
// creates no outer session.
func TestEnsureReady_NoInnerServerNamesTheSocketAndCreatesNothing(t *testing.T) {
	f := newFakeTmux().
		fail(outerCall("has-session", "-t", outerSessionName), "no server running").
		fail(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"), "no server running on /tmp/tmux-501/nosuchsocket")
	w := testWrapper(f, func(o *Options) { o.InnerSocket = "nosuchsocket" })

	err := w.ensureReady()
	if err == nil {
		t.Fatal("ensureReady succeeded with no inner server")
	}
	var noServer *noInnerServerError
	if !errors.As(err, &noServer) {
		t.Fatalf("error is %T; want *noInnerServerError", err)
	}
	if !strings.Contains(err.Error(), "nosuchsocket") {
		t.Errorf("error %q does not name the socket", err)
	}
	if !strings.Contains(err.Error(), "orchard new") {
		t.Errorf("error %q does not offer the orchard new hint", err)
	}
	if got := f.mutations(); len(got) != 0 {
		t.Errorf("an outer session was built despite the failure: %v", got)
	}
	if got := exitCodeFor(err); got != 1 {
		t.Errorf("exit code = %d; want 1 (2 is reserved for a missing session)", got)
	}
}

func TestResolveSession_EmptyInnerServerIsNoInnerServer(t *testing.T) {
	f := newFakeTmux().reply(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"), "")
	w := testWrapper(f)

	_, err := w.resolveSession()
	var noServer *noInnerServerError
	if !errors.As(err, &noServer) {
		t.Fatalf("error is %v (%T); want *noInnerServerError", err, err)
	}
}
