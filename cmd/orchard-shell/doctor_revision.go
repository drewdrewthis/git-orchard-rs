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
// A binary that cannot be found returns unresolvedVersion (a FAIL). When
// --revision errors we must distinguish a healthy older build that predates the
// flag from a crashed binary: we probe --version. A binary that answers
// --version is alive and merely lacks --revision → unimplementedRevision (a
// WARN, fix is a rebuild); one that fails --version too is broken → the same
// unresolvedVersion FAIL as a missing binary, never masked as "predates the
// flag". An empty ("") revision is a value like any other: a genuinely
// unstamped build, distinct from both sentinels.
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
		if _, verr := env.run(ctx, path, "--version"); verr != nil {
			return unresolvedVersion
		}
		return unimplementedRevision
	}
	return strings.TrimSpace(out)
}

// evaluateRevisions is the pure decision. Precedence, highest severity first:
//
//   - FAIL when the resolved revisions genuinely disagree, any binary could not
//     be found, or any binary reports an empty (unstamped) revision — the #787
//     skew signal must never read as a pass.
//   - WARN when every resolved revision agrees but ≥1 binary lacks --revision:
//     the check cannot confirm those binaries, and the fix is a rebuild.
//   - PASS when every binary reports the same non-empty revision.
//
// It shares grouping and the mismatch listing with the version check
// (groupSuite, suiteMismatch); every detail carries the excluded-Rust suffix.
func evaluateRevisions(revisions []binaryVersion) checkResult {
	groups := groupSuite(revisions)

	realGroups := map[string][]string{}
	var unimplemented, unresolved, unstamped []string
	for value, names := range groups {
		switch value {
		case unresolvedVersion:
			unresolved = names
		case unimplementedRevision:
			unimplemented = names
		case "":
			// Every RevisionBinaries member is reliably stampable now that
			// orchard-tui carries a build stamp (#807), so an empty revision is
			// a broken/unstamped build — the exact skew-hiding shape the check
			// exists to catch. It FAILs and is named, never read as a match.
			unstamped = names
		default:
			realGroups[value] = names
		}
	}

	suffix := excludedSuffix()

	if len(realGroups) == 0 && len(unimplemented) == 0 && len(unstamped) == 0 {
		return checkResult{ID: "suite-revisions", Status: statusFail,
			Detail: "none of the suite binaries could report a revision" + suffix,
			Remedy: "reinstall orchard so its binaries are on $PATH or beside orchard-shell"}
	}
	if len(realGroups) > 1 || len(unresolved) > 0 || len(unstamped) > 0 {
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
	// Exactly one real revision group remains (len(realGroups) == 1, guarded by
	// the branches above; empties FAIL, so it is non-empty), so this pass reads
	// its single key and returns.
	var rev string
	for rev = range realGroups {
	}
	return checkResult{ID: "suite-revisions", Status: statusPass,
		Detail: fmt.Sprintf("all %d suite binaries built from revision %s%s", len(revisions), rev, suffix)}
}

// excludedSuffix names the SuiteBinaries the revision check cannot cover — the
// Rust binaries. It lists release.UnstampedBinaries directly (the single source
// of the exclusion), so doctor's suffix and the covered set cannot drift apart.
func excludedSuffix() string {
	if len(release.UnstampedBinaries) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s excluded: Rust, not stamped)", strings.Join(release.UnstampedBinaries, ", "))
}
