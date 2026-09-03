package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseArgs_Defaults(t *testing.T) {
	opts, err := parseArgs(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseArgs(nil): %v", err)
	}
	if opts != (Options{}) {
		t.Errorf("parseArgs(nil) = %+v; want the zero Options", opts)
	}
}

// --version means two things, and both spellings have to work: bare it prints
// this binary's version (the contract every orchard binary shares), and with a
// value it pins the release.
func TestParseArgs_VersionIsBothAQueryAndAPin(t *testing.T) {
	cases := []struct {
		argv       []string
		wantShow   bool
		wantTarget string
		wantPrefix string
	}{
		{argv: []string{"--version"}, wantShow: true},
		{argv: []string{"--version", "v1.2.3"}, wantTarget: "v1.2.3"},
		{argv: []string{"--version=v1.2.3"}, wantTarget: "v1.2.3"},
		{argv: []string{"--version", "1.2.3"}, wantTarget: "1.2.3"},
		{argv: []string{"--version", "v1.2.3", "--prefix", "/tmp"}, wantTarget: "v1.2.3", wantPrefix: "/tmp"},
		{argv: []string{"--prefix", "/tmp", "--version"}, wantShow: true, wantPrefix: "/tmp"},
	}
	for _, c := range cases {
		opts, err := parseArgs(c.argv, io.Discard)
		if err != nil {
			t.Errorf("parseArgs(%v): %v", c.argv, err)
			continue
		}
		if opts.ShowVersion != c.wantShow || opts.Target != c.wantTarget || opts.Prefix != c.wantPrefix {
			t.Errorf("parseArgs(%v) = show:%v target:%q prefix:%q; want show:%v target:%q prefix:%q",
				c.argv, opts.ShowVersion, opts.Target, opts.Prefix, c.wantShow, c.wantTarget, c.wantPrefix)
		}
	}
}

func TestParseArgs_CheckAndDryRun(t *testing.T) {
	opts, err := parseArgs([]string{"--check"}, io.Discard)
	if err != nil || !opts.Check {
		t.Fatalf("parseArgs(--check) = %+v, %v", opts, err)
	}
	opts, err = parseArgs([]string{"--dry-run"}, io.Discard)
	if err != nil || !opts.DryRun {
		t.Fatalf("parseArgs(--dry-run) = %+v, %v", opts, err)
	}
	if _, err := parseArgs([]string{"--check", "--dry-run"}, io.Discard); err == nil {
		t.Error("parseArgs(--check --dry-run) succeeded; the two are alternatives")
	}
}

func TestParseArgs_RejectsStrayArguments(t *testing.T) {
	for _, argv := range [][]string{{"install"}, {"--check", "now"}} {
		if _, err := parseArgs(argv, io.Discard); err == nil {
			t.Errorf("parseArgs(%v) succeeded; want an error naming the stray argument", argv)
		}
	}
}

func TestParseArgs_HelpIsFlagErrHelp(t *testing.T) {
	if _, err := parseArgs([]string{"--help"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseArgs(--help) = %v; want flag.ErrHelp", err)
	}
}

func TestRun_VersionPrintsTheBakedVersion(t *testing.T) {
	withVersion(t, "9.9.9")
	var stdout, stderr strings.Builder
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--version) = %d; want 0", code)
	}
	if strings.TrimSpace(stdout.String()) != "9.9.9" {
		t.Errorf("stdout = %q; want 9.9.9", stdout.String())
	}
}

// AC5/AC1's shared contract: -ldflags stamps the version, and a plain build
// reports dev.
func TestVersionBaked_LdflagsInjectsSemver(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary build test in short mode")
	}
	if out := runVersionBinary(t, buildUpgrade(t, "-X main.version=1.2.3")); !strings.Contains(out, "1.2.3") {
		t.Errorf("--version output = %q, want it to contain 1.2.3", out)
	}
}

func TestVersionBaked_DefaultIsDevWithoutLdflags(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary build test in short mode")
	}
	if out := runVersionBinary(t, buildUpgrade(t, "")); !strings.Contains(out, "dev") {
		t.Errorf("--version output = %q, want it to contain dev", out)
	}
}

func buildUpgrade(t *testing.T, ldflags string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "orchard-upgrade")
	args := []string{"build", "-o", bin}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, "./cmd/orchard-upgrade")

	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	return bin
}

func runVersionBinary(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", bin, err, out)
	}
	return string(out)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
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
