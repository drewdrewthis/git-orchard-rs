package main

import (
	"runtime/debug"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// readBuildInfo is a seam: the real binary reaches its build stamp only
// through debug.ReadBuildInfo, which tests cannot populate, so they swap this.
var readBuildInfo = debug.ReadBuildInfo

// buildIdent is the dev-build label: "dev@<7-char vcs.revision>", with a
// trailing "*" when the tree was dirty at build (vcs.modified == "true"). It
// falls back to plain "dev" when there is no VCS stamp to read — a `go build`
// outside a repo, or one built with -buildvcs=false. A dev build has no
// semver, so the header labels it with its VCS revision instead — enough to
// tell two dev binaries apart at a glance (#789).
func buildIdent() string {
	info, ok := readBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	ident := "dev@" + rev
	if modified == "true" {
		ident += "*"
	}
	return ident
}

// isDevBuild is the one home for the rule of what counts as an unreleased
// binary: no semver stamped, not literally the string "dev" — a git-describe
// build (e.g. "v1.1.0-3-gabc1234-dirty") is just as unreleased (AC1 of #789).
func isDevBuild() bool {
	return !release.IsSemver(version)
}
