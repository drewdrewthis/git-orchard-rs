package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// unimplementedRevision is the group key for a suite binary that was found and
// runs, but does not answer --revision (an older build that predates this
// check). It is distinct from unresolvedVersion (not found at all): a binary
// that lacks the flag warns rather than fails, because the fix is to rebuild it
// from a checkout that has --revision, not to reinstall.
const unimplementedRevision = "(no --revision)"

// checkRevisions is #787's defense inside the toolchain: two suite binaries can
// report the same --version "dev" yet come from different commits — the exact
// shape of the stale prototype launcher that silently broke sidebar clicks.
// vcs.revision distinguishes them where the semver check (checkSuiteVersions)
// cannot. It covers release.RevisionBinaries — the Go binaries only, since the
// Rust pair carries no build stamp to compare.
func checkRevisions(ctx context.Context, env doctorEnv) checkResult {
	return evaluateRevisions(resolveSuiteRevisions(ctx, env))
}

// resolveSuiteRevisions is the impure half: a revision per RevisionBinaries
// member, using the same sibling-then-PATH resolution as resolveSuiteVersions.
func resolveSuiteRevisions(ctx context.Context, env doctorEnv) []binaryVersion {
	out := make([]binaryVersion, 0, len(release.RevisionBinaries))
	for _, name := range release.RevisionBinaries {
		out = append(out, binaryVersion{name: name, version: resolveOneRevision(ctx, env, name)})
	}
	return out
}

// resolveOneRevision resolves and runs a single suite binary's --revision.
//
// orchard-shell is read in-process (env.selfRevision) for the same reason
// resolveOneVersion special-cases it: self-exec would just rerun this binary.
// A binary that cannot be found returns unresolvedVersion (a FAIL); one that is
// found but errors on --revision returns unimplementedRevision (a WARN — it
// predates the flag). An empty ("") revision is a value like any other: a
// genuinely unstamped build, distinct from both sentinels.
func resolveOneRevision(ctx context.Context, env doctorEnv, name string) string {
	if name == "orchard-shell" {
		return env.selfRevision
	}
	path := resolveBinary(env.self, name)
	if path == "" {
		return unresolvedVersion
	}
	out, err := env.run(ctx, path, "--revision")
	if err != nil {
		return unimplementedRevision
	}
	return strings.TrimSpace(out)
}

// evaluateRevisions is the pure decision. Precedence, highest severity first:
//
//   - FAIL when the resolved revisions genuinely disagree, or any binary could
//     not be found — the #787 skew signal must never read as a pass.
//   - WARN when every resolved revision agrees but ≥1 binary lacks --revision:
//     the check cannot confirm those binaries, and the fix is a rebuild.
//   - PASS when every binary reports the same revision (or all are unstamped).
//
// It shares grouping and the mismatch listing with the version check
// (groupSuite, suiteMismatch); every detail carries the excluded-Rust suffix.
func evaluateRevisions(revisions []binaryVersion) checkResult {
	groups := groupSuite(revisions)

	realGroups := map[string][]string{}
	var unimplemented, unresolved []string
	for value, names := range groups {
		switch value {
		case unresolvedVersion:
			unresolved = names
		case unimplementedRevision:
			unimplemented = names
		default:
			realGroups[value] = names // an actual revision, or "" (unstamped)
		}
	}

	suffix := excludedSuffix()

	if len(realGroups) == 0 && len(unimplemented) == 0 {
		return checkResult{ID: "suite-revisions", Status: statusFail,
			Detail: "none of the suite binaries could report a revision" + suffix,
			Remedy: "reinstall orchard so its binaries are on $PATH or beside orchard-shell"}
	}
	if len(realGroups) > 1 || len(unresolved) > 0 {
		res := suiteMismatch("suite-revisions", "suite binaries built from different revisions: ",
			"reinstall or rebuild every orchard binary from the same checkout so their revisions match", groups)
		res.Detail += suffix
		return res
	}
	if len(unimplemented) > 0 {
		sort.Strings(unimplemented)
		return checkResult{ID: "suite-revisions", Status: statusWarn,
			Detail: fmt.Sprintf("these suite binaries do not answer --revision: %s%s", strings.Join(unimplemented, ", "), suffix),
			Remedy: "rebuild the listed binaries from a checkout that answers `--revision` — they predate this check"}
	}
	for rev := range realGroups {
		if rev == "" {
			return checkResult{ID: "suite-revisions", Status: statusPass,
				Detail: fmt.Sprintf("all %d suite binaries carry no VCS revision (an unstamped build)%s", len(revisions), suffix)}
		}
		return checkResult{ID: "suite-revisions", Status: statusPass,
			Detail: fmt.Sprintf("all %d suite binaries built from revision %s%s", len(revisions), rev, suffix)}
	}
	return checkResult{ID: "suite-revisions", Status: statusPass, Detail: "no suite binaries to check" + suffix}
}

// excludedSuffix names the SuiteBinaries the revision check cannot cover — the
// Rust binaries, derived as SuiteBinaries minus RevisionBinaries rather than
// hardcoded, so the two lists cannot silently drift apart.
func excludedSuffix() string {
	covered := map[string]bool{}
	for _, name := range release.RevisionBinaries {
		covered[name] = true
	}
	var excluded []string
	for _, name := range release.SuiteBinaries {
		if !covered[name] {
			excluded = append(excluded, name)
		}
	}
	if len(excluded) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s excluded: Rust, not stamped)", strings.Join(excluded, ", "))
}
