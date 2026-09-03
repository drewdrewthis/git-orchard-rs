package main

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// The update indicator: a header glyph when the cached update check
// (internal/release) names a release newer than this binary's own version.
//
// The sidebar never touches the network itself (RULES T1) — `orchard shell`
// writes the cache on its own schedule; this only ever reads the file it
// left behind, on the same self-scheduling fetch/tick idiom the fast and
// slow lanes use (see fastDataMsg/fastTickMsg in model.go).

// updateCheckEvery is how often the cache is re-read. The cache itself only
// changes once a day (release.CheckTTL) or on the next `orchard shell`
// startup; this is about noticing that, not about polling a server.
const updateCheckEvery = 10 * time.Minute

type updateCheckMsg struct {
	check release.Check
	err   error
}

type updateTickMsg struct{}

// fetchUpdateCheck reads the cache file. release.LoadCheck never errors — a
// missing, corrupt, or stale-format file all resolve to a zero Check, which
// updateAvailable reads as "nothing to show". err is set only when the state
// directory itself cannot be resolved, which is the one thing here worth a
// log line.
func fetchUpdateCheck() tea.Msg {
	path, err := release.CheckPath()
	if err != nil {
		return updateCheckMsg{err: err}
	}
	return updateCheckMsg{check: release.LoadCheck(path)}
}

// applyUpdateCheck stores the result. A resolve failure is logged once, not
// on every ten-minute tick — an unresolvable state directory is a fact about
// the machine that a retry does not change, and the guard is what makes that
// "once" rather than "once per tick forever".
func (m *model) applyUpdateCheck(msg updateCheckMsg) {
	if msg.err != nil {
		if !m.updateLogFailed {
			logf("update check: %v", msg.err)
			m.updateLogFailed = true
		}
		return
	}
	m.updateCheck = msg.check
}

// updateAvailable reports whether the cached check names a release newer
// than this binary. A dev build has no semver to compare (version is
// release.DevVersion), so it is never "upgradable" — release/semver.go would
// otherwise treat that non-semver string as older than any real version and
// show a phantom upgrade (#789). The header still labels dev builds, via
// updateHint's build-ident branch, just without this click-to-upgrade path.
func (m *model) updateAvailable() bool {
	if version == release.DevVersion {
		return false
	}
	return release.IsNewer(m.updateCheck.Latest, version)
}

// openUpdateOverlay shows the detail a click on the header glyph asks for.
func (m *model) openUpdateOverlay() { m.updateOpen = true }

// closeUpdateOverlay dismisses it. Called on any keypress while it is open
// (selection.go) — there is nothing in it to navigate, so there is nothing a
// key could do besides close it.
func (m *model) closeUpdateOverlay() { m.updateOpen = false }
