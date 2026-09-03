package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// menuSpy replaces every tmux mutation the menu can reach and records the
// order they were called in — the kill guard is an ORDERING property (switch
// the client off a session before killing it), which a per-call flag cannot
// express.
type menuSpy struct{ calls []string }

func newMenuSpy(t *testing.T) *menuSpy {
	t.Helper()
	s := &menuSpy{}
	// Default to a single-pane active window so the break-pane item stays
	// absent and the existing action list is Rename/Close/Pin; break-pane
	// tests override paneInfo to offer it.
	pi := paneInfo
	paneInfo = func(string) (string, int) { return "", 1 }
	t.Cleanup(func() { paneInfo = pi })
	rn, kl, sw, hb := renameSession, killSession, switchClient, handBackFocus
	renameSession = func(old, name string) error {
		s.calls = append(s.calls, "rename "+old+" "+name)
		return nil
	}
	killSession = func(name string) error {
		s.calls = append(s.calls, "kill "+name)
		return nil
	}
	switchClient = func(name string, handBack bool) {
		s.calls = append(s.calls, "switch "+name)
	}
	handBackFocus = func(outerPane) { s.calls = append(s.calls, "handback") }
	t.Cleanup(func() {
		renameSession, killSession, switchClient, handBackFocus = rn, kl, sw, hb
	})
	return s
}

func (s *menuSpy) log() string { return strings.Join(s.calls, " | ") }

// realModel is a pane of ordinary (non-synthetic) sessions with the maps a
// live sidebar carries, so a rename can be checked against all of them.
func realModel() *model {
	m := &model{
		rows: []row{
			{session: "alpha", state: "idle", hooked: true, cwd: "/w/alpha"},
			{session: "beta", state: "input", hooked: true, cwd: "/w/beta"},
		},
		hooksBySess:    map[string]hookState{"alpha": {state: "idle"}},
		attachedBySess: map[string]bool{"alpha": true},
		paneToSess:     map[string]string{"%1": "alpha", "%2": "beta"},
		cursorSess:     "alpha",
		stateDirOK:     true,
		width:          42,
		height:         30,
	}
	m.reanchorCursor()
	viewOf(m)
	return m
}

// rowLine is a pane line that maps to a card, and the row it maps to.
func rowLine(t *testing.T, m *model) (y, row int) {
	t.Helper()
	for i, r := range m.pane.lineToRow {
		if r >= 0 {
			return i, r
		}
	}
	t.Fatal("no card line in the rendered pane")
	return -1, -1
}

func rightClick(m *model, x, y int) *model {
	mm, _ := m.Update(tea.MouseMsg{X: x, Y: y,
		Action: tea.MouseActionPress, Button: tea.MouseButtonRight})
	return mm.(*model)
}

// Right-click is "tell me about this one", not "take me there": it opens the
// menu on the card under the pointer and touches nothing else — no selection
// (which would attach the terminal to that session), no scroll, and no focus
// hand-back, since the menu is the one thing here that needs the keyboard.
func TestRightClickOpensTheMenuWithoutSelectingOrScrolling(t *testing.T) {
	spy := newMenuSpy(t)
	m := fakeModel(t, 30, 42, 40)
	for i := 0; i < 6; i++ {
		mm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = mm.(*model)
	}
	viewOf(m)
	off, cur, top := m.scroll, m.cursor, topCard(m)

	y, want := clickLine(t, m)
	m = rightClick(m, 5, y)
	viewOf(m)

	if !m.menuOpen() {
		t.Fatal("right-click did not open the menu")
	}
	if m.menu.sess != m.rows[want].session {
		t.Errorf("menu opened on %q, want the row under the pointer (%q)",
			m.menu.sess, m.rows[want].session)
	}
	if m.cursor != cur {
		t.Errorf("right-click moved the selection: %d -> %d", cur, m.cursor)
	}
	if m.scroll != off || topCard(m) != top {
		t.Errorf("right-click moved the viewport: offset %d -> %d, top %q -> %q",
			off, m.scroll, top, topCard(m))
	}
	if spy.log() != "" {
		t.Errorf("right-click ran tmux: %s", spy.log())
	}
	// and the box is actually on screen, covering rows rather than sitting
	// beside them: its lines must map to nothing clickable
	if !strings.Contains(ansi.Strip(viewOf(m)), "Rename") {
		t.Errorf("the menu did not render:\n%s", ansi.Strip(viewOf(m)))
	}
	for _, z := range m.pane.menuRows {
		if m.pane.lineToRow[z.y] != -1 {
			t.Errorf("menu line %d still maps to row %d", z.y, m.pane.lineToRow[z.y])
		}
	}
}

// The action list is a two-item loop driven by either spelling of up/down,
// Enter opens the highlighted one, and Esc backs all the way out.
func TestMenuNavigatesAndEscapeCloses(t *testing.T) {
	newMenuSpy(t)
	m := realModel()
	y, _ := rowLine(t, m)
	m = rightClick(m, 1, y)

	if m.menu.item != itemRename {
		t.Fatalf("menu opened on item %d, want Rename", m.menu.item)
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if m.menu.item != itemClose {
		t.Errorf("j left the highlight on item %d, want Close", m.menu.item)
	}
	m.key(tea.KeyMsg{Type: tea.KeyUp})
	if m.menu.item != itemRename {
		t.Errorf("Up left the highlight on item %d, want Rename", m.menu.item)
	}
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.mode != menuRename {
		t.Fatalf("Enter on Rename went to mode %v", m.menu.mode)
	}
	if m.menu.input.value() != "alpha" {
		t.Errorf("the rename field starts at %q, want the current name", m.menu.input.value())
	}
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if m.menuOpen() {
		t.Error("esc left the menu open")
	}
}

// While the menu is up it owns the keyboard: q must not quit the sidebar and
// j must not walk the selection, which would attach a different session out
// from under the menu the user is reading.
func TestMenuKeysDoNotReachTheList(t *testing.T) {
	newMenuSpy(t)
	m := realModel()
	y, _ := rowLine(t, m)
	m = rightClick(m, 1, y)
	cur := m.cursor

	if cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}); cmd != nil {
		t.Error("j returned a command while the menu was open")
	}
	if m.cursor != cur {
		t.Errorf("j moved the selection under an open menu: %d -> %d", cur, m.cursor)
	}
	if cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}); cmd != nil {
		t.Error("q quit the sidebar instead of closing the menu")
	}
	if m.menuOpen() {
		t.Error("q left the menu open")
	}
	// ctrl-c is the exception: nothing may swallow it
	m = rightClick(m, 1, y)
	if cmd := m.key(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Error("ctrl-c was swallowed by the menu")
	}
}

// A click on a menu item runs it; a click outside dismisses. The dismissing
// click must not fall through to the card the box was covering — that would
// attach a session on the way out of a menu.
func TestMenuClicksActAndDismissWithoutFallingThrough(t *testing.T) {
	newMenuSpy(t)
	m := realModel()
	y, _ := rowLine(t, m)
	m = rightClick(m, 1, y)
	viewOf(m)

	closeItem := m.pane.menuRows[itemClose]
	mm, _ := m.Update(tea.MouseMsg{X: closeItem.x, Y: closeItem.y,
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = mm.(*model)
	if m.menu.mode != menuConfirm {
		t.Fatalf("clicking Close went to mode %v", m.menu.mode)
	}

	viewOf(m)
	cur := m.cursor
	away := m.pane.menuBox.y + m.pane.menuBox.h + 1
	if away >= len(m.pane.lineToRow) {
		away = 0 // the header: outside the box either way
	}
	mm, _ = m.Update(tea.MouseMsg{X: 1, Y: away,
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = mm.(*model)
	if m.menuOpen() {
		t.Error("a click outside the box left the menu open")
	}
	if m.cursor != cur {
		t.Errorf("the dismissing click fell through and selected row %d", m.cursor)
	}
}

// The box is furniture like the git box: every line exactly the pane's width,
// so nothing soft-wraps and skews the line->row map underneath it.
func TestMenuBoxLinesAreExactlyOneRowWide(t *testing.T) {
	newMenuSpy(t)
	for _, w := range []int{34, 40, 42, 60} {
		m := realModel()
		m.width = w
		viewOf(m)
		y, _ := rowLine(t, m)
		m = rightClick(m, 1, y)
		for _, mode := range []menuMode{menuActions, menuRename, menuConfirm} {
			m.menu.mode, m.menu.notice = mode, "a notice"
			m.menu.input = newTextField("some-name", 20)
			for i, l := range m.menuLines(w) {
				if got := ansi.StringWidth(l); got != w-3 {
					t.Errorf("w=%d mode=%v line %d is %d cells, want %d: %q",
						w, mode, i, got, w-3, ansi.Strip(l))
				}
			}
		}
	}
}
