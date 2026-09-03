package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
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
	m := &launchModel{pick: newPicker(dir, knownCwds()), taken: takenSessions()}
	m.cmd = newTextField(cmd, fieldWidth)
	m.name = newTextField(uniqueName(filepath.Base(m.pick.dir()), m.taken), fieldWidth)
	return m
}

// Init kicks off the candidate walk off the update loop, plus the spinner tick
// that animates while it runs.
func (m *launchModel) Init() tea.Cmd { return tea.Batch(m.pick.walkCmd(), m.pick.spin.Tick) }

// walkAndTick adds a spinner tick to a re-walk command only when the picker
// was not already walking: the tick loop dies once walking goes false (the
// spinner.TickMsg case below), so a widen/toggle mid-walk must not start a
// second tick loop racing the one already alive.
func (m *launchModel) walkAndTick(cmd tea.Cmd, wasWalking bool) tea.Cmd {
	if wasWalking {
		return cmd
	}
	return tea.Batch(cmd, m.pick.spin.Tick)
}

// syncName re-derives the session name from the highlighted directory, until
// you take the name over by typing in it.
func (m *launchModel) syncName() {
	if !m.nameEdited {
		m.name.set(uniqueName(filepath.Base(m.pick.dir()), m.taken))
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
	case walkDoneMsg:
		m.pick.setCands(msg.gen, msg.hidden, msg.cands)
		return m, nil
	case spinner.TickMsg:
		if !m.pick.walking {
			return m, nil // the walk landed; let the tick loop die
		}
		var cmd tea.Cmd
		m.pick.spin, cmd = m.pick.spin.Update(msg)
		return m, cmd
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

// pickKey drives the directory picker. Every printable key — j and k included —
// goes to the fuzzy query, because a directory name can contain any of them;
// navigation therefore takes the keys typing cannot: the arrows and ^n/^p.
// Enter picks the highlighted directory, alt+h toggles hidden, and backspace on
// an empty query widens the roots.
func (m *launchModel) pickKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyDown, tea.KeyCtrlN:
		m.pick.move(1)
		m.syncName()
		return m, nil
	case tea.KeyUp, tea.KeyCtrlP:
		m.pick.move(-1)
		m.syncName()
		return m, nil
	case tea.KeyEnter:
		// pick the highlighted directory and move on to the command
		m.syncName()
		m.focus = focusCmd
		return m, nil
	case tea.KeyBackspace:
		wasWalking := m.pick.walking
		deleted, cmd := m.pick.backspaceSearch()
		if !deleted {
			cmd = m.pick.widen() // empty query: backspace widens the roots
		}
		m.syncName()
		if cmd != nil {
			return m, m.walkAndTick(cmd, wasWalking)
		}
		return m, nil
	}
	if msg.Alt {
		if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 && msg.Runes[0] == 'h' {
			wasWalking := m.pick.walking
			return m, m.walkAndTick(m.pick.toggleHidden(), wasWalking)
		}
		return m, nil // every other alt key belongs to the outer wrapper
	}
	wasWalking := m.pick.walking
	var cmd tea.Cmd
	if typed := typedRunes(msg); len(typed) > 0 {
		cmd = m.pick.searchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: typed})
	} else if msg.Type != tea.KeyRunes && msg.Type != tea.KeySpace {
		cmd = m.pick.searchKey(msg) // ^U and the rest of the field's editing keys
	}
	m.syncName()
	if cmd != nil { // a query crossing the dot-mode boundary re-walks
		return m, m.walkAndTick(cmd, wasWalking)
	}
	return m, nil
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
	if err := launchSession(m.pick.dir(), m.cmd.value(), m.resolvedName()); err != nil {
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
	styModMatch = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color(neonAccent))
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
	dispDir, _ := abbrevHome(m.pick.dir(), nil)
	fmt.Fprintln(&b, styModTitle.Render(" Launch a session"))
	fmt.Fprintln(&b, " "+styModPath.Render(trunc(dispDir, w-2)))
	fmt.Fprintln(&b, "")

	// the list gets whatever the fixed chrome doesn't need: title, path, blank,
	// filter line, two fields, the button, the hint, and a status line
	listH := h - 10
	if listH < 3 {
		listH = 3
	}
	top := m.pick.top(listH)
	for i, mt := range m.pick.window(listH) {
		mark, rowSty := "  ", styModLabel
		if top+i == m.pick.cursor && m.focus == focusPick {
			mark, rowSty = styModSel.Render("▌ "), styModSel
		} else if top+i == m.pick.cursor {
			mark = "▌ "
		}
		fmt.Fprintln(&b, " "+mark+renderMatch(mt, rowSty, styModMatch, w-5))
	}
	if m.pick.walking {
		fmt.Fprintln(&b, " "+m.pick.spin.View()+styModLabel.Render(" scanning directories…"))
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
		"type to search · ↑↓ move · ⏎ pick · ⌫ widen · ⌥h hidden: "+m.pick.mode.label(), w-2)))
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
