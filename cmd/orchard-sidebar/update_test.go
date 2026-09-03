package main

import (
	"errors"
	"runtime/debug"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// The update indicator reads a file `orchard shell` writes on its own
// schedule (internal/release) and never touches the network itself (RULES
// T1) — these tests are about that read, the comparison it drives, and the
// glyph/overlay it shows, not about the check itself (release/check_test.go
// and release/semver_test.go already cover LoadCheck and Compare directly).

// TestUpdateAvailable: the header glyph tracks one comparison — the cached
// check's Latest against this binary's own version var, never the check's
// own Current field, which is whichever version wrote the cache and may not
// be this one.
func TestUpdateAvailable(t *testing.T) {
	prevVersion := version
	t.Cleanup(func() { version = prevVersion })

	for _, tc := range []struct {
		name    string
		version string
		latest  string
		want    bool
	}{
		{"older latest: nothing to show", "1.3.0", "1.2.3", false},
		{"equal: nothing to show", "1.2.3", "1.2.3", false},
		{"genuinely newer", "1.2.3", "1.3.0", true},
		{"dev build: no semver, never upgradable", release.DevVersion, "0.0.1", false},
		{"git-describe stamp: no semver, never upgradable", "v1.1.0-3-gabc1234-dirty", "1.1.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version = tc.version
			m := &model{updateCheck: release.Check{Latest: tc.latest}}
			if got := m.updateAvailable(); got != tc.want {
				t.Errorf("updateAvailable() = %v, want %v (version=%q latest=%q)",
					got, tc.want, tc.version, tc.latest)
			}
		})
	}
}

// buildInfoWith returns a readBuildInfo stub carrying the given VCS settings.
func buildInfoWith(settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: settings}, true
	}
}

// TestUpdateHint: what the header shows on the right. A dev build labels
// itself with its VCS revision (dev@<rev>, "*" when dirty, bare "dev" without
// a stamp) and never the upgrade glyph (#789); a release build shows the glyph
// plus a newer version, and nothing for an equal or older latest.
func TestUpdateHint(t *testing.T) {
	prevVersion := version
	prevBuild := readBuildInfo
	t.Cleanup(func() { version = prevVersion; readBuildInfo = prevBuild })

	rev := debug.BuildSetting{Key: "vcs.revision", Value: "abcdef1234567"}
	for _, tc := range []struct {
		name        string
		version     string
		latest      string
		build       func() (*debug.BuildInfo, bool)
		wantSub     string // substring the hint must contain
		wantNoGlyph bool   // and must NOT carry the upgrade glyph
	}{
		{"dev clean: revision only", release.DevVersion, "9.9.9",
			buildInfoWith(rev, debug.BuildSetting{Key: "vcs.modified", Value: "false"}),
			"dev@abcdef1", true},
		{"dev modified: trailing star", release.DevVersion, "9.9.9",
			buildInfoWith(rev, debug.BuildSetting{Key: "vcs.modified", Value: "true"}),
			"dev@abcdef1*", true},
		{"dev no build info: plain dev", release.DevVersion, "9.9.9",
			func() (*debug.BuildInfo, bool) { return nil, false }, "dev", true},
		{"dev no vcs.revision: plain dev", release.DevVersion, "9.9.9",
			buildInfoWith(debug.BuildSetting{Key: "vcs.modified", Value: "true"}), "dev", true},
		{"versioned equal latest: empty", "1.2.3", "1.2.3", nil, "", true},
		{"versioned older latest: empty", "1.2.3", "1.1.0", nil, "", true},
		{"versioned newer latest: glyph", "1.2.3", "1.3.0", nil, updateGlyph + "v1.3.0", false},
		{"git-describe stamp: no semver, dev ident", "v1.1.0-3-gabc1234-dirty", "1.1.0",
			buildInfoWith(debug.BuildSetting{Key: "vcs.revision", Value: "abcdef1234567"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"}),
			"dev@abcdef1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version = tc.version
			if tc.build != nil {
				readBuildInfo = tc.build
			}
			m := &model{updateCheck: release.Check{Latest: tc.latest}}
			got := ansi.Strip(m.updateHint())
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("updateHint() = %q, want it to contain %q", got, tc.wantSub)
			}
			if tc.wantNoGlyph && strings.Contains(got, updateGlyph) {
				t.Errorf("updateHint() = %q, want no upgrade glyph %q", got, updateGlyph)
			}
			// a dev hint is never a bare empty string — the label always shows
			if tc.version == release.DevVersion && got == "" {
				t.Errorf("dev build hint is empty, want a build ident")
			}
		})
	}
}

// A dev build shows its ident in the header, but the ident is not a click
// target: no updateZone is published, so a click where it renders opens no
// "update available" overlay (#789).
func TestDevBuildHintIsNotAClickTarget(t *testing.T) {
	prevVersion := version
	prevBuild := readBuildInfo
	t.Cleanup(func() { version = prevVersion; readBuildInfo = prevBuild })
	version = release.DevVersion
	readBuildInfo = buildInfoWith(
		debug.BuildSetting{Key: "vcs.revision", Value: "abcdef1234567"},
		debug.BuildSetting{Key: "vcs.modified", Value: "false"})

	m := fakeModel(t, 10, 80, 30) // wide: the hint is not width-suppressed
	head := ansi.Strip(strings.Split(viewOf(m), "\n")[0])
	if !strings.Contains(head, "dev@abcdef1") {
		t.Fatalf("header does not show the dev ident: %q", head)
	}
	if m.pane.updateZone.w != 0 {
		t.Fatalf("dev build published a click zone (w=%d); the ident must not be clickable",
			m.pane.updateZone.w)
	}
	// clicking where the ident renders must not open the overlay
	x := strings.Index(head, "dev@")
	mm, _ := m.Update(tea.MouseMsg{X: x, Y: 0,
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = mm.(*model)
	if m.updateOpen {
		t.Error("clicking the dev ident opened the update overlay")
	}
}

// TestFetchUpdateCheck: a missing file, a corrupt one, and a stale-shape one
// all resolve the same way LoadCheck resolves them — the zero Check, no
// error — since fetchUpdateCheck adds nothing on top of CheckPath+LoadCheck
// but the sidebar's own read of that same path.
func TestFetchUpdateCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		write     bool
		body      string
		wantCheck release.Check
	}{
		{"missing file", false, "", release.Check{}},
		{"corrupt file", true, "{not json", release.Check{}},
		{"stale-shape file", true, `{"unexpected":"shape"}`, release.Check{}},
		{"a real check", true, `{"current":"1.2.3","latest":"1.3.0"}`,
			release.Check{Current: "1.2.3", Latest: "1.3.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateHome(t)
			if tc.write {
				writeStateFile(t, release.CheckFile, tc.body)
			}
			msg := fetchUpdateCheck().(updateCheckMsg)
			if msg.err != nil {
				t.Fatalf("err = %v, want nil", msg.err)
			}
			if msg.check != tc.wantCheck {
				t.Errorf("check = %+v, want %+v", msg.check, tc.wantCheck)
			}
		})
	}
}

// A resolve failure (release.CheckPath erroring — an unresolvable state
// directory) is logged once, not on every ten-minute tick: a broken $HOME is
// a fact about the machine that a retry does not change. The guard is the
// mechanism, so it is the guard's own state this asserts — logf itself has
// no test seam (unlike emitBell) and its destination is a process-lifetime
// singleton that outlives any one test.
func TestApplyUpdateCheckLogsAResolveFailureOnce(t *testing.T) {
	stateHome(t) // sandbox logf's file wherever this test lands in the run
	m := &model{}
	m.applyUpdateCheck(updateCheckMsg{err: errors.New("boom")})
	if !m.updateLogFailed {
		t.Fatal("updateLogFailed = false after one error, want true")
	}
	m.applyUpdateCheck(updateCheckMsg{err: errors.New("boom again")})
	if !m.updateLogFailed {
		t.Error("updateLogFailed flipped back to false on a second error")
	}
}

func TestApplyUpdateCheckStoresTheCheckOnSuccess(t *testing.T) {
	m := &model{}
	check := release.Check{Current: "1.2.3", Latest: "1.3.0"}
	m.applyUpdateCheck(updateCheckMsg{check: check})
	if m.updateCheck != check {
		t.Errorf("updateCheck = %+v, want %+v", m.updateCheck, check)
	}
	if m.updateLogFailed {
		t.Error("a successful check set updateLogFailed")
	}
}

// The overlay's exact copy: what changed, and the one command that fixes it.
// The sidebar cannot run the upgrade itself (RULES T1) — only say to.
func TestUpdateBodyFormat(t *testing.T) {
	prevVersion := version
	version = "1.2.3"
	t.Cleanup(func() { version = prevVersion })

	m := &model{updateCheck: release.Check{Latest: "1.3.0"}}
	want := "update available v1.2.3 → v1.3.0 — run: orchard upgrade"
	if got := m.updateBody(200); got != want {
		t.Errorf("updateBody = %q, want %q", got, want)
	}
}

// Suppressed first on a narrowing pane, well before the badge (18) or the
// buttons (never): a version string is the widest thing the header can
// carry, so it has to be the first thing to go.
func TestUpdateHintNarrowWidthSuppression(t *testing.T) {
	prevVersion := version
	version = "1.0.0"
	t.Cleanup(func() { version = prevVersion })
	m := &model{updateCheck: release.Check{Latest: "2.0.0"}}

	for _, tc := range []struct {
		name string
		w    int
		want bool
	}{
		{"wide enough", defaultWidth, true},
		{"exactly at the floor (iw == updateMinWidth)", updateMinWidth + 3, true},
		{"one below the floor (iw == updateMinWidth-1)", updateMinWidth + 2, false},
		{"very narrow", 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, zones := m.header(tc.w)
			head := ansi.Strip(lines[0].text)
			if got := strings.Contains(head, updateGlyph); got != tc.want {
				t.Errorf("header contains %q = %v, want %v (header: %q)",
					updateGlyph, got, tc.want, head)
			}
			if got := zones.update.w > 0; got != tc.want {
				t.Errorf("zones.update.w > 0 = %v, want %v", got, tc.want)
			}
		})
	}
}

// Three columns is not room for a version number, only the fact that there
// is one — the collapsed strip carries the bare glyph, never "v2.0.0".
func TestCollapsedStripShowsBareUpdateGlyph(t *testing.T) {
	prevVersion := version
	version = "1.0.0"
	t.Cleanup(func() { version = prevVersion })

	m := &model{updateCheck: release.Check{Latest: "2.0.0"}}
	lines, _ := m.collapsedLines(collapsedWidth, 40)
	var strip []string
	for _, l := range lines {
		strip = append(strip, ansi.Strip(l.text))
	}
	joined := strings.Join(strip, "\n")
	if !strings.Contains(joined, updateGlyph) {
		t.Fatalf("collapsed strip = %q, want the bare glyph %q", joined, updateGlyph)
	}
	if strings.Contains(joined, "2.0.0") {
		t.Errorf("collapsed strip = %q, want no version number", joined)
	}

	quiet := &model{}
	qlines, _ := quiet.collapsedLines(collapsedWidth, 40)
	for _, l := range qlines {
		if strings.Contains(ansi.Strip(l.text), updateGlyph) {
			t.Errorf("collapsed strip carries the glyph with no update available: %q", ansi.Strip(l.text))
		}
	}
}

// Clicking the glyph opens the same one-line overlay whichever way you found
// it, and any keypress closes it again — there is nothing in it to navigate,
// so there is nothing a key could do besides dismiss it.
func TestClickingTheUpdateGlyphOpensAnOverlayDismissedByAnyKey(t *testing.T) {
	prevVersion := version
	version = "1.0.0"
	t.Cleanup(func() { version = prevVersion })

	m := fakeModel(t, 10, 80, 30) // wide: the overlay's line must not truncate below
	m.updateCheck = release.Check{Latest: "2.0.0"}
	viewOf(m) // re-compose: the field above changed after fakeModel's own render

	zone := m.pane.updateZone
	if zone.w == 0 {
		t.Fatal("updateZone is unset; the header did not publish a click target")
	}

	mm, _ := m.Update(tea.MouseMsg{X: zone.x, Y: zone.y,
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = mm.(*model)
	if !m.updateOpen {
		t.Fatal("clicking the glyph did not open the overlay")
	}
	want := "update available v1.0.0 → v2.0.0 — run: orchard upgrade"
	if got := ansi.Strip(viewOf(m)); !strings.Contains(got, want) {
		t.Errorf("view does not contain %q:\n%s", want, got)
	}

	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = mm.(*model)
	if m.updateOpen {
		t.Error("a keypress did not dismiss the overlay")
	}
}
