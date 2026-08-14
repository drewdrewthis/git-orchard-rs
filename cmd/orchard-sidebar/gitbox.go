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
	"github.com/charmbracelet/x/ansi"
)

// boxItem is one line of the git box: what it says, and what clicking it
// puts on the clipboard.
type boxItem struct {
	text string
	copy string
}

// boxLine is one *rendered* line, carrying its own click-to-copy payload
// ("" = border/decoration). Returning the pairing keeps the caller from
// reconstructing it with index arithmetic against this file's layout.
type boxLine struct {
	text string
	copy string
}

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
// and the box doesn't render.
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

// gitBoxRender draws the bordered panel: a title border, one line per item
// with a right-aligned ⧉ copy affordance, a closing border. Every line is
// clamped to width cells (the line()/lineToRow invariant — an overshooting
// line soft-wraps and skews the mouse maps). flash swaps the title for a
// short-lived "✓ copied" acknowledgment after a click.
func gitBoxRender(items []boxItem, width int, flash bool) []boxLine {
	inner := width - 5 // " │ " + content + " │"
	title := "─ Git "
	titleSty := styDim
	if flash {
		title = "─ ✓ copied "
		titleSty = stySelBody
	}
	// pad with exactly the dashes that fit — trunc would end the border in "…"
	tw := ansi.StringWidth(title)
	mid := title + strings.Repeat("─", max(0, width-3-tw))
	if tw > width-3 {
		mid = strings.Repeat("─", max(0, width-3))
	}
	lines := []boxLine{{text: trunc(" "+styDim.Render("╭")+titleSty.Render(mid)+styDim.Render("╮"), width)}}
	for _, it := range items {
		body := line(max(1, inner), styDim.Render(trunc(it.text, max(1, inner-2))), stySelHead.Render("⧉"))
		pipe := styDim.Render("│")
		lines = append(lines, boxLine{text: trunc(" "+pipe+" "+body+" "+pipe, width), copy: it.copy})
	}
	bot := " ╰" + strings.Repeat("─", max(0, width-3)) + "╯"
	lines = append(lines, boxLine{text: trunc(styDim.Render(bot), width)})
	return lines
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
