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

func TestFilterDirsHidesAndMatches(t *testing.T) {
	names := []string{"src", ".git", "Docs", "docker", "target"}
	got := strings.Join(filterDirs(names, false, ""), ",")
	if got != "docker,Docs,src,target" { // case-insensitive sort, hidden dropped
		t.Errorf("unfiltered = %q", got)
	}
	if got := strings.Join(filterDirs(names, true, ""), ","); !strings.Contains(got, ".git") {
		t.Errorf("hidden toggle did not reveal .git: %q", got)
	}
	// typing is case-insensitive: you type "doc", not "Doc"
	if got := strings.Join(filterDirs(names, false, "doc"), ","); got != "docker,Docs" {
		t.Errorf("filtered = %q, want docker,Docs", got)
	}
}

// Typing a filter has to leave the cursor on the first match. Parking it on
// ".." meant typing a directory name and pressing enter walked *up*.
func TestPickerFilterLandsOnTheFirstMatch(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"cmd", "docs", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := newPicker(root)
	if p.entries[0] != parentEntry || p.cursor != 0 {
		t.Fatalf("fresh picker = %v cursor %d", p.entries, p.cursor)
	}
	p.filterKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("cmd")})
	if p.cursor != 1 || p.entries[p.cursor] != "cmd" {
		t.Fatalf("after typing: entries %v cursor %d", p.entries, p.cursor)
	}
	p.enter()
	if p.dir != filepath.Join(root, "cmd") {
		t.Fatalf("enter went to %q", p.dir)
	}
	if p.filter.value() != "" {
		t.Errorf("filter %q survived the descent", p.filter.value())
	}
	p.parent()
	if p.dir != root {
		t.Fatalf("parent went to %q, want %q", p.dir, root)
	}
	p.toggleHidden()
	if !contains(p.entries, ".hidden") {
		t.Errorf("hidden toggle left entries %v", p.entries)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

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

// The whole modal in one pass: filter, descend, edit the command, launch.
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

// The picker's viewport: a directory with more entries than the modal has
// rows scrolls under the cursor instead of overflowing the popup. Off-by-one
// here is a list that hides its last entry or shows one that isn't there.
func TestPickerWindowScrollsUnderTheCursor(t *testing.T) {
	p := &picker{entries: []string{"..", "a", "b", "c", "d", "e"}} // 6 entries
	cases := []struct {
		name   string
		cursor int
		rows   int
		top    int
		window []string
	}{
		{"everything fits", 0, 6, 0, []string{"..", "a", "b", "c", "d", "e"}},
		{"more rows than entries", 3, 10, 0, []string{"..", "a", "b", "c", "d", "e"}},
		{"cursor inside the first window", 2, 3, 0, []string{"..", "a", "b"}},
		{"cursor on the last visible row", 2, 3, 0, []string{"..", "a", "b"}},
		{"cursor past the window: scrolls by one", 3, 3, 1, []string{"a", "b", "c"}},
		{"cursor at the end", 5, 3, 3, []string{"c", "d", "e"}},
		{"no rows to draw", 5, 0, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p.cursor = c.cursor
			if got := p.top(c.rows); got != c.top {
				t.Errorf("top(%d) = %d, want %d", c.rows, got, c.top)
			}
			got := p.window(c.rows)
			if strings.Join(got, ",") != strings.Join(c.window, ",") {
				t.Errorf("window(%d) = %v, want %v", c.rows, got, c.window)
			}
			// the cursor is always inside the window it just chose
			if len(got) > 0 {
				if i := p.cursor - p.top(c.rows); i < 0 || i >= len(got) {
					t.Errorf("cursor %d falls outside the window %v", p.cursor, got)
				}
			}
		})
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
