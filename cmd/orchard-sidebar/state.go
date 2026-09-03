package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// The sidebar's own files, all under one directory: the layout it remembers
// between runs, the last launch the modal replays, and the log it writes
// instead of shouting into its own pane (logf).

// stateFile is the path of one of them.
func stateFile(name string) string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(base, "orchard", name)
}

// sidebarState is the layout the sidebar restores on startup: the width the
// user dragged to and whether they left it collapsed. Both survive a restart
// of the sidebar, of the wrapper, and of the machine — a sidebar that reopens
// at 40 columns every morning is a sidebar the user re-drags every morning.
//
// The width is ALSO published to the outer server as its own main-pane-width
// (setWidthOption, widthOption in tmux.go), which is what the wrapper's
// resize hooks read while the sidebar is running. This file is the same fact
// made durable, and the only copy that outlives the tmux server.
type sidebarState struct {
	Width     int  `json:"width"`
	Collapsed bool `json:"collapsed"`
	// Bell is opt-in and off by default: a sidebar that starts making noise
	// on its own is a sidebar people close.
	Bell bool `json:"bell"`
	// Pinned is the ordered set of pinned session names. Order is significant
	// and preserved across restarts — it is the block's top-to-bottom order.
	Pinned []string `json:"pinned,omitempty"`
}

func sidebarStatePath() string { return stateFile("sidebar-state.json") }

// loadSidebarState never fails: a missing or corrupt file just means no
// remembered layout, and the sidebar opens at whatever width the wrapper split
// it to rather than refusing to start. A width below the readable floor is
// dropped for the same reason it is clamped on a drag — restoring an unusable
// pane is worse than forgetting one.
func loadSidebarState() sidebarState {
	var st sidebarState
	b, err := os.ReadFile(sidebarStatePath())
	if err != nil {
		return sidebarState{}
	}
	if json.Unmarshal(b, &st) != nil {
		return sidebarState{}
	}
	if st.Width != 0 && st.Width < minWidth {
		st.Width = 0
	}
	return st
}

// saveSidebarState is a var so tests can observe layout writes without a
// filesystem. Called on every drag and every collapse toggle: both are single
// user gestures, and the file is a few dozen bytes.
var saveSidebarState = func(st sidebarState) error {
	p := sidebarStatePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// restoreLayout re-applies a remembered layout to the pane, SYNCHRONOUSLY,
// before bubbletea ever reads the pane's size. Asynchronously it would race
// the first WindowSizeMsg: the sidebar would see the pre-restore width, take
// it for the wrapper's own split, and publish it back over the width it was
// restoring.
//
// Nothing remembered is not an error — an unset main-pane-width is exactly
// what makes outer.conf fall back to its own default width.
func restoreLayout(st sidebarState) {
	w, collapsed, ok := restorePane(st)
	if !ok || env.self == "" {
		return
	}
	if st.Width != 0 {
		runOuter(setPaneOptionArgs(env.self, widthOption, strconv.Itoa(st.Width))...)
	}
	applyCollapsed(env.self, collapsed, w)
}

// restorePane resolves a remembered layout to the pane width to apply and the
// collapsed flag to publish alongside it. ok is false when there is nothing to
// restore.
func restorePane(st sidebarState) (width int, collapsed bool, ok bool) {
	switch {
	case st.Collapsed:
		return collapsedWidth, true, true
	case st.Width >= minWidth:
		return st.Width, false, true
	}
	return 0, false, false
}
