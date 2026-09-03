package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// unresolvedVersion is the group key for a suite binary that could not be
// found, or would not report a version — its own distinct group, so a
// missing binary fails the check and is named exactly like a mismatch.
const unresolvedVersion = "(not found)"

// binaryVersion is one suite member's resolved --version output.
type binaryVersion struct {
	name    string
	version string
}

// checkSuiteVersions resolves and evaluates every release.SuiteBinaries
// member in one check.
func checkSuiteVersions(ctx context.Context, env doctorEnv) checkResult {
	return evaluateSuiteVersions(resolveSuiteVersions(ctx, env))
}

// resolveSuiteVersions is the impure half: sibling-then-PATH resolution and
// a --version exec per binary (see resolveBinary, discover.go).
func resolveSuiteVersions(ctx context.Context, env doctorEnv) []binaryVersion {
	out := make([]binaryVersion, 0, len(release.SuiteBinaries))
	for _, name := range release.SuiteBinaries {
		out = append(out, binaryVersion{name: name, version: resolveOneVersion(ctx, env, name)})
	}
	return out
}

// resolveOneVersion resolves and runs a single suite binary's --version.
//
// orchard-shell is special-cased to the in-process version: self-exec'ing it
// would just run this same binary again, and --version is a flag main.go's
// run() already answers without a subprocess.
func resolveOneVersion(ctx context.Context, env doctorEnv, name string) string {
	if name == "orchard-shell" {
		return env.selfVersion
	}
	path := resolveBinary(env.self, name)
	if path == "" {
		return unresolvedVersion
	}
	out, err := env.run(ctx, path, "--version")
	if err != nil || out == "" {
		return unresolvedVersion
	}
	return lastToken(out)
}

// lastToken returns --version output's last whitespace-separated field,
// where every suite binary places its bare semver (or "dev").
func lastToken(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return unresolvedVersion
	}
	return fields[len(fields)-1]
}

// evaluateSuiteVersions is the pure half: given every binary's resolved
// version, decide pass/warn/fail.
//
// Grouping by exact string equality is deliberate, not release.Compare:
// two DIFFERENT non-semver placeholders (unresolvedVersion and some other
// garbage --version prints) must never compare equal, which a semver-aware
// comparison could get wrong by treating both as equally invalid.
func evaluateSuiteVersions(versions []binaryVersion) checkResult {
	groups := map[string][]string{}
	for _, v := range versions {
		groups[v.version] = append(groups[v.version], v.name)
	}
	if len(groups) == 1 {
		for version := range groups {
			switch version {
			case unresolvedVersion:
				return checkResult{ID: "suite-versions", Status: statusFail,
					Detail: "none of the suite binaries could be resolved",
					Remedy: "reinstall orchard so its binaries are on $PATH or beside orchard-shell"}
			case release.DevVersion:
				return checkResult{ID: "suite-versions", Status: statusWarn,
					Detail: fmt.Sprintf("all %d suite binaries report version %q (an unreleased build)", len(versions), version)}
			default:
				return checkResult{ID: "suite-versions", Status: statusPass,
					Detail: fmt.Sprintf("all %d suite binaries report version %s", len(versions), version)}
			}
		}
	}

	versionList := make([]string, 0, len(groups))
	for version := range groups {
		versionList = append(versionList, version)
	}
	sort.Strings(versionList)
	parts := make([]string, 0, len(versionList))
	for _, version := range versionList {
		label := version
		if label == unresolvedVersion {
			label = "not found"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", label, strings.Join(groups[version], ", ")))
	}
	return checkResult{ID: "suite-versions", Status: statusFail,
		Detail: "suite binaries report mismatched versions: " + strings.Join(parts, "; "),
		Remedy: "reinstall or rebuild so every orchard binary comes from the same release"}
}
