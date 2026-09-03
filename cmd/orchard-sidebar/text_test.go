package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Cell arithmetic and the width thresholds that switch the card layout: every
// rendered line is one pane row, and a line that overshoots soft-wraps and
// skews every mouse map below it.

// trunc must clip to the interior width even when a narrow pane drives
// iw = w-3 to zero or below, since View() feeds it unchecked.
func TestTruncAtNarrowInteriorWidth(t *testing.T) {
	for _, w := range []int{0, 1, 2, 3, 4} {
		iw := w - 3
		if got := trunc("Needs input", iw); len([]rune(got)) > max(0, iw) {
			t.Errorf("trunc(_, %d) = %q, longer than %d", iw, got, iw)
		}
	}
}

// Below minWidth the card layout degrades to name-only rather than shredding
// sub-lines; at or above it the full card renders.
func TestCompactModeThreshold(t *testing.T) {
	mk := func(w int) *model {
		return &model{width: w, rows: []row{{
			session: "orchard-sidebar", state: "working", mission: "a mission",
			repo: "git-orchard-rs", issueNum: 719,
		}}}
	}
	narrow := viewOf(mk(minWidth - 1))
	wide := viewOf(mk(minWidth))
	if strings.Contains(narrow, "a mission") || strings.Contains(narrow, "issue#719") {
		t.Errorf("compact view kept detail lines:\n%s", narrow)
	}
	if !strings.Contains(narrow, "orchard-sidebar") {
		t.Errorf("compact view dropped the name:\n%s", narrow)
	}
	if !strings.Contains(wide, "a mission") || !strings.Contains(wide, "issue#719") {
		t.Errorf("full view missing detail lines:\n%s", wide)
	}
	for _, v := range []struct {
		w    int
		body string
	}{{minWidth - 1, narrow}, {minWidth, wide}} {
		for _, l := range strings.Split(v.body, "\n") {
			if ansi.StringWidth(l) > v.w {
				t.Errorf("width %d: line %q is %d cells", v.w, l, ansi.StringWidth(l))
			}
		}
	}
}

// scripts/sidebar-open.sh clamps the pane width to the same floor as minWidth.
// The two live in different languages, so pin them together here rather than
// trusting the cross-reference comments to stay honest.
func TestLauncherWidthFloorMatchesMinWidth(t *testing.T) {
	src, err := os.ReadFile("../../scripts/sidebar-open.sh")
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	re := regexp.MustCompile(`\[ "\$width" -lt (\d+) \] && width=(\d+)`)
	mm := re.FindSubmatch(src)
	if mm == nil {
		t.Fatalf("width clamp not found in sidebar-open.sh — did the clamp move?")
	}
	for _, g := range mm[1:] {
		got, err := strconv.Atoi(string(g))
		if err != nil {
			t.Fatal(err)
		}
		if got != minWidth {
			t.Errorf("launcher floor %d != minWidth %d", got, minWidth)
		}
	}
}

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
