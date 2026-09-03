package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// @scenario orchard --version reports the release tag's version
//
// AC1: `bin/orchard-shell --version` prints the VERSION passed to make, and
// `dev` when none was — the same two scenarios cmd/orchard-daemon pins.
func TestVersionBaked_LdflagsInjectsSemver(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary build test in short mode")
	}
	out := runVersion(t, buildShell(t, "-X main.version=1.2.3"))
	if !strings.Contains(out, "1.2.3") {
		t.Errorf("--version output = %q, want it to contain 1.2.3", out)
	}
}

// @scenario An unstamped dev build reports "dev"
func TestVersionBaked_DefaultIsDevWithoutLdflags(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary build test in short mode")
	}
	out := runVersion(t, buildShell(t, ""))
	if !strings.Contains(out, "dev") {
		t.Errorf("--version output = %q, want it to contain dev", out)
	}
}

// AC1's sibling-resolution rule: a stale orchard-sidebar earlier on $PATH must
// not beat the one installed beside this binary.
func TestSidebarNextTo_PrefersTheSibling(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(dir, sidebarBinary)
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := sidebarNextTo(filepath.Join(dir, "orchard-shell")); got != sibling {
		t.Errorf("sidebarNextTo = %q; want %q", got, sibling)
	}
	if got := sidebarNextTo(filepath.Join(t.TempDir(), "orchard-shell")); got != "" {
		t.Errorf("sidebarNextTo with no sibling = %q; want empty so $PATH is tried", got)
	}
	if got := sidebarNextTo(""); got != "" {
		t.Errorf("sidebarNextTo(\"\") = %q; want empty", got)
	}
}

func TestSidebarNextTo_IgnoresANonExecutableFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sidebarBinary), []byte("not a binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sidebarNextTo(filepath.Join(dir, "orchard-shell")); got != "" {
		t.Errorf("sidebarNextTo picked a non-executable file: %q", got)
	}
}

// binaryNextTo generalizes sidebarNextTo (doctor's suite-versions check
// resolves all six suite binaries, not just the sidebar) — this proves the
// generalization preserves sidebarNextTo's own behaviour for an arbitrary
// name.
func TestBinaryNextTo_PrefersTheSiblingForAnyName(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(dir, "orchard-daemon")
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := binaryNextTo(filepath.Join(dir, "orchard-shell"), "orchard-daemon"); got != sibling {
		t.Errorf("binaryNextTo = %q; want %q", got, sibling)
	}
	if got := binaryNextTo(filepath.Join(t.TempDir(), "orchard-shell"), "orchard-daemon"); got != "" {
		t.Errorf("binaryNextTo with no sibling = %q; want empty so $PATH is tried", got)
	}
}

// resolveBinary has no sibling in its own dir, so it must fall back to
// $PATH — the branch resolveSidebar's own tests never exercise, since they
// call sidebarNextTo directly.
func TestResolveBinary_FallsBackToPath(t *testing.T) {
	pathDir := t.TempDir()
	target := filepath.Join(pathDir, "orchard-upgrade")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)

	self := filepath.Join(t.TempDir(), "orchard-shell") // no sibling here
	if got := resolveBinary(self, "orchard-upgrade"); got != target {
		t.Errorf("resolveBinary = %q; want %q", got, target)
	}
}

func TestResolveBinary_EmptyWhenNeitherLookupFinds(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	self := filepath.Join(t.TempDir(), "orchard-shell")
	if got := resolveBinary(self, "orchard-tui"); got != "" {
		t.Errorf("resolveBinary = %q; want empty", got)
	}
}

func buildShell(t *testing.T, ldflags string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "orchard-shell")
	args := []string{"build", "-o", bin}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, "./cmd/orchard-shell")

	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	return bin
}

func runVersion(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", bin, err, out)
	}
	return string(out)
}
