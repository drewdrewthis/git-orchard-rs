package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// checkRevisions is #787's defense inside the toolchain: two suite binaries can
// report the same --version "dev" yet come from different commits — the exact
// shape of the stale prototype launcher that silently broke sidebar clicks.
// vcs.revision distinguishes them where the semver check (checkSuiteVersions)
// cannot, so it covers the same release.SuiteBinaries set.
func checkRevisions(ctx context.Context, env doctorEnv) checkResult {
	return evaluateRevisions(resolveSuiteRevisions(ctx, env))
}

// resolveSuiteRevisions is the impure half: a revision per suite binary, using
// the same sibling-then-PATH resolution as resolveSuiteVersions.
func resolveSuiteRevisions(ctx context.Context, env doctorEnv) []binaryVersion {
	out := make([]binaryVersion, 0, len(release.SuiteBinaries))
	for _, name := range release.SuiteBinaries {
		out = append(out, binaryVersion{name: name, version: resolveOneRevision(ctx, env, name)})
	}
	return out
}

// resolveOneRevision resolves and runs a single suite binary's --revision.
//
// orchard-shell is read in-process (env.selfRevision) for the same reason
// resolveOneVersion special-cases it: self-exec would just rerun this binary.
// A binary that cannot be found or errors returns unresolvedVersion — its own
// distinct group, so a missing binary FAILs the check rather than reading as an
// unstamped-but-matching build. An empty ("") revision is a value like any
// other: a genuinely unstamped build, distinct from unresolvedVersion.
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
		return unresolvedVersion
	}
	return strings.TrimSpace(out)
}

// evaluateRevisions is the pure decision: OK when every binary agrees, FAIL
// naming each group when they differ. It shares grouping and the mismatch
// listing with the version check (groupSuite, suiteMismatch); only the
// single-group verdicts differ — revisions have no dev-warn, an all-unstamped
// suite passes, and an all-unresolved suite fails.
func evaluateRevisions(revisions []binaryVersion) checkResult {
	groups := groupSuite(revisions)
	if len(groups) == 1 {
		for rev := range groups {
			switch rev {
			case unresolvedVersion:
				return checkResult{ID: "suite-revisions", Status: statusFail,
					Detail: "none of the suite binaries could report a revision",
					Remedy: "reinstall orchard so its binaries are on $PATH or beside orchard-shell"}
			case "":
				return checkResult{ID: "suite-revisions", Status: statusPass,
					Detail: fmt.Sprintf("all %d suite binaries carry no VCS revision (an unstamped build)", len(revisions))}
			default:
				return checkResult{ID: "suite-revisions", Status: statusPass,
					Detail: fmt.Sprintf("all %d suite binaries built from revision %s", len(revisions), rev)}
			}
		}
	}
	return suiteMismatch("suite-revisions", "suite binaries built from different revisions: ",
		"reinstall or rebuild every orchard binary from the same checkout so their revisions match", groups)
}
