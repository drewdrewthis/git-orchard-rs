package main

import (
	"context"
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
	ctx, cancel := context.WithTimeout(context.Background(), tmuxOpTimeout)
	defer cancel()
	out, err := env.innerCmdContext(ctx, "list-panes", "-t", sess+":", "-F", "#{pane_id} #{pane_active}").Output()
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
// mid-sequence failure. Hard errors (new-session, break-pane) come back in err
// with the step that failed named, so the status line names it; on a break-pane
// failure the empty session created by step one is killed, so a failure never
// leaves a stray windowless session behind.
//
// The placeholder window new-session leaves behind is killed by the window id
// it PRINTS (`-P -F '#{window_id}'`), never a hardcoded `name:0` — the inner
// server loads the user's tmux.conf, so with `base-index 1` the first window is
// `name:1` and a `name:0` kill would miss it. kill-window failure is SOFT: the
// pane already moved, so it comes back in warn (not err) and the caller still
// switches to the new session.
var breakPaneToSession = func(paneID, name string) (warn string, err error) {
	winID, err := runTmuxOutput("new-session", "-d", "-s", name, "-P", "-F", "#{window_id}")
	if err != nil {
		return "", fmt.Errorf("pane → session failed: new-session: %w", err)
	}
	if err := runTmux("break-pane", "-d", "-s", paneID, "-t", name+":"); err != nil {
		_ = runTmux("kill-session", "-t", name) // undo the empty session
		return "", fmt.Errorf("pane → session failed: break-pane: %w", err)
	}
	if err := runTmux("kill-window", "-t", winID); err != nil {
		return fmt.Sprintf("pane → session: kill-window %s failed: %s", winID, err), nil
	}
	return "", nil
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
// the same way a launch is; a HARD tmux failure (new-session, break-pane) stays
// on the menu with the failing step named, so no half-state is silent. On
// success the inner client is switched onto the new session — sortRows
// (model.go) already puts the most-recently-attached session at row 0, so the
// switch alone is what lands the new card at the top; a switch failure surfaces
// the same way, keeping the menu open with the notice, since the break itself
// already succeeded.
//
// A kill-window warning is SOFT: the pane already moved, so the switch still
// runs and then the warning is set on the still-open menu — the break worked,
// only the placeholder window survived.
func (m *model) commitBreakPane() {
	name := uniqueName(m.menu.input.value(), takenSessions())
	warn, err := breakPaneToSession(m.menu.activePane, name)
	if err != nil {
		m.menu.notice = err.Error()
		return
	}
	if err := switchClientTo(name); err != nil {
		m.menu.notice = err.Error()
		return
	}
	if warn != "" {
		m.menu.notice = warn
		return
	}
	m.closeMenu()
}
