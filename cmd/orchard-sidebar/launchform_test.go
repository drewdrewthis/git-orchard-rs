package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The launch modal as the user drives it: browsing to a directory, typing a
// command, and seeing the name it will really launch under before it does.
// The picker itself (search, scrolling) is covered in dirpick_test.go.

// A lone space arrives as KeySpace, not KeyRunes. Reading only KeyRunes dropped
// every space typed into the command field — "sleep 300" became "sleep300".
func TestTypedRunesKeepsSpaces(t *testing.T) {
	space := tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	if got := string(typedRunes(space)); got != " " {
		t.Errorf("space produced %q", got)
	}
	burst := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("cmd")}
	if got := string(typedRunes(burst)); got != "cmd" {
		t.Errorf("burst produced %q", got)
	}
	// alt+h is the hidden-files shortcut; it must never land in a field
	if got := typedRunes(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h"), Alt: true}); got != nil {
		t.Errorf("alt+h produced %q", string(got))
	}
}

// The whole modal in one pass: search, descend, edit the command, launch.
func TestLaunchModalFlow(t *testing.T) {
	stubTaken(t, "cmd")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	var gotDir, gotCmd, gotName string
	prev := launchSession
	launchSession = func(dir, cmd, name string) error {
		gotDir, gotCmd, gotName = dir, cmd, name
		return nil
	}
	t.Cleanup(func() { launchSession = prev })

	m := newLaunchModel(root, "claude")
	press := func(k tea.KeyMsg) { m.Update(k) }
	runes := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
	key := func(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

	press(runes("cmd"))
	press(key(tea.KeyEnter)) // descend into cmd/
	if m.pick.dir != filepath.Join(root, "cmd") {
		t.Fatalf("picker at %q", m.pick.dir)
	}
	// the name follows the directory until you type in it, and "cmd" is taken
	if m.name.value() != "cmd-2" {
		t.Fatalf("name = %q, want cmd-2", m.name.value())
	}
	press(key(tea.KeyTab))
	press(key(tea.KeyCtrlU))
	press(runes("sleep"))
	press(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	press(runes("300"))
	press(key(tea.KeyTab))
	press(key(tea.KeyTab)) // onto the launch button
	if m.focus != focusGo {
		t.Fatalf("focus = %d, want %d", m.focus, focusGo)
	}
	press(key(tea.KeyEnter))
	if !m.launched {
		t.Fatalf("enter on the button did not launch (status %q)", m.status)
	}
	if gotDir != filepath.Join(root, "cmd") || gotCmd != "sleep 300" || gotName != "cmd-2" {
		t.Errorf("launched (%q, %q, %q)", gotDir, gotCmd, gotName)
	}
}

// A failed launch keeps the modal open with the error on it: closing the popup
// on failure looks exactly like a launch that worked.
func TestFailedLaunchKeepsTheModalOpen(t *testing.T) {
	stubTaken(t)
	prev := launchSession
	launchSession = func(dir, cmd, name string) error { return os.ErrPermission }
	t.Cleanup(func() { launchSession = prev })

	m := newLaunchModel(t.TempDir(), "claude")
	m.focus = focusGo
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.launched {
		t.Fatal("modal reported a launch that failed")
	}
	if m.status == "" || !strings.Contains(m.View(), "permission") {
		t.Errorf("no error shown; status %q", m.status)
	}
}

// uniqueName silently suffixing at launch time created a session under a name
// the user never saw. The form shows the resolved name before launching, and
// launches exactly what it showed.
func TestTheFormShowsTheNameItWillLaunch(t *testing.T) {
	stubTaken(t, "taken", "taken-2")
	var got string
	prev := launchSession
	launchSession = func(_, _, name string) error { got = name; return nil }
	t.Cleanup(func() { launchSession = prev })

	m := newLaunchModel(t.TempDir(), "claude")
	m.focus = focusName
	m.name.set("taken")
	m.nameEdited = true

	if want := "taken-3"; m.resolvedName() != want {
		t.Fatalf("resolvedName = %q, want %q", m.resolvedName(), want)
	}
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "→ taken-3") {
		t.Errorf("the form does not show the name it will use:\n%s", view)
	}
	m.focus = focusGo
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got != "taken-3" {
		t.Errorf("launched %q, want the name the form showed", got)
	}

	// a free name shows no hint at all — the arrow is a warning, not chrome
	m.name.set("free")
	if h := m.renamedHint(); h != "" {
		t.Errorf("hint %q shown for a name that needs no resolving", h)
	}
}

// A burst of keystrokes arrives as ONE KeyRunes message. Routing on
// msg.String() matched "cla" against nothing and the field stayed empty —
// exactly when the user was typing fastest.
func TestFieldTakesACoalescedBurst(t *testing.T) {
	stubTaken(t)
	m := newLaunchModel(t.TempDir(), "")
	m.focus = focusCmd
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("claude --resume")})
	m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")})
	if got, want := m.cmd.value(), "claude --resume abc"; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
	// and the same burst rule in the picker: three j's move three rows
	m.focus = focusPick
	m.pick.entries = []string{"..", "a", "b", "c", "d"}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("jjj")})
	if m.pick.cursor != 3 {
		t.Errorf("cursor = %d after a 3-rune burst, want 3", m.pick.cursor)
	}
}
