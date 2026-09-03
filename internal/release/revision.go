package release

import (
	"fmt"
	"io"
	"runtime/debug"
)

// revision is overridden via -ldflags "-X ...release.revision=<sha>" at build
// time (Makefile, scripts/dist.sh). It takes precedence over the embedded
// build info so a release build carries a revision even where the VCS stamp is
// absent, and so the Rust and Go binaries can be pinned to one commit.
var revision string

// RevisionBinaries are the suite binaries that answer `--revision` with their
// build-time VCS commit — the Go binaries only, in SuiteBinaries order. The
// Rust pair (orchard, orchard-tui) is excluded: Go bakes vcs.revision into
// every binary automatically, while stamping the Rust builds would mean running
// git at tarball-build time to inject it, a dependency the release does not
// take. orchard-shell's doctor iterates this set, not SuiteBinaries.
var RevisionBinaries = []string{
	"orchard-daemon",
	"orchard-sidebar",
	"orchard-shell",
	"orchard-upgrade",
}

// HandleRevisionFlag answers a bare `--revision` invocation: when args[0] is
// "--revision" it writes this binary's Revision() as one line and returns true,
// so every suite binary shares one implementation. The caller wires it as the
// first thing in main, before its own flag parsing, and os.Exit(0)s on true.
func HandleRevisionFlag(args []string, w io.Writer) bool {
	if len(args) > 0 && args[0] == "--revision" {
		fmt.Fprintln(w, Revision())
		return true
	}
	return false
}

// Revision returns this binary's VCS revision: the -ldflags override when set,
// else the embedded build info, with a "+dirty" suffix when the working tree
// was modified at build time, or "" when the build carried no VCS stamp (e.g.
// -buildvcs=false).
//
// orchard-shell's doctor compares it across suite binaries: two binaries built
// from the same commit report the same revision, while a stale/prototype build
// reports a different one — the exact skew that silently broke sidebar clicks
// (orchardist#787) yet can hide behind an identical --version "dev".
func Revision() string {
	if revision != "" {
		return revision
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" && dirty {
		rev += "+dirty"
	}
	return rev
}
