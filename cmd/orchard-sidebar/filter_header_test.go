package main

import (
	"runtime/debug"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// A dev build labels the header with its ident (dev@<rev>*), and the open
// filter's query field shares that same row. On a defaultWidth pane the two
// cannot both fit, so the header drops the ident for the frame rather than
// scroll the query out of view — the bug was typing "payments" and seeing the
// front-clipped "/yments" (#801). The ident returns when the pane is wide
// enough to seat both, or when the filter is off entirely.
func TestFilterHeaderYieldsToDevIdent(t *testing.T) {
	prevVersion := version
	prevBuild := readBuildInfo
	t.Cleanup(func() { version = prevVersion; readBuildInfo = prevBuild })
	version = release.DevVersion
	readBuildInfo = buildInfoWith(
		debug.BuildSetting{Key: "vcs.revision", Value: "abc1234def567"},
		debug.BuildSetting{Key: "vcs.modified", Value: "true"})

	for _, tc := range []struct {
		name       string
		query      string // "" leaves the filter off
		w          int
		wantSub    []string
		wantAbsent []string
	}{
		{"filter open, defaultWidth: field wins, ident dropped", "payments", defaultWidth,
			[]string{"/payments"}, []string{"dev@abc1234*", "/yments"}},
		{"filter open, wide pane: both fit", "payments", 80,
			[]string{"dev@abc1234*", "/payments"}, nil},
		{"filter off, defaultWidth: ident shows", "", defaultWidth,
			[]string{"dev@abc1234*"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fakeModel(t, 30, 42, 40)
			if tc.query != "" {
				m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
				typeIntoView(m, tc.query)
			}
			lines, _ := m.header(tc.w)
			head := ansi.Strip(lines[0].text)
			for _, s := range tc.wantSub {
				if !strings.Contains(head, s) {
					t.Errorf("header %q is missing %q", head, s)
				}
			}
			for _, s := range tc.wantAbsent {
				if strings.Contains(head, s) {
					t.Errorf("header %q must not contain %q", head, s)
				}
			}
		})
	}
}
