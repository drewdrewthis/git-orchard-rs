package main

import (
	"context"
	"fmt"
	"strings"
)

// checkRevisions is #787's defense inside the toolchain: two suite binaries can
// report the same --version "dev" yet come from different commits — the exact
// shape of the stale prototype launcher that silently broke sidebar clicks.
// vcs.revision distinguishes them where the semver check (checkSuiteVersions)
// cannot.
func checkRevisions(ctx context.Context, env doctorEnv) checkResult {
	return evaluateRevisions(env.selfRevision, resolveSidebarRevision(ctx, env))
}

// resolveSidebarRevision execs `orchard-sidebar --revision`. orchard-shell's
// own revision is read in-process (env.selfRevision) for the same reason
// resolveOneVersion special-cases it: self-exec would just rerun this binary.
func resolveSidebarRevision(ctx context.Context, env doctorEnv) string {
	path := resolveBinary(env.self, "orchard-sidebar")
	if path == "" {
		return ""
	}
	out, err := env.run(ctx, path, "--revision")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// evaluateRevisions is the pure decision: OK when the two revisions are equal,
// FAIL naming both when they differ. An empty revision (a binary that could not
// be resolved, or a build with no VCS stamp) is a value like any other — two
// empties match (nothing to distinguish), one empty against a real revision is
// a mismatch worth failing.
func evaluateRevisions(shell, sidebar string) checkResult {
	if shell == sidebar {
		detail := "orchard-shell and orchard-sidebar built from revision " + shell
		if shell == "" {
			detail = "orchard-shell and orchard-sidebar carry no VCS revision (an unstamped build)"
		}
		return checkResult{ID: "suite-revision", Status: statusPass, Detail: detail}
	}
	return checkResult{ID: "suite-revision", Status: statusFail,
		Detail: fmt.Sprintf("orchard-shell (%s) and orchard-sidebar (%s) built from different revisions",
			revLabel(shell), revLabel(sidebar)),
		Remedy: "reinstall or rebuild both from the same checkout so their revisions match"}
}

// revLabel renders an unresolved revision as "unknown" rather than an empty
// pair of parens, so the mismatch detail still names which side is missing.
func revLabel(r string) string {
	if r == "" {
		return "unknown"
	}
	return r
}
