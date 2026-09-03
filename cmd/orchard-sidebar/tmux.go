package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"time"
)

// Every tmux exec in this package goes through env.innerCmd or env.outerCmd
// (env.go), never exec.Command directly, so which server a command reaches is
// a property of the call and not of the process's ambient environment.

// paneToSession maps tmux pane ids (%5) to session names.
//
// DAEMON-DOWN FALLBACK ONLY. The daemon serves this same mapping on
// tmuxSessions{windows{panes{paneId}}}, and fetchHooks prefers it — a client
// that execs tmux for data the daemon owns violates RULES L7/M2 and the
// ADR-017 anti-pattern. It survives here because the state-dir lane is
// documented to work with the daemon down (header, issue #719), and without
// pane->session every state file looks headless and the sidebar renders empty.
// Steady state no longer touches tmux at all; see orchardist#726 for the
// remaining exec (switchClient), which has no daemon equivalent yet.
// Session names may contain spaces, so the name goes last.
func paneToSession() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := env.innerCmdContext(ctx, "list-panes", "-a", "-F",
		"#{pane_id} #{session_name}").Output()
	m := map[string]string{}
	if err != nil {
		return m
	}
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, name, ok := strings.Cut(ln, " ")
		if !ok {
			continue
		}
		m[id] = name
	}
	return m
}

// sessMeta is a session's ordering keys, read straight from tmux — the daemon
// snapshot carries neither. last_attached is what the list sorts on; created
// breaks ties and orders sessions that have never been attached.
type sessMeta struct {
	lastAttached time.Time
	created      time.Time
}

// sessionOrder reads last_attached and created for every session on the INNER
// server. Client-side tmux exec, same daemon-owns-state exception as
// paneToSession/switchClient (orchardist#726): the schema serves attach and
// the pane map but not these two timestamps, and the sidebar orders the whole
// list by them. The name goes LAST because a session name may contain the tab
// the other two fields are delimited by.
func sessionOrder() map[string]sessMeta {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := env.innerCmdContext(ctx, "list-sessions", "-F",
		sessionOrderFormat).Output()
	if err != nil {
		return map[string]sessMeta{}
	}
	return parseSessionOrder(out)
}

// sessionOrderFormat is the tmux -F string sessionOrder reads back; a const so
// the parser's test drives the exact field order the exec produces.
const sessionOrderFormat = "#{session_last_attached}\t#{session_created}\t#{session_name}"

// parseSessionOrder folds `list-sessions` output into the ordering map. Split
// on newline WITHOUT trimming the whole blob first: a never-attached session
// reports an EMPTY last_attached, so its line begins with the tab delimiter —
// a leading TrimSpace would eat that empty first field and collapse the line to
// two columns, which silently dropped every idle session's created time and
// sank it below the fakes.
func parseSessionOrder(out []byte) map[string]sessMeta {
	m := map[string]sessMeta{}
	for _, ln := range strings.Split(string(out), "\n") {
		if ln == "" {
			continue
		}
		f := strings.SplitN(ln, "\t", 3)
		if len(f) < 3 {
			continue
		}
		m[f[2]] = sessMeta{lastAttached: epochTime(f[0]), created: epochTime(f[1])}
	}
	return m
}

// epochTime parses a tmux #{session_*} unix-epoch field. An empty or unparsable
// value (a session tmux has never recorded attaching to reports "0") becomes
// the zero time, which sortRows treats as never-attached.
func epochTime(s string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// resizePane, setWidthOption, setCollapsed and handBackFocus are vars so tests
// can observe the width and focus traffic without a live tmux. Same
// client-side-exec exception as switchClient.
//
// All four target the OUTER server: the sidebar's own pane and the inner
// client's pane both live there (env.self / env.outer), never on the inner
// server every data exec talks to. Routed inwards they would send an outer
// pane id to a server where that id names a different, unrelated pane.
//
// Each runs off the UI goroutine — a tmux exec must never stall a paint — and
// logs a non-zero exit rather than dropping it (#747 defect 1 was exactly a
// tmux exec failing in silence).
var resizePane = func(w int) {
	if env.self == "" {
		return
	}
	go runOuter(resizePaneArgs(env.self, w)...)
}

// setWidthOption publishes the width the user dragged to, on the OUTER
// server, as the single source of truth for the sidebar's width: outer.conf's
// M-s binding and its client-resized / window-resized hooks re-pin the pane to
// it, so a terminal resize restores the width the user chose instead of a
// hard-coded 40. Publishing it inwards (what this used to do) meant the outer
// hooks re-pinned to 40, the sidebar read that back as a fresh drag, and the
// dragged width was lost.
var setWidthOption = func(w int) {
	if env.self == "" {
		return
	}
	go runOuter(setPaneOptionArgs(env.self, widthOption, strconv.Itoa(w))...)
}

// widthOption and collapsedOption are the two facts the sidebar and the
// wrapper's config share. Both are window options on the outer server, both
// are read by outer.conf, and neither is ever read back by the sidebar —
// which learns its width from the pane size it is handed.
//
// The width is tmux's OWN main-pane-width rather than an @orchard one because
// outer.conf has to read it back synchronously, and only a built-in can be:
// `resize-pane -x` does not expand formats, and the run-shell workaround that
// does returns before its shell has resized anything. `select-layout
// main-vertical` reads main-pane-width inside tmux, with no shell and no
// delay. See the comment above outer.conf's M-s binding.
const (
	widthOption     = "main-pane-width"
	collapsedOption = "@sidebar_collapsed"
)

// setCollapsed drives the pane between its full width and the collapsed
// strip. The @sidebar_collapsed window option is written BEFORE the resize so
// outer.conf's client-resized/window-resized hooks — which re-pin the sidebar
// on every outer-level resize — read the width the sidebar just chose rather
// than the one it just left, and so M-s (bound to the same toggle) agrees with
// what the sidebar is rendering.
var setCollapsed = func(collapsed bool, width int) {
	if env.self == "" {
		return
	}
	go applyCollapsed(env.self, collapsed, width)
}

// applyCollapsed is setCollapsed's body, synchronous, so startup can restore
// the persisted collapse state BEFORE bubbletea reads the pane size — a
// restore that raced the first WindowSizeMsg would look like a fresh drag and
// republish the width it was supposed to be restoring.
func applyCollapsed(p outerPane, collapsed bool, width int) {
	flag := "0"
	if collapsed {
		flag = "1"
	}
	runOuter(setPaneOptionArgs(p, collapsedOption, flag)...)
	runOuter(resizePaneArgs(p, width)...)
}

// runOuter runs one tmux command against the sidebar's own (outer) server and
// logs a non-zero exit rather than dropping it.
func runOuter(args ...string) {
	cmd := env.outerCmd(args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		logf("%s: %v: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
}

// switch/attach helpers (switchClient, renameSession, killSession, runTmux,
// handBackFocus, ...) live in tmux_switch.go.
