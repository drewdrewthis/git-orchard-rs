// Command orchard-upgrade replaces the installed orchard binaries with a
// verified GitHub release.
//
// Per ADR-013 it is dispatched as `orchard upgrade`. It supersedes the TUI
// stub that only printed a download URL: this one resolves the release,
// verifies the download against the release's SHA256SUMS, and atomically
// replaces every orchard binary sitting beside it — helpers first, the
// dispatcher last, so a crash mid-set still leaves a working `orchard`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// version is overridden via -ldflags at release time.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable body. It returns the process exit code.
func run(argv []string, stdout, stderr io.Writer) int {
	// --revision prints the bare VCS revision before flag parsing, so doctor
	// can compare it across the suite (orchardist#787).
	if release.HandleRevisionFlag(argv, stdout) {
		return 0
	}
	opts, err := parseArgs(argv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "orchard upgrade: %v\n", err)
		return 1
	}
	if opts.ShowVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}

	dir, err := installDir(opts.Prefix)
	if err != nil {
		fmt.Fprintf(stderr, "orchard upgrade: %v\n", err)
		return 1
	}
	if err := upgrade(context.Background(), opts, dir, version, stdout); err != nil {
		fmt.Fprintf(stderr, "orchard upgrade: %v\n", err)
		return 1
	}
	return 0
}

// installDir resolves where the binaries live: --prefix, else the directory
// holding this binary. Sibling-of-self is the same rule the dispatcher uses
// to find its helpers (ADR-013), so upgrade replaces the set it is part of
// rather than whichever copy happens to be first on $PATH.
func installDir(prefix string) (string, error) {
	if prefix != "" {
		abs, err := filepath.Abs(prefix)
		if err != nil {
			return "", fmt.Errorf("resolve --prefix %s: %w", prefix, err)
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return "", fmt.Errorf("--prefix %s is not a directory", prefix)
		}
		return abs, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate this binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return filepath.Dir(self), nil
}
