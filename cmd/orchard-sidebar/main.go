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

func tickAfter(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

// One in-flight request per lane: each lane schedules its next tick only when
// its data message lands, so a slow response can never be overwritten by an
// older poll that finished later.
func (m *model) Init() tea.Cmd {
	return tea.Batch(fetchFast, fetchSlow, fetchHooksWith(nil), fetchClientSession(0),
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
		m.height = msg.Height
		m.applyWidth(msg.Width)
		return nil
	case clientSessMsg:
		next := tickAfter(clientEvery, clientTickMsg{})
		// A read that started before the last switch carries the old world;
		// applying it is the visible flicker. Drop it.
		if msg.gen != m.clientGen {
			return next
		}
		// tmux is the authority here — no grace window, no daemon reconciliation.
		// If the name is empty the read failed; keep the last known good value
		// rather than dropping the bar.
		if msg.name != "" && msg.name != m.cursorSess {
			m.followSession(msg.name)
		}
		return next
	case clientTickMsg:
		return fetchClientSession(m.clientGen)
	case animTickMsg:
		m.frame++
		return tickAfter(animEvery, animTickMsg{})
	case fastTickMsg:
		return tea.Batch(fetchFast, fetchHooksWith(m.paneToSess))
	case slowTickMsg:
		return fetchSlow
	case fastDataMsg:
		m.applyFast(msg)
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
			logf("tmux subscription: %v", msg.err)
			return nil
		}
		m.subErr = nil
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

// mouseCmd is the one mouse gesture that produces a command rather than a
// state change: a click on a git-box line copies its payload.
func (m *model) mouseCmd(msg tea.MouseMsg) tea.Cmd {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft ||
		m.menuOpen() || m.pane.launchZone.hit(msg.X, msg.Y) ||
		m.pane.collapseZone.hit(msg.X, msg.Y) || m.pane.updateZone.hit(msg.X, msg.Y) {
		return nil
	}
	if payload, ok := m.pane.copyAtLine(msg.Y); ok {
		return copyCmd(payload)
	}
	return nil
}

// mouse routes a mouse event. Clicking a card is the same gesture as walking
// onto it with j/k: selection and the attached session are one thing, not two.
// Git-box lines copy instead of selecting — they map to no row, and mouseCmd
// has already claimed the click. The wheel scrolls the list and nothing else —
// no selection, so no attach. tmux forwards wheel events into this pane like
// any other mouse report once mouse mode is on.
func (m *model) mouse(msg tea.MouseMsg) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		// the menu is anchored to a card, and the cards are about to move out
		// from under it
		m.closeMenu()
		m.scrollBy(-wheelStep)
		return
	case tea.MouseButtonWheelDown:
		m.closeMenu()
		m.scrollBy(wheelStep)
		return
	}
	if msg.Action != tea.MouseActionPress {
		return
	}
	// right-click is the row menu (menu.go) — rename or close the session
	// under the pointer. It never selects, so it never attaches.
	if msg.Button == tea.MouseButtonRight {
		m.rightClick(msg.X, msg.Y)
		return
	}
	if msg.Button != tea.MouseButtonLeft {
		return
	}
	// an open menu owns every left click: on an item it acts, anywhere else it
	// dismisses, and either way the click stops there rather than reaching the
	// card the box is covering
	if m.menuOpen() {
		m.menuClick(msg.X, msg.Y)
		return
	}
	// the collapse button first: it owns a small rectangle of the header (or
	// the whole strip when collapsed), and neither maps to a row or a payload
	if m.pane.collapseZone.hit(msg.X, msg.Y) {
		m.toggleCollapse()
		return
	}
	if m.pane.updateZone.hit(msg.X, msg.Y) {
		m.openUpdateOverlay()
		return
	}
	if m.pane.launchZone.hit(msg.X, msg.Y) {
		m.openLaunch()
		return
	}
	if _, ok := m.pane.copyAtLine(msg.Y); ok {
		return // mouseCmd is copying it; a copy never also selects
	}
	if ri, ok := m.pane.rowAtLine(msg.Y); ok {
		m.selectRow(ri, true)
	}
}

// wheelStep is how many rendered lines one wheel notch moves. Cards are a few
// lines tall, so one line per notch would feel stuck.
const wheelStep = 3

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
		// resolved once: addFakes ran fakeCount() twice a second, re-reading
		// the environment and rebuilding the same synthetic list every time
		fakes: fakeRows(fakeCount()),
	}
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go subscribeTmux(ctx, prog.Send)
	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
