package main

import (
	"fmt"
	"strings"
)

// exitSessionMissing is the exit code for "you asked for a session that is not
// there" — distinct from 1 so a caller can tell a wrong name from a broken
// wrapper without parsing stderr.
const exitSessionMissing = 2

// noInnerServerError means there is no tmux server on the inner socket at
// all: there is nothing to wrap yet.
type noInnerServerError struct {
	socket string
	cause  error
}

func (e *noInnerServerError) Error() string {
	msg := fmt.Sprintf("no tmux server with sessions on socket %q", e.socket)
	if e.cause != nil {
		msg += fmt.Sprintf(" (%v)", e.cause)
	}
	return msg + "\nStart one first:  orchard new   (or: tmux -L " + e.socket + " new -s work)"
}

func (e *noInnerServerError) Unwrap() error { return e.cause }

// sessionMissingError means the inner server is up but does not have the
// requested session. It carries the sessions that DO exist: a bare "not
// found" makes the user go and run list-sessions themselves, and the answer
// is already in hand at the point of failure.
type sessionMissingError struct {
	want   string
	socket string
	have   []string
}

func (e *sessionMissingError) Error() string {
	return fmt.Sprintf("inner session %q not found on socket %q\nSessions on %s:\n  %s",
		e.want, e.socket, e.socket, strings.Join(e.have, "\n  "))
}

// brokenWrapperError means the outer session exists but does not have the
// two-pane shape the wrapper needs, so neither attaching nor respawning is
// safe. The remedy is destructive, so it is printed rather than taken.
type brokenWrapperError struct {
	socket string
}

func (e *brokenWrapperError) Error() string {
	return fmt.Sprintf("outer session %q on socket %q has no pane 0.1 — it is not a wrapper session\nRemedy:  tmux -L %s kill-session -t %s",
		outerSessionName, e.socket, e.socket, outerSessionName)
}
