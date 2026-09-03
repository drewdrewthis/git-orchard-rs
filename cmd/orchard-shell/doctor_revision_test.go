package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// @scenario doctor fails when the suite binaries were built from different revisions
//
// evaluateRevisions is the pure decision (#787 AC3): OK when every binary
// agrees, FAIL naming each group when they differ. It covers the whole suite,
// so a missing/erroring binary (the unresolvedVersion sentinel) FAILs rather
// than reading as an unstamped-but-matching build.
func TestEvaluateRevisions(t *testing.T) {
	t.Run("equal revisions pass", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard", "abc123"}, {"orchard-daemon", "abc123"}, {"orchard-sidebar", "abc123"},
		})
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass (detail: %s)", got.Status, got.Detail)
		}
	})

	t.Run("all unstamped pass (nothing to distinguish)", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard", ""}, {"orchard-daemon", ""},
		})
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass", got.Status)
		}
	})

	t.Run("different revisions fail and name both", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard", "abc123"}, {"orchard-sidebar", "def456+dirty"},
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

	t.Run("one unstamped against a real revision fails and is labelled unknown", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard", "abc123"}, {"orchard-sidebar", ""},
		})
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail", got.Status)
		}
		if !strings.Contains(got.Detail, "unknown") {
			t.Errorf("Detail = %q; want it to label the missing side as unknown", got.Detail)
		}
	})

	t.Run("a missing binary among resolved ones fails and is labelled not found", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard", "abc123"}, {"orchard-sidebar", unresolvedVersion},
		})
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail — a missing binary must never read as an unstamped pass", got.Status)
		}
		if !strings.Contains(got.Detail, "not found") {
			t.Errorf("Detail = %q; want it to label the unresolved binary as not found", got.Detail)
		}
	})

	t.Run("all unresolved fails", func(t *testing.T) {
		got := evaluateRevisions([]binaryVersion{
			{"orchard", unresolvedVersion}, {"orchard-daemon", unresolvedVersion},
		})
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail", got.Status)
		}
	})
}

// checkRevisions end-to-end: every suite binary's --revision (orchard-shell's
// own via env.selfRevision) is grouped, and a stale sidebar reporting a
// different revision fails the check and names both.
//
// Every fake binary is PATH-only (env.self has no siblings), so resolveBinary
// exercises its real exec.LookPath fallback rather than the sibling short-cut.
func TestCheckRevisions_MismatchedSidebarIsDetected(t *testing.T) {
	pathDir := t.TempDir()
	const currentRev = "shellREV"
	const staleSidebarRev = "sidebarREV"
	for _, name := range release.SuiteBinaries {
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

// A suite whose binaries all report the same revision passes.
func TestCheckRevisions_MatchingSuitePasses(t *testing.T) {
	pathDir := t.TempDir()
	const rev = "sameREV"
	for _, name := range release.SuiteBinaries {
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
	if got := checkRevisions(context.Background(), env); got.Status != statusPass {
		t.Errorf("Status = %v; want pass (detail: %s)", got.Status, got.Detail)
	}
}

// A binary that cannot be resolved off $PATH fails the check rather than reading
// as an unstamped-but-matching build (the unresolvedVersion sentinel).
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
