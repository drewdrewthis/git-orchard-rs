package release

import (
	"strings"

	"golang.org/x/mod/semver"
)

// DevVersion is the version an unreleased build reports — the package-level
// default every orchard main.go declares before -ldflags overwrites it.
const DevVersion = "dev"

// Compare orders two orchard version strings, with or without a leading "v".
//
// Anything that is not valid semver — "dev" above all — sorts before every
// real version, so an unreleased binary is always the older side of a
// comparison and is always offered the upgrade rather than being told it is
// current.
func Compare(a, b string) int {
	av, bv := canonical(a), canonical(b)
	switch {
	case av == "" && bv == "":
		return 0
	case av == "":
		return -1
	case bv == "":
		return 1
	}
	return semver.Compare(av, bv)
}

// IsNewer reports whether latest is strictly newer than current.
func IsNewer(latest, current string) bool { return Compare(latest, current) > 0 }

// canonical normalises a version for comparison, returning "" when the string
// is not a semver at all.
func canonical(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return ""
	}
	return v
}
