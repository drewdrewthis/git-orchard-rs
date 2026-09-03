package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Pane → own session: break the attached session's active pane out into a new
// inner tmux session of its own, so a long-running pane (a server, a log tail,
// a second Claude) gets its own card, its own M-<n> slot, and its own attach.
// Split from the menu's rename/close mutations (menuops.go) because it is a
// third session mutation with its own multi-step tmux dance and undo.

// breakPaneLabel is the menu item's text; appended to menuActionLabels only on
// the attached session's multi-pane active window.
const breakPaneLabel = "Pane → own session…"

// paneInfo reports the active window's active-pane id and pane count for a
// session. A var so the menu-gating test can vary the count without a live
// tmux; the real one asks the inner server. A failure returns a zero count,
// which drops the menu item — the same fail-quiet stance as takenSessions.
var paneInfo = func(sess string) (active string, count int) {
	out, err := env.innerCmd("list-panes", "-t", sess+":", "-F", "#{pane_id} #{pane_active}").Output()
	if err != nil {
		return "", 0
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l == "" {
			continue
		}
		count++
		if strings.HasSuffix(l, " 1") {
			active = strings.Fields(l)[0]
		}
	}
	return active, count
}

// breakPaneToSession moves paneID into a brand-new session called name. A var
// so the flow's test can record the exact three-step argv and inject a
// mid-sequence failure. Each step's error is wrapped with the step that failed
// so the sidebar's status line names it. On a break-pane failure the empty
// session created by step one is killed, so a failure never leaves a stray
// windowless session behind.
var breakPaneToSession = func(paneID, name string) error {
	if err := runTmux("new-session", "-d", "-s", name); err != nil {
		return fmt.Errorf("pane → session failed: new-session: %w", err)
	}
	if err := runTmux("break-pane", "-d", "-s", paneID, "-t", name+":"); err != nil {
		_ = runTmux("kill-session", "-t", name) // undo the empty session
		return fmt.Errorf("pane → session failed: break-pane: %w", err)
	}
	if err := runTmux("kill-window", "-t", name+":0"); err != nil {
		return fmt.Errorf("pane → session failed: kill-window: %w", err)
	}
	return nil
}

// breakPaneKey drives the name field, mirroring renameKey: Esc and Enter are
// the menu's, everything else is the text field's. Esc cancels with zero tmux
// mutations — nothing has run yet at that point.
func (m *model) breakPaneKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.closeMenu()
		return nil
	case tea.KeyEnter:
		m.commitBreakPane()
		return nil
	}
	if msg.Alt && msg.Type == tea.KeyRunes {
		return nil // M-x belongs to the outer wrapper, not to a text field
	}
	return m.menu.input.key(msg)
}

// commitBreakPane runs the break-out on Enter. The name is made collision-free
// the same way a launch is; a tmux failure stays on the menu with the failing
// step named, so no half-state is silent. On success the new session is pinned
// to the TOP of the list (row 0) and the inner client is switched onto it — a
// switch failure surfaces the same way, keeping the menu open with the notice,
// since the break itself already succeeded.
func (m *model) commitBreakPane() {
	name := uniqueName(m.menu.input.value(), takenSessions())
	if err := breakPaneToSession(m.menu.activePane, name); err != nil {
		m.menu.notice = err.Error()
		return
	}
	// prepend → pinRank 1 → sortRows places the new card at row 0
	m.pinned = append([]string{name}, m.pinned...)
	m.persistState()
	m.rebuild()
	if err := switchClientTo(name); err != nil {
		m.menu.notice = err.Error()
		return
	}
	m.closeMenu()
}
