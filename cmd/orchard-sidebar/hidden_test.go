package main

import "testing"

// AC1: queryWantsHidden fires on a dot-prefixed path segment anywhere in the
// query, and only there — bare "." / ".." and a mid-name dot are not dotdirs.
func TestQueryWantsHidden(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{".claude", true},
		{"~/.config/nvim", true},
		{"proj/.github", true},
		{".c", true},
		{"claude", false},
		{"src/nvim", false},
		{"a.b", false}, // a mid-name dot is not a dotdir
		{"", false},
		{".", false},     // bare cwd, not a name
		{"..", false},    // bare parent, not a name
		{"./foo", false}, // leading "./" is cwd noise
		{"~", false},
	}
	for _, c := range cases {
		if got := queryWantsHidden(c.query); got != c.want {
			t.Errorf("queryWantsHidden(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

// AC2: effectiveHidden resolves the manual override first, and auto defers to
// the query.
func TestEffectiveHidden(t *testing.T) {
	cases := []struct {
		mode  hiddenMode
		query string
		want  bool
	}{
		{hiddenAuto, ".claude", true},
		{hiddenAuto, "claude", false},
		{hiddenOn, "claude", true},    // override forces hidden for a non-dot query
		{hiddenOff, ".claude", false}, // override suppresses hidden for a dot query
	}
	for _, c := range cases {
		if got := effectiveHidden(c.mode, c.query); got != c.want {
			t.Errorf("effectiveHidden(%v, %q) = %v, want %v", c.mode, c.query, got, c.want)
		}
	}
}

// The footer state string names the active mode.
func TestHiddenModeLabel(t *testing.T) {
	for mode, want := range map[hiddenMode]string{hiddenAuto: "auto", hiddenOn: "on", hiddenOff: "off"} {
		if got := mode.label(); got != want {
			t.Errorf("mode %d label = %q, want %q", mode, got, want)
		}
	}
}
