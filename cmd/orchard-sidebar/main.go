// orchard-sidebar: a tmux sidebar pane over claude-session-state + the orchard daemon.
//
// Truth layering (issue #719):
//   - state dir (2s): ~/.local/state/claude-sessions/state/*.json written by the
//     claude-session-state plugin hooks — authoritative for working|idle|input
//     and the latest prompt (last_prompt). Works with the daemon down.
//   - daemon fast lane (2s): claudeInstances + tmuxSessions via GraphQL — session
//     inventory, model, pane titles; inference fallback for sessions with no
//     state file (marked "inferred").
//   - daemon slow lane (30s): workView.repos — eagerly walks gh per worktree
//     (measured 27s cold), so it must never block the fast lane.
//
// j/k and clicking both attach the session via `tmux switch-client` — selection
// and the attached session are the same thing. Read-only plus that switch.
//
// CI: statusCheckRollup is collapsed to one status word by prStatus, which only
// ever says "green" on a literal SUCCESS rollup — a queued or unknown rollup
// reads as "unresolved", never green (false-green incident).
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// tickAfter is a var so tests can read back the cadence a lane scheduled
// without waiting on a real timer. Same testing-seam pattern as resizePane.
var tickAfter = func(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

// One in-flight request per lane: each lane schedules its next tick only when
// its data message lands, so a slow response can never be overwritten by an
// older poll that finished later.
func (m *model) Init() tea.Cmd {
	return tea.Batch(fetchFast, fetchSlow, fetchHooksWith(nil), fetchClientSession(0, m.workTTYs()),
		fetchUpdateCheck, tickAfter(animEvery, animTickMsg{}))
}

// Update handles the message, then composes the pane it produced. Composing
// here rather than in View is what makes View a pure accessor: the frame — the
// viewport offset, the mouse maps, the button zones — is resolved exactly once
// per message, not once per repaint.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.compose()
	return m, cmd
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A resize is the loudest hint that the client lane's answer is about
		// to move (a drag, an attach reflow), so the cadence goes back to fast
		// whether or not the width itself ends up changing (#727).
		m.clientTick.reset()
		m.height = msg.Height
		m.applyWidth(msg.Width)
		return nil
	case clientSessMsg:
		// A read that started before the last switch carries the old world;
		// applying it is the visible flicker. It is also no evidence that the
		// answer has settled, so it does not feed the backoff either — the lane
		// just re-ticks at its current cadence (#727).
		if msg.gen != m.clientGen {
			return tickAfter(m.clientTick.interval(), clientTickMsg{})
		}
		next := tickAfter(
			m.clientTick.observe(clientRead{session: msg.name}),
			clientTickMsg{})
		// tmux is the authority here — no grace window, no daemon reconciliation.
		// If the name is empty the read failed; keep the last known good value
		// rather than dropping the bar.
		if msg.name != "" && msg.name != m.cursorSess {
			m.followSession(msg.name)
		}
		// In split mode the winning client's tty says which work pane is focused
		// now — retarget the switch and hand-back at it (#777).
		if msg.tty != "" {
			m.retargetWork(msg.tty)
		}
		return next
	case splitDoneMsg:
		// The doSplit exec ran off the UI goroutine; its result lands here so the
		// split's model state is set on the UI goroutine only (R13 shared state).
		m.splitPending = false // the in-flight doSplit has landed
		if !msg.ok {
			m.setStatus("split failed — see log")
			return nil
		}
		m.splitOpen = true
		m.alt = msg.pane
		return nil
	case clientTickMsg:
		return fetchClientSession(m.clientGen, m.workTTYs())
	case animTickMsg:
		m.frame++
		return tickAfter(animEvery, animTickMsg{})
	case fastTickMsg:
		return tea.Batch(fetchFast, fetchHooksWith(m.paneToSess))
	case slowTickMsg:
		return fetchSlow
	case fastDataMsg:
		m.applyFast(msg)
		// Sampled every fast tick regardless of whether this read succeeded: it
		// is the only thing that notices the push lane going quietly stale
		// (subFresh, no error ever arrives) rather than erroring outright. While
		// push is down the client lane cannot rely on the attach signals that
		// normally re-arm it, so it must not coast on the assumption nothing is
		// happening (#727).
		m.clientTick.observePushHealth(m.subLive())
		return tickAfter(fastEvery, fastTickMsg{})
	case hookDataMsg:
		if msg.err != nil {
			// the state dir is a local read; a failure is a broken install or
			// a permissions problem, not something the user can act on here
			logf("state dir: %v", msg.err)
		}
		m.hooksBySess = msg.bySession
		m.stateDirOK = msg.dirOK
		if msg.order != nil {
			m.sessMeta = msg.order
		}
		m.rebuild()
		return nil
	case tmuxSubMsg:
		if msg.err != nil {
			m.subErr = msg.err // the header marks the degraded lane
			// The lane cannot trust attach signals it isn't receiving: pin the
			// client lane at the fast rung until the push lane recovers (#727).
			m.clientTick.observePushHealth(false)
			logf("tmux subscription: %v (idle %s)", msg.err, msg.idle.Round(time.Millisecond))
			return nil
		}
		m.subErr = nil
		m.clientTick.observePushHealth(true)
		// An attach or detach anywhere means "which session is THIS client on"
		// is about to move, and it is the only such event the sidebar sees
		// without having caused it (a switch from another terminal or keybind).
		// Re-arm the fast cadence before adopting the snapshot; a repeated
		// identical snapshot must not, or the push lane's steady stream would
		// defeat the backoff entirely (#727).
		attached, _ := foldSessions(msg.sessions)
		if !sameAttach(attached, m.attachedBySess) {
			m.clientTick.reset()
		}
		m.applySessions(msg.sessions)
		return nil
	case slowDataMsg:
		if msg.err != nil {
			// the slow lane only enriches (branch, PR, issue); a failure
			// leaves the last join in place rather than blanking the cards
			logf("slow lane: %v", msg.err)
			return tickAfter(slowEvery, slowTickMsg{})
		}
		m.wtBySession = msg.wtBySession
		m.repoBySess = msg.repoBySess
		m.wtByPath = msg.wtByPath
		m.repoByPath = msg.repoByPath
		m.join()
		return tickAfter(slowEvery, slowTickMsg{})
	case updateCheckMsg:
		m.applyUpdateCheck(msg)
		return tickAfter(updateCheckEvery, updateTickMsg{})
	case updateTickMsg:
		return fetchUpdateCheck
	case copiedMsg:
		if msg.err != nil {
			logf("pbcopy: %v", msg.err)
			return nil
		}
		m.copiedUntil = time.Now().Add(2 * time.Second)
		return nil
	case tea.MouseMsg:
		// decided BEFORE the click is applied: a click that dismisses the row
		// menu must not then be read as a click on the git-box line the menu
		// was covering
		cmd := m.mouseCmd(msg)
		m.mouse(msg)
		return cmd
	case tea.KeyMsg:
		return m.key(msg)
	}
	return nil
}

// followSession moves the bar to the session this client is now attached to —
// an attach that happened somewhere else entirely (another pane, another
// window) is still a selection change here, and the viewport follows it.
func (m *model) followSession(name string) {
	m.cursorSess = name
	m.snapSel = true
	// No row yet (brand-new session): the bar goes dark rather than sitting on
	// the card you just left; reanchor finds the row when the daemon serves it.
	m.cursor = -1
	for i, r := range m.rows {
		if r.session == name {
			m.cursor = i
			break
		}
	}
}

// mouseCmd, mouse and wheelStep (the mouse-event dispatch) live in mouse.go.

func main() {
	// --version before anything else: it must answer without a tmux server,
	// a daemon, or a terminal (version.go)
	handleVersionFlag()
	// one binary, two programs: `orchard-sidebar launch` is the modal the +
	// button opens in a tmux popup (see openLaunchPopup)
	if len(os.Args) > 1 && os.Args[1] == "launch" {
		env = readTmuxEnv()
		os.Exit(runLaunch())
	}
	env = readTmuxEnv()
	st := loadSidebarState()
	// synchronously, before bubbletea reads the pane size: a restore that
	// raced the first WindowSizeMsg would look like a fresh drag and publish
	// the pre-restore width back over the one it was restoring
	restoreLayout(st)
	m := &model{
		desiredWidth: st.Width,
		collapsed:    st.Collapsed,
		bell:         st.Bell,
		pinned:       st.Pinned,
		// resolved once: addFakes ran fakeCount() twice a second, re-reading
		// the environment and rebuilding the same synthetic list every time
		fakes: fakeRows(fakeCount()),
	}
	// Bind the switch to THIS model so the exec reads its focus-follow snapshot
	// (m.workOverride) rather than a package global (#777 data-race fix).
	switchClient = m.switchClientBound
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go subscribeTmux(ctx, prog.Send)
	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
