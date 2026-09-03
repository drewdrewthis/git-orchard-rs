package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// The launch modal's recent-directory memory. It lives in its own state file
// next to the last-launch file — kept out of lastLaunch so the two evolve
// independently — and seeds the picker's empty-query list.

// maxRecents caps how many launch directories the recents file remembers.
const maxRecents = 8

// recentsPath is the recents file, in the same state directory as the
// last-launch file (lastLaunchPath), so both share one location.
func recentsPath() string { return stateFile("sidebar-recents.json") }

// loadRecents reads the newest-first launch directories, or nil when the file
// is missing or corrupt — an absent memory, never an error.
func loadRecents() []string {
	b, err := os.ReadFile(recentsPath())
	if err != nil {
		return nil
	}
	var recents []string
	if json.Unmarshal(b, &recents) != nil {
		return nil
	}
	return recents
}

func saveRecents(recents []string) error {
	p := recentsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(recents)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// rememberRecent moves dir to the front of the recents, best-effort: a launch
// that cannot write its memory still succeeded, so the error is dropped.
func rememberRecent(dir string) {
	_ = saveRecents(addRecent(loadRecents(), dir, maxRecents))
}

// addRecent prepends dir, drops an earlier sighting of it, and caps the list —
// a most-recently-used order the empty-query picker reads.
func addRecent(recents []string, dir string, n int) []string {
	out := []string{dir}
	for _, r := range recents {
		if r != dir {
			out = append(out, r)
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// existingRecents is the picker's empty-query head: the remembered launch
// directories that still exist, newest first. With no recents file yet, it
// falls back to the single prior launch dir the last-launch file records, so a
// pre-recents install still seeds one.
func existingRecents() []string {
	rec := loadRecents()
	if len(rec) == 0 {
		if d := loadLastLaunch().Dir; d != "" {
			rec = []string{d}
		}
	}
	var out []string
	for _, d := range rec {
		if dirExists(d) {
			out = append(out, d)
		}
	}
	return out
}

// knownCwds asks the inner tmux for every pane's current directory — the walk's
// "known session cwds", whose shared parent becomes a root. A failure (daemon
// down) yields nothing, and the walk falls back to $HOME.
var knownCwds = func() []string {
	out, err := env.innerCmd("list-panes", "-a", "-F", "#{pane_current_path}").Output()
	if err != nil {
		return nil
	}
	var cwds []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			cwds = append(cwds, l)
		}
	}
	return cwds
}
