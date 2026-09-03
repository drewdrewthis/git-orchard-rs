package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// @scenario doctor warns, not fails, when a suite binary lacks --revision
// @scenario doctor detects a suite built from different revisions
//
// evaluateRevisions is the pure decision (#787 AC3, #803): PASS when every
// binary agrees; WARN when they agree but ≥1 binary predates --revision; FAIL
// when the resolved revisions disagree or a binary is missing. It covers only
// the Go RevisionBinaries — the Rust pair is named in the excluded suffix.
func TestEvaluateRevisions(t *testing.T) {
	t.Run("equal revisions pass", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard-daemon", "abc123"}, {"orchard-sidebar", "abc123"}, {"orchard-shell", "abc123"},
		})
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass (detail: %s)", got.Status, got.Detail)
		}
	})

	t.Run("all unstamped pass (nothing to distinguish)", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard-daemon", ""}, {"orchard-sidebar", ""},
		})
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass", got.Status)
		}
	})

	t.Run("every detail names the excluded Rust binaries", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard-daemon", "abc123"}, {"orchard-sidebar", "abc123"},
		})
		if !strings.Contains(got.Detail, "orchard-tui") || !strings.Contains(got.Detail, "Rust") {
			t.Errorf("Detail = %q; want the excluded-Rust suffix", got.Detail)
		}
	})

	t.Run("a binary lacking --revision warns and names it, others match", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard-daemon", "abc123"}, {"orchard-sidebar", "abc123"},
			{"orchard-upgrade", unimplementedRevision},
		})
		if got.Status != statusWarn {
			t.Errorf("Status = %v; want warn (detail: %s)", got.Status, got.Detail)
		}
		if !strings.Contains(got.Detail, "orchard-upgrade") {
			t.Errorf("Detail = %q; want it to name the binary lacking --revision", got.Detail)
		}
		if got.Remedy == "" || strings.Contains(strings.ToLower(got.Remedy), "reinstall") {
			t.Errorf("Remedy = %q; want a rebuild remedy, not reinstall", got.Remedy)
		}
	})

	t.Run("different revisions fail and name both", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard-daemon", "abc123"}, {"orchard-sidebar", "def456+dirty"},
		})
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail", got.Status)
		}
		if !strings.Contains(got.Detail, "abc123") || !strings.Contains(got.Detail, "def456+dirty") {
			t.Errorf("Detail = %q; want it to name both revisions", got.Detail)
		}
		if got.Remedy == "" {
			t.Error("mismatch carries no remedy")
		}
	})

	t.Run("a missing binary among resolved ones fails and is labelled not found", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard-daemon", "abc123"}, {"orchard-sidebar", unresolvedVersion},
		})
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail — a missing binary must never read as a warn or pass", got.Status)
		}
		if !strings.Contains(got.Detail, "not found") {
			t.Errorf("Detail = %q; want it to label the unresolved binary as not found", got.Detail)
		}
	})

	t.Run("all missing fails", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard-daemon", unresolvedVersion}, {"orchard-sidebar", unresolvedVersion},
		})
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail", got.Status)
		}
	})
}

// checkRevisions end-to-end: every RevisionBinaries member's --revision
// (orchard-shell's own via env.selfRevision) is grouped, and a stale sidebar
// reporting a different revision fails the check and names both. Rust binaries
// are never in the iteration, so they are never exec'd.
//
// Every fake binary is PATH-only (env.self has no siblings), so resolveBinary
// exercises its real exec.LookPath fallback rather than the sibling short-cut.
func TestCheckRevisions_MismatchedSidebarIsDetected(t *testing.T) {
	pathDir := t.TempDir()
	const currentRev = "shellREV"
	const staleSidebarRev = "sidebarREV"
	for _, name := range release.RevisionBinaries {
		if name == "orchard-shell" {
			continue // resolved from env.selfRevision, never exec'd
		}
		rev := currentRev
		if name == "orchard-sidebar" {
			rev = staleSidebarRev
		}
		writeFakeRevisionBinary(t, pathDir, name, rev)
	}
	t.Setenv("PATH", pathDir)

	env := doctorEnv{
		self:         filepath.Join(t.TempDir(), "orchard-shell"), // no siblings: forces PATH resolution
		selfRevision: currentRev,
		run:          runCommand,
	}
	got := checkRevisions(context.Background(), env)

	if got.Status != statusFail {
		t.Errorf("Status = %v; want fail (detail: %s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, currentRev) || !strings.Contains(got.Detail, staleSidebarRev) {
		t.Errorf("Detail = %q; want it to name both revisions", got.Detail)
	}
}

// A suite whose binaries all report the same revision passes, and the pass
// detail still names the excluded Rust binaries.
func TestCheckRevisions_MatchingSuitePasses(t *testing.T) {
	pathDir := t.TempDir()
	const rev = "sameREV"
	for _, name := range release.RevisionBinaries {
		if name == "orchard-shell" {
			continue
		}
		writeFakeRevisionBinary(t, pathDir, name, rev)
	}
	t.Setenv("PATH", pathDir)

	env := doctorEnv{
		self:         filepath.Join(t.TempDir(), "orchard-shell"),
		selfRevision: rev,
		run:          runCommand,
	}
	got := checkRevisions(context.Background(), env)
	if got.Status != statusPass {
		t.Errorf("Status = %v; want pass (detail: %s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "orchard") || !strings.Contains(got.Detail, "Rust") {
		t.Errorf("Detail = %q; want it to name the excluded Rust binaries", got.Detail)
	}
}

// A suite binary that is present but predates --revision warns (not fails), so
// a single-checkout install can adopt the check without a hard failure.
func TestCheckRevisions_BinaryLackingFlagWarns(t *testing.T) {
	pathDir := t.TempDir()
	const rev = "sameREV"
	for _, name := range release.RevisionBinaries {
		if name == "orchard-shell" {
			continue
		}
		if name == "orchard-upgrade" {
			writeUnimplementedRevisionBinary(t, pathDir, name)
			continue
		}
		writeFakeRevisionBinary(t, pathDir, name, rev)
	}
	t.Setenv("PATH", pathDir)

	env := doctorEnv{
		self:         filepath.Join(t.TempDir(), "orchard-shell"),
		selfRevision: rev,
		run:          runCommand,
	}
	got := checkRevisions(context.Background(), env)
	if got.Status != statusWarn {
		t.Errorf("Status = %v; want warn (detail: %s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "orchard-upgrade") {
		t.Errorf("Detail = %q; want it to name orchard-upgrade", got.Detail)
	}
}

// A binary that cannot be resolved off $PATH fails the check rather than
// reading as a warn or pass (the unresolvedVersion sentinel).
func TestCheckRevisions_MissingBinaryFails(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no suite binaries findable
	env := doctorEnv{
		self:         filepath.Join(t.TempDir(), "orchard-shell"),
		selfRevision: "shellREV", // shell resolves, the rest do not
		run:          runCommand,
	}
	if got := checkRevisions(context.Background(), env); got.Status != statusFail {
		t.Errorf("Status = %v; want fail when siblings cannot be resolved", got.Status)
	}
}

// writeFakeRevisionBinary writes an executable that prints revision regardless
// of its arguments — enough for resolveOneRevision's `--revision` exec.
func writeFakeRevisionBinary(t *testing.T, dir, name, revision string) {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\necho \"" + revision + "\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeUnimplementedRevisionBinary writes an executable that rejects --revision
// with a nonzero exit — an older build that predates the flag.
func writeUnimplementedRevisionBinary(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\necho \"unknown flag: --revision\" >&2\nexit 2\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
