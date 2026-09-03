package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Cell arithmetic. Every string the pane draws passes through here, because
// the mouse maps assume one rendered line per pane row: a line that overshoots
// the pane width soft-wraps, and every click below it then reports the wrong
// row. ANSI- and wide-rune-aware throughout — the styles are inline escapes
// and the states are emoji, so len() is never the width.

// trunc clips to n terminal cells, first line only.
func trunc(s string, n int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	if n < 1 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

// padTo extends s to exactly n terminal cells.
func padTo(s string, n int) string {
	s = trunc(s, n)
	return s + strings.Repeat(" ", max(0, n-ansi.StringWidth(s)))
}

// line lays out left + right-aligned right within width cells, truncating
// left (never right) when they don't both fit.
func line(width int, left, right string) string {
	rw := ansi.StringWidth(right)
	avail := width - rw
	if rw > 0 {
		avail-- // one-cell gap
	}
	left = trunc(left, max(1, avail))
	pad := width - ansi.StringWidth(left) - rw
	if pad < 1 {
		pad = 1
	}
	if right == "" {
		return left
	}
	// final clamp: at pathological widths the 1-cell floors on avail/pad can
	// still overshoot; a soft-wrapped line would skew the lineToRow map
	return trunc(left+strings.Repeat(" ", pad)+right, width)
}

// age is the short relative time the cards carry in their right margin.
func age(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t).Round(time.Minute)
	if d < time.Minute {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// cellWidth is how many terminal cells s occupies, ANSI escapes excluded. The
// header lays its two halves out against each other, and every string it
// measures is already styled.
func cellWidth(s string) int { return ansi.StringWidth(s) }
