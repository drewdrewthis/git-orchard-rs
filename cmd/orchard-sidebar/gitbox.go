package main

// The git box: a bordered panel under the card list showing the selected
// session's git state — the same facts the card abbreviates, but laid out to
// be *taken*: every line is click-to-copy (branch name, worktree path, issue
// and PR URLs). Copying goes through pbcopy rather than OSC 52: this is a
// macOS-local tool, pbcopy is unconditional, and OSC 52 would additionally
// depend on tmux's set-clipboard passthrough being enabled. ADR-016/018 do
// not reach clipboard writes (not a git/gh/tmux exec, not a tracked
// mutation); precedent for the sidebar owning purely-local interactions is
// the ratified direct tmux attach exception.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// stripCtl removes control characters from remote-sourced text (issue titles
// come from GitHub). An embedded newline would add a rendered line the mouse
// maps don't know about; an ESC would inject terminal escapes; a bidi
// override (Trojan-Source) would visually reorder the row on bidi-aware
// terminals; Unicode line/paragraph separators are dropped for terminals
// that treat them as breaks.
func stripCtl(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f,
			r >= 0x202a && r <= 0x202e,              // bidi embedding/override
			r >= 0x2066 && r <= 0x2069,              // bidi isolates
			r == 0x85 || r == 0x2028 || r == 0x2029: // NEL, LS, PS
			return -1
		}
		return r
	}, s)
}

// ghURL builds the repo's GitHub web URL from an "owner/name" slug. Bare
// slugs (a repo the daemon tracks by directory name only) have no canonical
// web home, so they yield "".
func ghURL(slug string) string {
	if strings.Count(slug, "/") != 1 {
		return ""
	}
	return "https://github.com/" + slug
}

// gitBoxItems folds whatever git info the row has into box lines. Missing
// facts are omitted, never placeholdered; an all-zero row yields no items
// and the box body is blank.
func gitBoxItems(r row) []boxItem {
	var items []boxItem
	if bl := branchLine(r); bl != "" {
		items = append(items, boxItem{text: bl, copy: r.branch})
	}
	// only a real cwd earns a line — dirLabel's repo-slug fallback has no
	// path to copy, and the card already shows it
	if r.cwd != "" {
		items = append(items, boxItem{text: "📁 " + dirLabel(r), copy: r.cwd})
	}
	base := ghURL(r.repo)
	if r.issueNum > 0 {
		text := issueRef(r.issueNum)
		if t := stripCtl(r.issueTitle); t != "" {
			text += " " + t
		}
		cp := fmt.Sprintf("#%d", r.issueNum)
		if base != "" {
			cp = fmt.Sprintf("%s/issues/%d", base, r.issueNum)
		}
		items = append(items, boxItem{text: text, copy: cp})
	}
	if r.pr != nil {
		cp := fmt.Sprintf("#%d", r.pr.Number)
		if base != "" {
			cp = fmt.Sprintf("%s/pull/%d", base, r.pr.Number)
		}
		items = append(items, boxItem{text: prRef(*r.pr), copy: cp})
	}
	return items
}

// gitBoxRows is how many body rows the box always draws. Four is what
// gitBoxItems can produce at most (branch, directory, issue, PR), so a fixed
// height never has to drop a fact to fit — the alternative, a 3-row box, would
// silently hide the PR line for exactly the sessions that have the most going
// on.
const gitBoxRows = 4

// gitBoxRender draws the panel as pane lines, each already carrying the
// payload a click on it copies and a row of -1 (a box line copies, it never
// selects). Returning viewLines is what keeps the footer from re-deriving
// which rendered line carries which payload from this file's layout. flash
// swaps the title for a short-lived "✓ copied" acknowledgment after a click.
func gitBoxRender(items []boxItem, width int, flash bool) []viewLine {
	title, sty := "Git", gitBoxStyle
	if flash {
		title, sty = "✓ copied", copiedStyle
	}
	// Always exactly gitBoxRows body rows, padded with empty ones. The box is
	// the bottom half of a FIXED footer: if it grew and shrank with whatever
	// the selected session happens to know, the list band above it would
	// change height on every selection and the cards would jump under the
	// cursor. Padding costs a few blank rows; a moving footer costs the user
	// their place.
	inner := boxInner(width)
	body := make([]string, gitBoxRows)
	for i := range body {
		if i >= len(items) {
			continue
		}
		if items[i].copy == "" {
			body[i] = styDim.Render(trunc(items[i].text, max(1, inner)))
			continue
		}
		// the ⧉ affordance is the promise that this line is takeable, so it
		// is right-aligned against the border on every line that has a payload
		body[i] = line(max(1, inner),
			styDim.Render(trunc(items[i].text, max(1, inner-2))), stySelHead.Render("⧉"))
	}
	lines := boxRender(title, body, width, sty)
	out := make([]viewLine, len(lines))
	for i, l := range lines {
		out[i] = viewLine{text: l, row: -1}
		// body line i sits one row below the title border, so an item's
		// payload rides the line that draws it and nothing has to re-derive
		// the offset
		if bi := i - 1; bi >= 0 && bi < len(items) {
			out[i].copy = items[bi].copy
		}
	}
	return out
}

// copiedMsg reports the pbcopy result; success flips the box title to
// "✓ copied" until copiedUntil passes (the animation tick repaints it away).
type copiedMsg struct{ err error }

func copyCmd(text string) tea.Cmd {
	return func() tea.Msg {
		// bounded: a stalled pbcopy would otherwise pin this goroutine (and
		// every later click's) forever
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "pbcopy")
		c.Stdin = strings.NewReader(text)
		return copiedMsg{err: c.Run()}
	}
}
