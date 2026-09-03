package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ---- local tmux lane: which session is THIS client on, right now.
//
// The daemon's tmuxSessions.attached is per-SESSION and global — it answers
// "is anyone looking at this session", never "am I looking at it". It is also
// a poll behind. Both problems land on the one thing the sidebar most needs to
// be right and instant: which card carries the bar. So attach state is read
// from tmux directly, on a fast local tick.
//
// This is a deliberate exception to ADR-016/017/018 (clients don't exec tmux),
// taken on the user's explicit instruction after the daemon path measured too
// slow to use. The daemon still owns everything else: session inventory,
// claude state, model, PR/issue join. Tracked with switchClient under #726.
type clientSessMsg struct {
	name string
	tty  clientTTY // the work client that session belongs to (split focus, #777)
	gen  int       // m.clientGen when the read started; mismatched reads are stale
}

const clientEvery = 150 * time.Millisecond

// fetchClientSession reports the session of the client this sidebar should
// track. Unscoped (no client tty — the sidebar's normal, unwrapped mode)
// that's the most-recently-active client: with one client that is simply
// "where you are", and with several it's the closest thing to "the one you are
// driving" — unlike the daemon's flag it can never report two sessions at
// once. Scoped (wrapped, #747 defect 2) it is instead the one client whose
// #{client_tty} matches: on a shared inner server "most recent activity" can
// pick a bystander client the user never touched from this sidebar.
func fetchClientSession(gen int, work []clientTTY) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		// tty before session: tty is fixed-format but session names can
		// contain spaces, so the free-form field must come last
		out, err := env.innerCmdContext(ctx, "list-clients", "-F",
			"#{client_activity} #{client_tty} #{client_session}").Output()
		if err != nil {
			return clientSessMsg{gen: gen}
		}
		// In split mode the sidebar follows whichever of ITS OWN work clients is
		// most recently active (pickWork), so the bar tracks the last-focused
		// pane; otherwise it is the single scoped client, exactly as before.
		if len(work) > 0 {
			name, tty := pickWork(string(out), work)
			return clientSessMsg{name: name, tty: tty, gen: gen}
		}
		return clientSessMsg{name: pickClient(string(out), env.client), gen: gen}
	}
}

// pickWork chooses the work client the sidebar follows in split mode: the
// most-recently-active among the wrapper's own work panes (allow), returning
// its session and tty so the bar, the switch and the hand-back all follow the
// last-focused pane without a click. A bystander client on a shared inner
// server is never eligible — the same #747 defect-2 guard pickClient applies.
func pickWork(out string, allow []clientTTY) (string, clientTTY) {
	ok := make(map[clientTTY]bool, len(allow))
	for _, t := range allow {
		ok[t] = true
	}
	best, bestTTY, bestAct := "", clientTTY(""), int64(-1)
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		act, rest, k := strings.Cut(ln, " ")
		if !k {
			continue
		}
		tty, sess, k := strings.Cut(rest, " ")
		if !k || sess == "" || !ok[clientTTY(tty)] {
			continue
		}
		n, err := strconv.ParseInt(act, 10, 64)
		if err != nil {
			continue
		}
		if n > bestAct {
			best, bestTTY, bestAct = sess, clientTTY(tty), n
		}
	}
	return best, bestTTY
}

// pickClient selects the client this sidebar should report on from
// list-clients output lines ("<activity> <tty> <session>"). With wantTTY set,
// only the line whose tty matches is eligible — a bystander client on a shared
// inner server (#747 defect 2) is never considered, even when it is more
// recently active. Unset, the most-recently-active client wins (legacy,
// unwrapped mode).
func pickClient(out string, wantTTY clientTTY) string {
	best, bestAct := "", int64(-1)
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		act, rest, ok := strings.Cut(ln, " ")
		if !ok {
			continue
		}
		tty, sess, ok := strings.Cut(rest, " ")
		if !ok || sess == "" {
			continue
		}
		if wantTTY != "" && clientTTY(tty) != wantTTY {
			continue
		}
		n, err := strconv.ParseInt(act, 10, 64)
		if err != nil {
			continue
		}
		if n > bestAct {
			best, bestAct = sess, n
		}
	}
	return best
}
