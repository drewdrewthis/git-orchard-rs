package main

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// line() must never exceed width, even at pathological widths where the
// 1-cell floors on avail/pad would otherwise overshoot (review finding on
// PR #725: line(3, "sessionname", "12m") rendered 5 cells and soft-wrapped,
// skewing the lineToRow mouse mapping).
func TestLineNeverExceedsWidth(t *testing.T) {
	cases := []struct {
		width       int
		left, right string
	}{
		{3, "sessionname", "12m"},
		{1, "sessionname", "12m"},
		{2, "x", "longright"},
		{5, "abc", "12m"},
		{10, "⏎ jump · j/k · q", "PR#725"},
		{42, "some-session-name", "3h"},
		{42, "left-only", ""},
	}
	for _, c := range cases {
		got := line(c.width, c.left, c.right)
		if w := ansi.StringWidth(got); w > c.width {
			t.Errorf("line(%d, %q, %q) = %q — %d cells, exceeds width", c.width, c.left, c.right, got, w)
		}
	}
}
