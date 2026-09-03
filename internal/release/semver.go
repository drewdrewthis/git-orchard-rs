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

// IsSemver reports whether v is a real release stamp — the one rule callers
// need instead of hand-rolling their own check against the literal "dev" or
// against a git-describe string with no version core, like "abc1234-dirty",
// neither of which is semver even though both look version-shaped. Note that
// "v1.2.3-<anything>" IS valid semver (a prerelease identifier), so a
// git-describe stamp built off a tag (e.g. "v1.2.3-3-gabc1234-dirty") will
// pass this check.
func IsSemver(v string) bool { return canonical(v) != "" }

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
