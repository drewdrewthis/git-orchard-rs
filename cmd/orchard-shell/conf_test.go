package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// While scripts/outer-shell/outer.conf still exists it is the file verify.sh
// source-files and the one a human edits; cmd/orchard-shell/outer.conf is the
// go:embed copy. A drift between them means the binary boots a config nobody
// is reading — the exact failure the plan's "git mv, not copy" note guards.
// Delete this test in the same commit that deletes the scripts/ copy.
func TestEmbeddedConf_MatchesTheScriptsCopyByteForByte(t *testing.T) {
	repo := repoRoot(t)
	scripted := filepath.Join(repo, "scripts", "outer-shell", "outer.conf")
	want, err := os.ReadFile(scripted)
	if os.IsNotExist(err) {
		t.Skip("scripts/outer-shell/outer.conf has been removed; the embedded copy is now the only one")
	}
	if err != nil {
		t.Fatalf("read %s: %v", scripted, err)
	}
	if string(want) != string(embeddedConf) {
		t.Fatalf("cmd/orchard-shell/outer.conf has drifted from %s\nRe-copy it:  cp %s %s",
			scripted, scripted, filepath.Join(repo, "cmd", "orchard-shell", "outer.conf"))
	}
}

func TestEmbeddedConf_IsTheRealConfig(t *testing.T) {
	for _, want := range []string{"set -g prefix None", "set -g mouse on", "set-hook -g window-resized", "set -s set-clipboard on"} {
		if !strings.Contains(string(embeddedConf), want) {
			t.Errorf("embedded outer.conf is missing %q", want)
		}
	}
}

// @scenario The embedded outer.conf is content-hashed so upgrades never reuse a stale copy
//
// AC1: the materialised name carries the first 12 hex chars of the embedded
// conf's sha256, so an upgrade can never re-use a stale conf.
func TestMaterializeConf_ContentHashedName(t *testing.T) {
	dir := t.TempDir()
	path, err := materializeConf(dir, embeddedConf)
	if err != nil {
		t.Fatalf("materializeConf: %v", err)
	}
	digest := release.SHA256(embeddedConf)[:12]
	if base := filepath.Base(path); base != "outer-"+digest+".conf" {
		t.Errorf("materialised as %q; want outer-%s.conf", base, digest)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read materialised conf: %v", err)
	}
	if string(got) != string(embeddedConf) {
		t.Error("materialised conf differs from the embedded bytes")
	}
}

func TestMaterializeConf_DifferentContentDifferentPath(t *testing.T) {
	dir := t.TempDir()
	a, err := materializeConf(dir, []byte("set -g mouse on\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := materializeConf(dir, []byte("set -g mouse off\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two different configs materialised to the same path %q", a)
	}
}

func TestMaterializeConf_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	first, err := materializeConf(dir, embeddedConf)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := materializeConf(dir, embeddedConf)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("second call materialised to %q; want %q", second, first)
	}
	after, _ := os.Stat(second)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("an unchanged conf was rewritten; the second call must be a no-op")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("state dir holds %d files; want 1", len(entries))
	}
}

// AC1: --conf <path> uses that file. A missing one is an error, not a silent
// fallback to the embedded copy — that would hide a typo behind behaviour
// that looks almost right.
func TestResolveConf_OverrideIsUsedAndMustExist(t *testing.T) {
	dir := t.TempDir()
	override := filepath.Join(dir, "other.conf")
	if err := os.WriteFile(override, []byte("set -g status on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveConf(override)
	if err != nil {
		t.Fatalf("resolveConf(%s): %v", override, err)
	}
	if got != override {
		t.Errorf("resolveConf = %q; want the override path", got)
	}

	if _, err := resolveConf(filepath.Join(dir, "absent.conf")); err == nil {
		t.Error("resolveConf accepted a --conf path that does not exist")
	}
}

func TestResolveConf_DefaultLandsUnderTheStateDir(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	got, err := resolveConf("")
	if err != nil {
		t.Fatalf("resolveConf(\"\"): %v", err)
	}
	want := filepath.Join(state, "orchard")
	if filepath.Dir(got) != want {
		t.Errorf("resolveConf = %q; want a file under %q", got, want)
	}
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from the test directory")
		}
		dir = parent
	}
}
