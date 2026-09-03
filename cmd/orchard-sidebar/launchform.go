package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The launch modal's UI. Four focus stops, cycled with tab: the directory
// picker, the command, the session name, and the launch button.
const (
	focusPick = iota
	focusCmd
	focusName
	focusGo
	focusStops
)

type launchModel struct {
	pick       *picker
	cmd        textField
	name       textField
	nameEdited bool // once you type a name, changing directory stops rewriting it
	taken      map[string]bool
	focus      int
	w, h       int
	status     string
	launched   bool
}

// fieldWidth is the visible width of the two text fields. The modal is 80% of
// the terminal and the label gutter takes a dozen cells; past this the field
// scrolls under the cursor rather than swallowing what you type.
const fieldWidth = 48

func newLaunchModel(dir, cmd string) *launchModel {
	m := &launchModel{pick: newPicker(dir), taken: takenSessions()}
	m.cmd = newTextField(cmd, fieldWidth)
	m.name = newTextField(uniqueName(filepath.Base(m.pick.dir), m.taken), fieldWidth)
	return m
}

func (m *launchModel) Init() tea.Cmd { return nil }

// syncName re-derives the session name from the directory you are standing in,
// until you take the name over by typing in it.
func (m *launchModel) syncName() {
	if !m.nameEdited {
		m.name.set(uniqueName(filepath.Base(m.pick.dir), m.taken))
	}
}

// resolvedName is the name the launch will actually use. uniqueName resolves a
// collision by suffixing, and doing that silently at launch time created a
// session under a name the user never saw and could not find — so the form
// shows it (View) and the launch uses the same value.
func (m *launchModel) resolvedName() string {
	return uniqueName(m.name.value(), m.taken)
}

func (m *launchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

// key routes on msg.Type, never on msg.String(): bubbletea folds a burst of
// runes arriving in one read into a single KeyRunes message, so a fast repeat
// or a paste reports msg.String() == "abc" and matches no case at all — the
// form looks stuck exactly when the user is typing fastest.
func (m *launchModel) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		return m, tea.Quit // cancel: the popup closes when this process exits
	case tea.KeyTab:
		m.focus = (m.focus + 1) % focusStops
		return m, nil
	case tea.KeyShiftTab:
		m.focus = (m.focus + focusStops - 1) % focusStops
		return m, nil
	}
	if m.focus == focusPick {
		return m.pickKey(msg)
	}
	return m.fieldKey(msg)
}

// pickKey drives the directory picker. Plain typing filters, so navigation and
// the hidden-file toggle take the keys typing cannot: j/k (which therefore
// never reach the filter), the arrows, and alt+h.
func (m *launchModel) pickKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyDown:
		m.pick.move(1)
		return m, nil
	case tea.KeyUp:
		m.pick.move(-1)
		return m, nil
	case tea.KeyEnter:
		m.pick.enter()
		m.syncName()
		return m, nil
	case tea.KeyBackspace:
		if !m.pick.backspaceSearch() {
			m.pick.parent()
			m.syncName()
		}
		return m, nil
	}
	if msg.Alt {
		if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 && msg.Runes[0] == 'h' {
			m.pick.toggleHidden()
		}
		return m, nil // every other alt key belongs to the outer wrapper
	}
	// one rune at a time, so a coalesced "jjk" moves three times instead of
	// matching nothing (the same rule as the sidebar's own key handler)
	var typed []rune
	for _, r := range typedRunes(msg) {
		switch r {
		case 'j':
			m.pick.move(1)
		case 'k':
			m.pick.move(-1)
		case '.':
			// select the directory you are standing in and move on
			m.syncName()
			m.focus = focusCmd
		default:
			typed = append(typed, r)
		}
	}
	if len(typed) > 0 {
		m.pick.searchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: typed})
		return m, nil
	}
	if msg.Type != tea.KeyRunes && msg.Type != tea.KeySpace {
		m.pick.searchKey(msg) // ^U and the rest of the field's editing keys
	}
	return m, nil
}

// typedRunes is the text a key event contributes to a field. A lone space is
// reported as KeySpace rather than KeyRunes, so reading only KeyRunes silently
// dropped every space typed into a command line — which is most of them. An
// alt-modified key is a shortcut that fell through, never text.
func typedRunes(msg tea.KeyMsg) []rune {
	if msg.Alt {
		return nil
	}
	switch msg.Type {
	case tea.KeyRunes, tea.KeySpace:
		return msg.Runes
	}
	return nil
}

// fieldKey edits the command and name fields, and fires the launch. Enter walks
// forward through the remaining stops so the whole form is one hand: type,
// enter, enter, done.
func (m *launchModel) fieldKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyEnter {
		if m.focus == focusGo {
			return m, m.launch()
		}
		m.focus++
		return m, nil
	}
	if m.focus == focusGo || msg.Alt {
		return m, nil
	}
	if m.focus == focusName {
		m.nameEdited = true
		return m, m.name.key(msg)
	}
	return m, m.cmd.key(msg)
}

// launch runs the tmux side and quits on success. A failure stays on screen
// with the error: the popup closing on a failed launch would look like it had
// worked.
func (m *launchModel) launch() tea.Cmd {
	if err := launchSession(m.pick.dir, m.cmd.value(), m.resolvedName()); err != nil {
		m.status = err.Error()
		return nil
	}
	m.launched = true
	return tea.Quit
}

var (
	styModTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(neonAccent))
	styModPath  = lipgloss.NewStyle().Foreground(lipgloss.Color(paleNeon))
	styModSel   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(neonAccent))
	styModLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styModFocus = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "26", Dark: "45"})
	styModErr   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
)

func (m *launchModel) View() string {
	w := m.w
	if w <= 0 {
		w = 80
	}
	h := m.h
	if h <= 0 {
		h = 24
	}
	var b strings.Builder
	fmt.Fprintln(&b, styModTitle.Render(" Launch a session"))
	fmt.Fprintln(&b, " "+styModPath.Render(trunc(m.pick.dir, w-2)))
	fmt.Fprintln(&b, "")

	// the list gets whatever the fixed chrome doesn't need: title, path, blank,
	// filter line, two fields, the button, the hint, and a status line
	listH := h - 10
	if listH < 3 {
		listH = 3
	}
	for i, e := range m.pick.window(listH) {
		cur := m.pick.cursor - m.pick.top(listH)
		mark, sty := "  ", styModLabel
		if i == cur && m.focus == focusPick {
			mark, sty = "▌ ", styModSel
		} else if i == cur {
			mark = "▌ "
		}
		label := e + "/"
		if e == parentEntry {
			label = e
		}
		fmt.Fprintln(&b, " "+mark+sty.Render(trunc(label, w-5)))
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, " "+styModLabel.Render("search  ")+m.pick.searchView(w-12))
	fmt.Fprintln(&b, " "+m.field("command", &m.cmd, focusCmd, w))
	fmt.Fprintln(&b, " "+m.field("name   ", &m.name, focusName, w)+m.renamedHint())
	fmt.Fprintln(&b, "")
	btn := "  ⏎ launch  "
	if m.focus == focusGo {
		btn = styModFocus.Render("▶ ⏎ launch  ")
	}
	fmt.Fprintln(&b, " "+btn+styModLabel.Render("  tab next · esc cancel"))
	if m.status != "" {
		fmt.Fprintln(&b, " "+styModErr.Render(trunc(m.status, w-2)))
	}
	fmt.Fprint(&b, " "+styModLabel.Render(trunc(
		"j/k move · ⏎ open · ⌫ up · . pick this dir · ⌥h hidden", w-2)))
	return b.String()
}

// renamedHint shows the name the launch will really use when a collision has
// bumped it, so the suffix is a thing the user saw coming.
func (m *launchModel) renamedHint() string {
	got := m.resolvedName()
	if got == m.name.value() {
		return ""
	}
	return styModFocus.Render("  → " + got)
}

func (m *launchModel) field(label string, f *textField, focus, w int) string {
	if m.focus != focus {
		return styModLabel.Render(label+"  ") + styModLabel.Render(trunc(f.value(), w-14))
	}
	return styModLabel.Render(label+"  ") + styModFocus.Render(f.view(w-14))
}

// runLaunch is the `orchard-sidebar launch` entry point.
func runLaunch() int {
	dir := os.Getenv("ORCHARD_LAUNCH_DIR")
	cmd := loadLastLaunch().Cmd
	if cmd == "" {
		cmd = defaultCmd
	}
	p := tea.NewProgram(newLaunchModel(dir, cmd), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
