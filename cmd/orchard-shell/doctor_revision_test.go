package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// @scenario doctor fails when orchard-shell and orchard-sidebar revisions differ
//
// evaluateRevisions is the pure decision (#787 AC3): OK when the two revisions
// are equal, FAIL naming both when they differ.
func TestEvaluateRevisions(t *testing.T) {
	t.Run("equal revisions pass", func(t *testing.T) {
		got := evaluateRevisions("abc123", "abc123")
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass (detail: %s)", got.Status, got.Detail)
		}
	})

	t.Run("both unstamped pass (nothing to distinguish)", func(t *testing.T) {
		got := evaluateRevisions("", "")
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass", got.Status)
		}
	})

	t.Run("different revisions fail and name both", func(t *testing.T) {
		got := evaluateRevisions("abc123", "def456+dirty")
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

	t.Run("one unstamped against a real revision fails and is labelled", func(t *testing.T) {
		got := evaluateRevisions("abc123", "")
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail", got.Status)
		}
		if !strings.Contains(got.Detail, "unknown") {
			t.Errorf("Detail = %q; want it to label the missing side as unknown", got.Detail)
		}
	})
}

// checkRevisions end-to-end: orchard-shell's own revision (env.selfRevision) is
// compared against `orchard-sidebar --revision` execd off $PATH. A stale sidebar
// reporting a different revision fails the check and names both.
func TestCheckRevisions_MismatchedSidebarIsDetected(t *testing.T) {
	pathDir := t.TempDir()
	writeFakeRevisionBinary(t, pathDir, "orchard-sidebar", "sidebarREV")
	t.Setenv("PATH", pathDir)

	env := doctorEnv{
		self:         filepath.Join(t.TempDir(), "orchard-shell"), // no siblings: forces PATH resolution
		selfRevision: "shellREV",
		run:          runCommand,
	}
	got := checkRevisions(context.Background(), env)

	if got.Status != statusFail {
		t.Errorf("Status = %v; want fail (detail: %s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "shellREV") || !strings.Contains(got.Detail, "sidebarREV") {
		t.Errorf("Detail = %q; want it to name both revisions", got.Detail)
	}
}

// A matching sidebar revision passes.
func TestCheckRevisions_MatchingSidebarPasses(t *testing.T) {
	pathDir := t.TempDir()
	writeFakeRevisionBinary(t, pathDir, "orchard-sidebar", "sameREV")
	t.Setenv("PATH", pathDir)

	env := doctorEnv{
		self:         filepath.Join(t.TempDir(), "orchard-shell"),
		selfRevision: "sameREV",
		run:          runCommand,
	}
	if got := checkRevisions(context.Background(), env); got.Status != statusPass {
		t.Errorf("Status = %v; want pass (detail: %s)", got.Status, got.Detail)
	}
}

// writeFakeRevisionBinary writes an executable that prints revision regardless
// of its arguments — enough for resolveSidebarRevision's `--revision` exec.
func writeFakeRevisionBinary(t *testing.T, dir, name, revision string) {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\necho \"" + revision + "\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
