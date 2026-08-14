package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// ghURL builds a GitHub web URL only when the repo slug carries an owner —
// bare local slugs (".claude", "langwatch") have no canonical web home.
func TestGHURL(t *testing.T) {
	cases := []struct {
		name, slug, want string
	}{
		{"owner/name slug", "drewdrewthis/orchardist", "https://github.com/drewdrewthis/orchardist"},
		{"bare slug has no URL", ".claude", ""},
		{"empty", "", ""},
		{"deep path is not a slug", "a/b/c", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ghURL(c.slug); got != c.want {
				t.Errorf("ghURL(%q) = %q, want %q", c.slug, got, c.want)
			}
		})
	}
}

// gitBoxItems shows whatever git info the row has — each line paired with the
// payload a click copies. Links copy full URLs when the slug supports them.
func TestGitBoxItems(t *testing.T) {
	branch := "fix/thing"
	two := 2
	full := row{
		branch:     branch,
		repo:       "drewdrewthis/orchardist",
		ahead:      &two,
		cwd:        "/ws/git-orchard-rs",
		issueNum:   727,
		issueTitle: "tick idle backoff",
		pr:         &prInfo{Number: 9, State: "MERGED"},
	}
	items := gitBoxItems(full)
	wants := []boxItem{
		{text: "🌿 fix/thing ↑2", copy: "fix/thing"},
		{text: "📁 git-orchard-rs", copy: "/ws/git-orchard-rs"},
		{text: "issue#727 tick idle backoff", copy: "https://github.com/drewdrewthis/orchardist/issues/727"},
		{text: "pr#9 (merged)", copy: "https://github.com/drewdrewthis/orchardist/pull/9"},
	}
	if len(items) != len(wants) {
		t.Fatalf("got %d items %v, want %d", len(items), items, len(wants))
	}
	for i, w := range wants {
		if items[i] != w {
			t.Errorf("item[%d] = %+v, want %+v", i, items[i], w)
		}
	}

	// no git info at all -> no items -> no box
	if got := gitBoxItems(row{}); len(got) != 0 {
		t.Errorf("empty row: got %v, want none", got)
	}

	// bare slug: issue/pr lines still show, but copy the #ref (no URL to give)
	bare := row{repo: "langwatch", issueNum: 5, pr: &prInfo{Number: 7, State: "OPEN"}}
	items = gitBoxItems(bare)
	if len(items) != 2 || items[0].copy != "#5" || items[1].copy != "#7" {
		t.Errorf("bare slug: got %+v, want copies #5/#7", items)
	}
}

// The box renders items+2 border lines, and no line ever exceeds the given
// width — the same invariant line() holds, because an overshooting line
// soft-wraps and skews the lineToRow/lineToCopy mouse maps.
func TestGitBoxRenderWidth(t *testing.T) {
	items := []boxItem{
		{text: "🌿 a-very-long-branch-name-that-truncates ↑2", copy: "x"},
		{text: "📁 git-orchard-rs", copy: "/p"},
		{text: "issue#727 tick idle backoff with a long title", copy: "u"},
	}
	for _, w := range []int{8, 12, 20, 31, 42, 80} {
		for _, flash := range []bool{false, true} {
			lines := gitBoxRender(items, w, flash)
			if len(lines) != len(items)+2 {
				t.Fatalf("w=%d: got %d lines, want %d", w, len(lines), len(items)+2)
			}
			for i, l := range lines {
				if got := ansi.StringWidth(l); got > w {
					t.Errorf("w=%d flash=%v line %d = %q — %d cells", w, flash, i, l, got)
				}
			}
		}
	}
}

// Clicking a git-box item line returns a copy command and never moves the
// selection; border lines do neither. The box maps lineToRow -1 throughout —
// copy and select are disjoint gestures.
func TestClickOnGitBoxCopies(t *testing.T) {
	m := &model{
		width: 42,
		rows: []row{{
			session: "s1", state: "working", branch: "main",
			cwd: "/tmp/x", repo: "o/r", issueNum: 3,
		}},
		cursor: 0,
	}
	m.View() // populates lineToRow / lineToCopy

	first, border := -1, -1
	for i, cp := range m.lineToCopy {
		if cp != "" && first == -1 {
			first = i
			border = i - 1 // the box's top border sits right above the first item
		}
	}
	if first == -1 {
		t.Fatal("no copyable line rendered")
	}
	if m.lineToRow[first] != -1 {
		t.Errorf("box item line maps to row %d, want -1", m.lineToRow[first])
	}

	click := func(y int) tea.Cmd {
		_, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: y})
		return cmd
	}
	if cmd := click(first); cmd == nil {
		t.Error("click on item line: want copy cmd, got nil")
	}
	if cmd := click(border); cmd != nil {
		t.Error("click on border line: want nil cmd")
	}
	if m.cursor != 0 {
		t.Errorf("cursor moved to %d on box clicks", m.cursor)
	}
}
