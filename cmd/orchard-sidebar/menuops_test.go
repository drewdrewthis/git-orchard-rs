package main

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The menu's two mutations (menuops.go): what they send to tmux, what they
// carry across in the model afterwards, and the guards that stop them.

// A rename has to move every identity that pointed at the old name, or the
// selection, the scroll anchor and the hook overlay all lose the row for the
// two seconds until the next poll — and applyHooks re-adds a ghost card under
// the old name in the meantime.
func TestRenameCarriesEveryIdentityToTheNewName(t *testing.T) {
	spy := newMenuSpy(t)
	m := realModel()
	y, _ := rowLine(t, m)
	m = rightClick(m, 1, y)
	old := m.menu.sess

	m.key(tea.KeyMsg{Type: tea.KeyEnter}) // Rename
	m.key(tea.KeyMsg{Type: tea.KeyCtrlU})
	for _, r := range "new.name" { // a "." would make every later -t ambiguous
		m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.key(tea.KeyMsg{Type: tea.KeyEnter})

	if got, want := spy.log(), "rename "+old+" new-name"; got != want {
		t.Fatalf("tmux calls were %q, want %q", got, want)
	}
	if m.menuOpen() {
		t.Error("a successful rename left the menu open")
	}
	if m.rows[m.cursor].session != "new-name" {
		t.Errorf("the card is still named %q", m.rows[m.cursor].session)
	}
	if m.cursorSess != "new-name" {
		t.Errorf("the selection is still on %q", m.cursorSess)
	}
	if _, ok := m.hooksBySess["new-name"]; !ok || len(m.hooksBySess) != 1 {
		t.Errorf("hook state did not follow the rename: %v", m.hooksBySess)
	}
	if !m.attachedBySess["new-name"] || len(m.attachedBySess) != 1 {
		t.Errorf("attach state did not follow the rename: %v", m.attachedBySess)
	}
	if m.paneToSess["%1"] != "new-name" {
		t.Errorf("pane map still points at %q", m.paneToSess["%1"])
	}
}

// A rename that fails keeps the modal open with tmux's own message on it:
// closing on failure is indistinguishable from having worked.
func TestFailedRenameKeepsTheMenuOpenWithTheError(t *testing.T) {
	newMenuSpy(t)
	renameSession = func(string, string) error { return errors.New("duplicate session: taken") }
	m := realModel()
	y, _ := rowLine(t, m)
	m = rightClick(m, 1, y)
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	m.key(tea.KeyMsg{Type: tea.KeyCtrlU})
	for _, r := range "taken" {
		m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.key(tea.KeyMsg{Type: tea.KeyEnter})

	if m.menu.mode != menuRename {
		t.Fatalf("a failed rename left mode %v", m.menu.mode)
	}
	if !strings.Contains(m.menu.notice, "duplicate session") {
		t.Errorf("notice is %q, want tmux's own message", m.menu.notice)
	}
	if !strings.Contains(ansi.Strip(viewOf(m)), "duplicate session") {
		t.Error("the error was not drawn in the menu")
	}
	if m.rows[m.cursor].session == "taken" {
		t.Error("a failed rename renamed the card anyway")
	}
}

// Close asks first. Anything that is not a y is a no — a confirm that can be
// dismissed INTO the action is not a confirm.
func TestCloseConfirmsBeforeKilling(t *testing.T) {
	spy := newMenuSpy(t)
	m := realModel()
	y, _ := rowLine(t, m)

	m = rightClick(m, 1, y)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}) // -> Close
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.mode != menuConfirm {
		t.Fatalf("Close went straight to mode %v", m.menu.mode)
	}
	if !strings.Contains(ansi.Strip(viewOf(m)), "y/N") {
		t.Errorf("the confirm line is not on screen:\n%s", ansi.Strip(viewOf(m)))
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.menuOpen() {
		t.Error("n left the menu open")
	}
	if spy.log() != "" {
		t.Fatalf("n killed something: %s", spy.log())
	}

	m = rightClick(m, 1, y)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !strings.Contains(spy.log(), "kill ") {
		t.Fatalf("y did not kill: %s", spy.log())
	}
	for _, r := range m.rows {
		if r.session == "alpha" {
			t.Error("the killed session's card is still in the list")
		}
	}
}

// The guard that matters: tmux drops a client whose session dies, and on this
// wrapper that client IS the user's terminal. So the client moves off the
// session first, and the switch has to happen BEFORE the kill, not after.
func TestCloseSwitchesTheClientOffItsOwnSessionFirst(t *testing.T) {
	spy := newMenuSpy(t)
	m := realModel() // cursorSess is "alpha": the client is sitting in it
	y, _ := rowLine(t, m)
	m = rightClick(m, 1, y)
	if m.menu.sess != m.cursorSess {
		t.Fatalf("setup: menu is on %q but the client is on %q", m.menu.sess, m.cursorSess)
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	if got, want := spy.log(), "switch beta | kill alpha"; got != want {
		t.Errorf("tmux calls were %q, want %q", got, want)
	}
	if m.cursorSess != "beta" {
		t.Errorf("the selection stayed on the killed session (%q)", m.cursorSess)
	}
}

// ...and with nowhere to move the client to, the close is refused outright
// rather than taking the terminal down with the session.
func TestCloseRefusesTheLastSession(t *testing.T) {
	spy := newMenuSpy(t)
	m := realModel()
	m.rows = m.rows[:1] // "alpha" alone, and the client is in it
	m.reanchorCursor()
	viewOf(m)
	y, _ := rowLine(t, m)
	m = rightClick(m, 1, y)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	if spy.log() != "" {
		t.Fatalf("the last session was closed anyway: %s", spy.log())
	}
	if m.menu.mode != menuConfirm {
		t.Errorf("a refused close left mode %v, want the confirm still up", m.menu.mode)
	}
	if !strings.Contains(m.menu.notice, "last session") {
		t.Errorf("notice is %q, want it to say why", m.menu.notice)
	}
	if len(m.rows) != 1 {
		t.Error("a refused close dropped the card anyway")
	}
}

// A synthetic row names no tmux session (see fake.go). Its menu opens — the
// scroll test data has to look like the real thing — but both actions decline
// and say so, rather than sending tmux a target that does not exist.
func TestSyntheticRowMenuActionsAreNoOps(t *testing.T) {
	spy := newMenuSpy(t)
	m := fakeModel(t, 30, 42, 40)
	y, want := clickLine(t, m)
	m = rightClick(m, 5, y)
	if !m.menuOpen() || !m.menu.fake {
		t.Fatalf("menu on synthetic row %q: open=%v fake=%v",
			m.rows[want].session, m.menuOpen(), m.menu.fake)
	}

	m.key(tea.KeyMsg{Type: tea.KeyEnter}) // Rename
	for _, r := range "-x" {
		m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.menu.notice, "synthetic") {
		t.Errorf("rename notice is %q", m.menu.notice)
	}

	m.closeMenu()
	m = rightClick(m, 5, y)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !strings.Contains(m.menu.notice, "synthetic") {
		t.Errorf("close notice is %q", m.menu.notice)
	}
	if spy.log() != "" {
		t.Fatalf("a synthetic row reached tmux: %s", spy.log())
	}
	if len(m.rows) != 30 {
		t.Errorf("a synthetic close dropped a row: %d left", len(m.rows))
	}
}

// Closing a session the client is NOT sitting in never moves the client: the
// switch is a guard against killing the terminal's own session, not part of
// closing one. Switching anyway would drag the user off whatever they were
// looking at every time they tidied up a background session.
func TestCloseOfAnotherSessionDoesNotMoveTheClient(t *testing.T) {
	spy := newMenuSpy(t)
	m := realModel() // the client is sitting in "alpha"
	m.cursorSess = "alpha"
	viewOf(m)
	y, ri := rowLine(t, m)
	if m.rows[ri].session == m.cursorSess {
		// rowLine found alpha's card; take the other row's line instead
		for line, row := range m.pane.lineToRow {
			if row >= 0 && m.rows[row].session != m.cursorSess {
				y, ri = line, row
				break
			}
		}
	}
	target := m.rows[ri].session
	if target == m.cursorSess {
		t.Fatalf("setup: no row other than the attached session %q", m.cursorSess)
	}
	m = rightClick(m, 1, y)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}) // Close
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	if got, want := spy.log(), "kill "+target; got != want {
		t.Fatalf("tmux calls were %q, want %q — a switch has no business here", got, want)
	}
	if m.cursorSess != "alpha" {
		t.Errorf("the selection moved to %q, want it left on alpha", m.cursorSess)
	}
	for _, r := range m.rows {
		if r.session == target {
			t.Errorf("the killed session's card is still on screen")
		}
	}
	if m.menuOpen() {
		t.Error("a successful close left the menu open")
	}
}
