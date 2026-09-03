package claudeaccount

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeExecutable drops a no-op POSIX script at dir/name with the
// executable bit set and returns its path. The scripts are never run —
// resolution only stats them — so the body is irrelevant.
func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestResolveToolPath_FoundOnPATH asserts PATH remains the first
// search location — the launchd fallbacks must not shadow a tool the
// operator deliberately put on PATH.
func TestResolveToolPath_FoundOnPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("resolution tests rely on POSIX executable bits")
	}
	dir := t.TempDir()
	want := writeExecutable(t, dir, "ccusage")

	t.Setenv("PATH", dir)
	t.Setenv(binDirsEnv, "") // no fallbacks: PATH must be what answers

	got, err := resolveToolPath("ccusage")
	if err != nil {
		t.Fatalf("resolveToolPath: %v", err)
	}
	if got != want {
		t.Errorf("resolveToolPath = %q, want %q", got, want)
	}
}

// TestResolveToolPath_LaunchdPATH_FoundInFallbackBinDir is the #400
// regression. Under launchd the daemon inherits a minimal PATH
// (/usr/bin:/bin:/usr/sbin:/sbin) that does not contain the user's
// bun/npm install prefix, so `ccusage` never resolved and every quota
// field came back null. Resolution must fall back to the well-known
// user bin dirs.
func TestResolveToolPath_LaunchdPATH_FoundInFallbackBinDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("resolution tests rely on POSIX executable bits")
	}
	home := t.TempDir()
	empty := t.TempDir()
	want := writeExecutable(t, filepath.Join(home, ".bun", "bin"), "ccusage")

	// The launchd environment: a PATH with none of the user's prefixes.
	t.Setenv("PATH", empty)
	t.Setenv("HOME", home)
	// t.Setenv first so the original value (set or unset) is restored at
	// test end; os.Unsetenv alone would leak an unset var into the rest
	// of the binary for anyone running with ORCHARD_BIN_DIRS exported.
	t.Setenv(binDirsEnv, "")
	os.Unsetenv(binDirsEnv) // exercise the real built-in fallback list

	got, err := resolveToolPath("ccusage")
	if err != nil {
		t.Fatalf("resolveToolPath under launchd-shaped PATH: %v", err)
	}
	if got != want {
		t.Errorf("resolveToolPath = %q, want %q (~/.bun/bin fallback)", got, want)
	}
}

// TestResolveToolPath_NotFoundAnywhere_ReturnsToolNotInstalled asserts
// the typed error is preserved when neither PATH nor the fallback dirs
// hold the tool — the resolver still needs errors.Is to map it to a
// per-field GraphQL error.
func TestResolveToolPath_NotFoundAnywhere_ReturnsToolNotInstalled(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	t.Setenv("HOME", empty)
	t.Setenv(binDirsEnv, empty)

	_, err := resolveToolPath("ccusage")
	if err == nil {
		t.Fatal("resolveToolPath succeeded with no ccusage anywhere; want ErrToolNotInstalled")
	}
	if !errors.Is(err, ErrToolNotInstalled) {
		t.Errorf("errors.Is(err, ErrToolNotInstalled) = false for %v; want true", err)
	}
	var typed *ToolNotInstalledError
	if !errors.As(err, &typed) {
		t.Fatalf("err = %v, want *ToolNotInstalledError", err)
	}
	if typed.Tool != "ccusage" {
		t.Errorf("typed.Tool = %q, want %q", typed.Tool, "ccusage")
	}
}

// TestResolveToolPath_EnvOverrideWins asserts the per-tool env pin
// beats both PATH and the fallback dirs, so an operator with a
// non-standard install can point the daemon at it directly.
func TestResolveToolPath_EnvOverrideWins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("resolution tests rely on POSIX executable bits")
	}
	onPath := t.TempDir()
	writeExecutable(t, onPath, "ccusage")
	elsewhere := t.TempDir()
	want := writeExecutable(t, elsewhere, "ccusage-custom")

	t.Setenv("PATH", onPath)
	t.Setenv(toolPathEnv("ccusage"), want)

	got, err := resolveToolPath("ccusage")
	if err != nil {
		t.Fatalf("resolveToolPath: %v", err)
	}
	if got != want {
		t.Errorf("resolveToolPath = %q, want the %s override %q", got, toolPathEnv("ccusage"), want)
	}
}

// TestResolveToolPath_EnvOverrideNotExecutable_Errors asserts a
// mis-set override fails loudly instead of silently falling through to
// PATH — a typo'd pin that silently resolves elsewhere is the same
// class of invisible failure as #400 itself.
func TestResolveToolPath_EnvOverrideNotExecutable_Errors(t *testing.T) {
	onPath := t.TempDir()
	writeExecutable(t, onPath, "ccusage")

	t.Setenv("PATH", onPath)
	t.Setenv(toolPathEnv("ccusage"), filepath.Join(t.TempDir(), "does-not-exist"))

	_, err := resolveToolPath("ccusage")
	if err == nil {
		t.Fatal("resolveToolPath succeeded with a dangling override; want an error")
	}
	if !errors.Is(err, ErrToolNotInstalled) {
		t.Errorf("errors.Is(err, ErrToolNotInstalled) = false for %v; want true", err)
	}
	if !strings.Contains(err.Error(), toolPathEnv("ccusage")) {
		t.Errorf("err = %q, want it to name the %s override", err, toolPathEnv("ccusage"))
	}
}

// TestToolPathEnv_DerivesOrchardVarName pins the documented naming
// rule so the env var an operator is told to set matches the one the
// daemon reads.
func TestToolPathEnv_DerivesOrchardVarName(t *testing.T) {
	cases := map[string]string{
		"ccusage": "ORCHARD_CCUSAGE_BIN",
		"claude":  "ORCHARD_CLAUDE_BIN",
	}
	for tool, want := range cases {
		if got := toolPathEnv(tool); got != want {
			t.Errorf("toolPathEnv(%q) = %q, want %q", tool, got, want)
		}
	}
}

// TestFallbackBinDirs_DefaultsCoverUserInstallPrefixes asserts the
// built-in list still contains the four prefixes #400 named, expanded
// against HOME.
func TestFallbackBinDirs_DefaultsCoverUserInstallPrefixes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(binDirsEnv, "") // registers restoration; see the launchd test
	os.Unsetenv(binDirsEnv)

	got := fallbackBinDirs()
	for _, want := range []string{
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".local", "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
	} {
		if !containsDir(got, want) {
			t.Errorf("fallbackBinDirs() = %v, missing %q", got, want)
		}
	}
}

// TestFallbackBinDirs_EnvReplacesDefaults asserts ORCHARD_BIN_DIRS
// replaces (not appends to) the built-in list, and that setting it
// empty disables the fallback entirely — the hermetic-test escape
// hatch.
func TestFallbackBinDirs_EnvReplacesDefaults(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t.Setenv(binDirsEnv, a+string(os.PathListSeparator)+b)

	got := fallbackBinDirs()
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("fallbackBinDirs() = %v, want exactly [%q %q]", got, a, b)
	}

	t.Setenv(binDirsEnv, "")
	if got := fallbackBinDirs(); len(got) != 0 {
		t.Errorf("fallbackBinDirs() with %s=\"\" = %v, want none", binDirsEnv, got)
	}
}

// TestToolLocator_WarnsOnceWhenToolMissing asserts the missing-tool
// case is visible in the daemon log (it was completely silent before
// #400) without re-warning on every 60s poll tick.
func TestToolLocator_WarnsOnceWhenToolMissing(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	t.Setenv("HOME", empty)
	t.Setenv(binDirsEnv, empty)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	loc := newToolLocator(logger)

	for i := 0; i < 3; i++ {
		if _, err := loc.Locate("ccusage"); err == nil {
			t.Fatalf("Locate(ccusage) succeeded on attempt %d; want not-installed", i)
		}
	}

	warns := strings.Count(buf.String(), "level=WARN")
	if warns != 1 {
		t.Errorf("logged %d WARN lines across 3 lookups, want exactly 1:\n%s", warns, buf.String())
	}
	if !strings.Contains(buf.String(), "ccusage") {
		t.Errorf("warn line does not name the missing tool:\n%s", buf.String())
	}
}

// TestToolLocator_ReWarnsAfterRecovery asserts the warn latch clears
// when the tool reappears, so a later disappearance is reported again
// rather than swallowed for the life of the daemon.
func TestToolLocator_ReWarnsAfterRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("resolution tests rely on POSIX executable bits")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binDirsEnv, "")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	loc := newToolLocator(logger)

	if _, err := loc.Locate("ccusage"); err == nil {
		t.Fatal("Locate succeeded before the tool was installed")
	}
	bin := writeExecutable(t, dir, "ccusage")
	if got, err := loc.Locate("ccusage"); err != nil || got != bin {
		t.Fatalf("Locate after install = (%q, %v), want (%q, nil)", got, err, bin)
	}
	if err := os.Remove(bin); err != nil {
		t.Fatalf("remove %s: %v", bin, err)
	}
	if _, err := loc.Locate("ccusage"); err == nil {
		t.Fatal("Locate succeeded after the tool was removed")
	}

	if warns := strings.Count(buf.String(), "level=WARN"); warns != 2 {
		t.Errorf("logged %d WARN lines across miss→hit→miss, want 2:\n%s", warns, buf.String())
	}
}

// containsDir reports whether want appears in dirs.
func containsDir(dirs []string, want string) bool {
	for _, d := range dirs {
		if d == want {
			return true
		}
	}
	return false
}
