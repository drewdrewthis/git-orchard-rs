package main

// M-1..M-9: go straight to the nth card on screen.
//
// outer.conf forwards the chord to this pane from wherever the keyboard is
// (`bind -n M-1 send-keys -t 0.0 M-1`, the same trick M-Up/M-Down use), so the
// reach works with focus on the inner client — which, after a click hands
// focus back, is where it normally is.

// maxJump is how many cards the keyboard reaches directly. M-0 is not a tenth
// slot: it has no ordinal to mark it with, and it stays the inner session's.
const maxJump = 9

// jumpOrdinals mark those cards, in the rail column of each card's first line.
// Superscripts because that column is exactly one cell wide: a "1." marker
// would widen the gutter for nine cards and shift every card in the list.
var jumpOrdinals = [maxJump]string{"¹", "²", "³", "⁴", "⁵", "⁶", "⁷", "⁸", "⁹"}

// jumpOrdinal is the marker for the nth VISIBLE card (0-based), empty past the
// ninth — the rest of the list is reached by scrolling, and marking cards no
// chord can reach would be a lie.
func jumpOrdinal(n int) string {
	if n < 0 || n >= maxJump {
		return ""
	}
	return jumpOrdinals[n]
}

// jumpDigit reads the digit out of a jump chord, ok=false for every other
// rune. 1-based, as the ordinals are: M-1 is the first card, not the second.
func jumpDigit(r rune) (int, bool) {
	if r < '1' || r > '9' {
		return 0, false
	}
	return int(r - '0'), true
}

// jumpTo selects the nth VISIBLE card, 1-based, and hands focus back: M-3 is a
// finished "take me there" gesture, exactly like a click, not a browse step.
//
// Past the end of the visible list it does nothing at all. With a filter on
// that is the whole point — M-9 over four matches must not walk off into the
// rows the filter is hiding and attach one of them.
func (m *model) jumpTo(n int) {
	vis := m.visibleRows()
	if n < 1 || n > len(vis) {
		return
	}
	m.selectRow(vis[n-1], true)
}
