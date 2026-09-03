package main

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// The launch picker's hidden-directory control. Hidden dirs are pruned during
// the walk (dirwalk.go), not the search, so revealing them needs a fresh walk —
// which is why the mode lives here and re-walks are gated on effectiveHidden
// changing, rather than filtering happening per keystroke.

// hiddenMode is the three-state control. The default (auto) follows the query:
// a dot-prefixed path segment is an unambiguous signal the user is naming a
// dotdir, so the walk includes hidden dirs for that query without a toggle.
// ⌥h cycles a manual override that wins in either direction.
type hiddenMode int

const (
	hiddenAuto hiddenMode = iota // follow the query (queryWantsHidden)
	hiddenOn                     // always include hidden dirs
	hiddenOff                    // never include hidden dirs
)

// next cycles auto → on → off → auto, so ⌥h is an override in both directions.
func (m hiddenMode) next() hiddenMode { return (m + 1) % 3 }

// label is the mode shown in the footer state, e.g. "hidden: auto".
func (m hiddenMode) label() string {
	switch m {
	case hiddenOn:
		return "on"
	case hiddenOff:
		return "off"
	default:
		return "auto"
	}
}

// effectiveHidden resolves whether the walk serving query should include hidden
// directories: a manual override wins outright, and auto defers to the query.
func effectiveHidden(mode hiddenMode, query string) bool {
	switch mode {
	case hiddenOn:
		return true
	case hiddenOff:
		return false
	default:
		return queryWantsHidden(query)
	}
}

// queryWantsHidden reports whether any path segment of the query begins with a
// dot — the user naming a hidden directory (".claude", "~/.config/nvim",
// "proj/.github"). A leading "~" is not a segment, and bare "." / ".." are
// path noise, not a dotdir name.
func queryWantsHidden(query string) bool {
	q := strings.TrimPrefix(strings.TrimSpace(query), "~")
	for _, seg := range strings.Split(q, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." && seg != ".." {
			return true
		}
	}
	return false
}

// activeCands is the cached candidate set for the query's effective hidden
// state, or nil when that state has not been walked yet.
func (p *picker) activeCands() []string {
	if effectiveHidden(p.mode, p.search.value()) {
		return p.hiddenCands
	}
	return p.plainCands
}

// toggleHidden cycles the hidden-directory mode (auto → on → off) and re-walks
// if the new mode changes the effective hidden state and that state is not
// already cached. ⌥h is thus an override in both directions.
func (p *picker) toggleHidden() tea.Cmd {
	p.mode = p.mode.next()
	return p.syncHidden()
}

// syncHidden re-walks when the query's effective hidden state has no cached tree
// yet, so a flip into dot-mode surfaces hidden dirs without blocking the UI. It
// returns nil when the needed set is already cached or a matching walk is
// already in flight — a flip back to a state already walked pays no filesystem
// cost. It rebuilds so the visible list reflects the (possibly cached) set now.
func (p *picker) syncHidden() tea.Cmd {
	want := effectiveHidden(p.mode, p.search.value())
	p.rebuild()
	if p.activeCands() != nil {
		return nil
	}
	if p.walking && p.cfg.showHidden == want {
		return nil // the walk we need is already running
	}
	p.cfg.showHidden = want
	p.walking = true
	p.walkGen++
	return p.walkCmd()
}
