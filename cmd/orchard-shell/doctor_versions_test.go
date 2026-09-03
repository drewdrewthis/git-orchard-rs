package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

func TestEvaluateSuiteVersions(t *testing.T) {
	t.Run("all binaries agree on a real version", func(t *testing.T) {
		got := evaluateSuiteVersions([]binaryVersion{
			{"orchard", "1.4.0"}, {"orchard-daemon", "1.4.0"}, {"orchard-sidebar", "1.4.0"},
		})
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass (detail: %s)", got.Status, got.Detail)
		}
	})

	t.Run("all binaries agree on dev warns, does not fail", func(t *testing.T) {
		got := evaluateSuiteVersions([]binaryVersion{
			{"orchard", release.DevVersion}, {"orchard-daemon", release.DevVersion},
		})
		if got.Status != statusWarn {
			t.Errorf("Status = %v; want warn (detail: %s)", got.Status, got.Detail)
		}
	})

	t.Run("none resolved fails", func(t *testing.T) {
		got := evaluateSuiteVersions([]binaryVersion{
			{"orchard", unresolvedVersion}, {"orchard-daemon", unresolvedVersion},
		})
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail", got.Status)
		}
	})

	t.Run("mismatch fails and names every group", func(t *testing.T) {
		got := evaluateSuiteVersions([]binaryVersion{
			{"orchard", "1.4.0"}, {"orchard-daemon", "1.4.0"}, {"orchard-sidebar", "1.3.0"},
		})
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail", got.Status)
		}
		if !strings.Contains(got.Detail, "1.4.0") || !strings.Contains(got.Detail, "1.3.0") {
			t.Errorf("Detail = %q; want it to name both 1.4.0 and 1.3.0", got.Detail)
		}
		if !strings.Contains(got.Detail, "orchard-sidebar") {
			t.Errorf("Detail = %q; want it to name orchard-sidebar", got.Detail)
		}
		if got.Remedy == "" {
			t.Error("mismatch carries no remedy")
		}
	})

	t.Run("one unresolved among resolved ones fails and is labelled", func(t *testing.T) {
		got := evaluateSuiteVersions([]binaryVersion{
			{"orchard", "1.4.0"}, {"orchard-daemon", unresolvedVersion},
		})
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail", got.Status)
		}
		if !strings.Contains(got.Detail, "not found") {
			t.Errorf("Detail = %q; want it to label the unresolved binary as not found", got.Detail)
		}
	})
}

func TestLastToken(t *testing.T) {
	tests := []struct{ in, want string }{
		{"orchard-shell version 1.4.0", "1.4.0"},
		{"1.4.0", "1.4.0"},
		{"", unresolvedVersion},
		{"   ", unresolvedVersion},
	}
	for _, tt := range tests {
		if got := lastToken(tt.in); got != tt.want {
			t.Errorf("lastToken(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolveOneVersion_ShellUsesSelfVersionWithoutExec(t *testing.T) {
	env := doctorEnv{
		self:        filepath.Join(t.TempDir(), "orchard-shell"),
		selfVersion: "9.9.9",
		run: func(ctx context.Context, name string, args ...string) (string, error) {
			t.Fatalf("resolveOneVersion exec'd %s for orchard-shell; want no exec at all", name)
			return "", nil
		},
	}
	if got := resolveOneVersion(context.Background(), env, "orchard-shell"); got != "9.9.9" {
		t.Errorf("resolveOneVersion = %q; want 9.9.9", got)
	}
}

// TestCheckSuiteVersions_MismatchedSiblingIsDetectedEndToEnd reproduces AC8's
// literal scenario: an older orchard-sidebar resolved off a real $PATH,
// mismatched against the rest of the suite, fails the check and names both
// versions.
//
// Every fake binary is PATH-only (env.self has no siblings), so resolveBinary
// exercises its real exec.LookPath fallback rather than the sibling-first
// short-circuit.
func TestCheckSuiteVersions_MismatchedSiblingIsDetectedEndToEnd(t *testing.T) {
	pathDir := t.TempDir()
	const currentVersion = "2.0.0"
	const staleSidebarVersion = "1.9.0"
	for _, name := range release.SuiteBinaries {
		if name == "orchard-shell" {
			continue // resolved from env.selfVersion, never exec'd
		}
		version := currentVersion
		if name == "orchard-sidebar" {
			version = staleSidebarVersion
		}
		writeFakeVersionBinary(t, pathDir, name, version)
	}
	t.Setenv("PATH", pathDir)

	env := doctorEnv{
		self:        filepath.Join(t.TempDir(), "orchard-shell"), // no siblings here
		selfVersion: currentVersion,
		run:         runCommand,
	}
	got := checkSuiteVersions(context.Background(), env)

	if got.Status != statusFail {
		t.Errorf("Status = %v; want fail (detail: %s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, currentVersion) || !strings.Contains(got.Detail, staleSidebarVersion) {
		t.Errorf("Detail = %q; want it to name both %s and %s", got.Detail, currentVersion, staleSidebarVersion)
	}
}

// writeFakeVersionBinary writes an executable shell script named `name` into
// dir that prints "<name> version <version>" regardless of its arguments —
// enough for lastToken to recover version from a `--version` invocation.
func writeFakeVersionBinary(t *testing.T, dir, name, version string) {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\necho \"" + name + " version " + version + "\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
