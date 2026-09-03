package main

import (
	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// textField is the one text input this program has: the rename box, the launch
// modal's command and name fields, and the directory filter are all this.
//
// It wraps bubbles/textinput rather than reimplementing it. The three
// hand-rolled versions this replaces each knew a different subset of
// backspace / ^U / cursor movement, none knew word motions or ^K, and each
// dropped a coalesced keystroke burst differently — bubbletea folds a fast
// repeat or a paste into ONE KeyRunes message, and textinput inserts every
// rune of it.
type textField struct{ ti textinput.Model }

// newTextField opens a focused field on value, with the cursor at the end so
// the usual edit is a suffix rather than a retype. width is the visible cell
// budget: past it the field scrolls horizontally under the cursor instead of
// truncating what you are typing off the right-hand side.
func newTextField(value string, width int) textField {
	ti := textinput.New()
	ti.Prompt = ""
	// static, not blinking: a blink needs a tea.Cmd threaded back through
	// every caller for a cursor that would fight the 250ms animation tick, and
	// a still cursor renders the same in a capture-pane probe as on screen
	ti.Cursor.SetMode(cursor.CursorStatic)
	ti.SetValue(value)
	ti.CursorEnd()
	ti.Focus()
	ti.Width = max(1, width)
	return textField{ti: ti}
}

// key hands one key event to the field. Callers intercept the keys that are
// theirs (Esc, Enter, Tab) FIRST — everything that reaches here is text or an
// editing key.
func (f *textField) key(msg tea.KeyMsg) tea.Cmd {
	var cmd tea.Cmd
	f.ti, cmd = f.ti.Update(msg)
	return cmd
}

func (f textField) value() string { return f.ti.Value() }

func (f *textField) set(s string) {
	f.ti.SetValue(s)
	f.ti.CursorEnd()
}

// view renders the field at width cells, cursor included. The caller clamps
// again (every line in this pane is clamped) — this only keeps the field's own
// horizontal viewport in step with the space it was actually given.
func (f *textField) view(width int) string {
	f.ti.Width = max(1, width)
	return f.ti.View()
}

// placeholder is the dim text shown while the field is empty.
func (f *textField) placeholder(s string) {
	f.ti.Placeholder = s
	f.ti.PlaceholderStyle = styDim
}
