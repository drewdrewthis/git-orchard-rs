package main

import (
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// sidebarBinary is the helper orchard shell puts in pane 0.0.
const sidebarBinary = "orchard-sidebar"

// fallbackCols/fallbackRows size a detached outer session when there is no
// terminal to measure — the case verify.sh drives, with stdin and stdout
// redirected away from a tty.
const (
	fallbackCols = 160
	fallbackRows = 45
)

// selfPath returns this executable's path, or "" when it cannot be resolved.
func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// resolveSidebar finds the sidebar binary, preferring the one installed
// beside this one.
//
// Sibling-first is ADR-013's own resolution rule, and it is the point: a bare
// `orchard-sidebar` re-resolves against the pane shell's PATH at exec time and
// can silently pick up a stale build instead of the one this launch meant.
// Returns "" when neither lookup finds anything, which is the caller's cue to
// fall back to the watch(1) placeholder.
func resolveSidebar() string {
	return resolveBinary(selfPath(), sidebarBinary)
}

// resolveBinary finds a suite binary by name, preferring the one installed
// beside self (ADR-013's sibling-first rule — see resolveSidebar). Returns ""
// when neither lookup finds anything.
func resolveBinary(self, name string) string {
	if sibling := binaryNextTo(self, name); sibling != "" {
		return sibling
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	return ""
}

// sidebarNextTo returns the sidebar installed alongside self, or "".
func sidebarNextTo(self string) string {
	return binaryNextTo(self, sidebarBinary)
}

// binaryNextTo returns the named binary installed alongside self, or "".
func binaryNextTo(self, name string) string {
	if self == "" {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(self), name)
	if isExecutable(candidate) {
		return candidate
	}
	return ""
}

func isExecutable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

// termSize returns the controlling terminal's size, falling back to a fixed
// baseline when there is no tty. The size only sets the DETACHED session's
// geometry: a real client resizes the window on attach, and outer.conf's
// resize hooks re-pin the sidebar afterwards.
func termSize() (cols, rows int) {
	for _, f := range []*os.File{os.Stdout, os.Stderr, os.Stdin} {
		ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
		if err == nil && ws.Col > 0 && ws.Row > 0 {
			return int(ws.Col), int(ws.Row)
		}
	}
	return fallbackCols, fallbackRows
}
