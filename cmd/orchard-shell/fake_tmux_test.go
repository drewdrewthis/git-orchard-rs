package main

import (
	"fmt"
	"strings"
)

// fakeTmux answers tmux invocations from canned replies and records every
// call, so the wrapper's decision logic is testable without a tmux server.
//
// A key is the full argv joined by spaces — socket and `-f` flags included —
// which is deliberate: it makes "every outer invocation passes -f" something
// the tests can assert rather than assume.
type fakeTmux struct {
	replies map[string]fakeReply
	calls   []string
}

type fakeReply struct {
	out string
	err error
}

func newFakeTmux() *fakeTmux {
	return &fakeTmux{replies: map[string]fakeReply{}}
}

// reply registers stdout for an exact argv.
func (f *fakeTmux) reply(argv, out string) *fakeTmux {
	f.replies[argv] = fakeReply{out: out}
	return f
}

// fail registers a failure for an exact argv — how tmux reports "no server
// running", "can't find session" and every other refusal.
func (f *fakeTmux) fail(argv, msg string) *fakeTmux {
	f.replies[argv] = fakeReply{err: fmt.Errorf("tmux %s: %s", argv, msg)}
	return f
}

// exec is the tmuxExec this fake provides. An unregistered argv succeeds with
// empty output: mutations (send-keys, select-pane, split-window) are the
// common case and would otherwise all need registering.
func (f *fakeTmux) exec(args ...string) (string, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if r, ok := f.replies[key]; ok {
		return r.out, r.err
	}
	return "", nil
}

// called reports whether any recorded call contains sub.
func (f *fakeTmux) called(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// mutations returns the recorded calls that change state, dropping the
// read-only probes so a test can assert "nothing was created".
func (f *fakeTmux) mutations() []string {
	var out []string
	for _, c := range f.calls {
		switch {
		case strings.Contains(c, " has-session"),
			strings.Contains(c, " list-sessions"),
			strings.Contains(c, " list-clients"),
			strings.Contains(c, " list-panes"),
			strings.Contains(c, " display "):
			continue
		}
		out = append(out, c)
	}
	return out
}

// testWrapper builds a wrapper over the fake with the defaults orchard shell
// uses.
func testWrapper(f *fakeTmux, mutate ...func(*Options)) *wrapper {
	opts := Options{
		InnerSocket: "inner-test",
		OuterSocket: "outer-test",
		Width:       defaultWidth,
	}
	for _, m := range mutate {
		m(&opts)
	}
	return &wrapper{opts: opts, conf: "/conf/outer.conf", tmux: f.exec, log: &strings.Builder{}}
}

// outerCall renders the argv the wrapper would use for an outer-server
// command, for registering replies.
func outerCall(args ...string) string {
	return strings.Join(outerArgs("outer-test", "/conf/outer.conf", args...), " ")
}

// innerCall renders the argv the wrapper would use for an inner-server
// command.
func innerCall(args ...string) string {
	return strings.Join(innerArgs("inner-test", args...), " ")
}
