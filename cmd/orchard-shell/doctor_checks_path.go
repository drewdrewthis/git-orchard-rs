package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// --- PATH --------------------------------------------------------------------

// pathShadow is one suite binary whose $PATH resolution does not match the
// install prefix (self's own directory) — a stale duplicate earlier on
// $PATH silently shadows the real one.
type pathShadow struct {
	name     string // binary name, e.g. "orchard-daemon"
	resolved string // what exec.LookPath actually found
	prefix   string // the sibling path beside self that it shadows
}

func checkPath(env doctorEnv) checkResult {
	return evaluatePath(env.self, env.pathEnv, resolvePathShadows(env.self, env.lookPath))
}

// resolvePathShadows exec.LookPath-resolves every release.SuiteBinaries
// member and flags any whose resolved directory differs from self's own —
// PATH order put a different, same-named binary in front of the one
// actually installed beside self. A binary not found on PATH at all is a
// different problem (untouched by this check).
func resolvePathShadows(self string, lookup pathLookup) []pathShadow {
	if self == "" || lookup == nil {
		return nil
	}
	dir := filepath.Dir(self)
	var shadows []pathShadow
	for _, name := range release.SuiteBinaries {
		resolved, err := lookup(name)
		if err != nil || resolved == "" {
			continue
		}
		if filepath.Dir(resolved) != dir {
			shadows = append(shadows, pathShadow{name: name, resolved: resolved, prefix: filepath.Join(dir, name)})
		}
	}
	return shadows
}

// evaluatePath is checkPath's pure decision, given self's own PATH
// membership and every detected suite-binary shadow (resolvePathShadows).
func evaluatePath(self, pathEnv string, shadows []pathShadow) checkResult {
	if self == "" {
		return checkResult{ID: "path", Status: statusWarn, Detail: "could not resolve this binary's own path"}
	}
	dir := filepath.Dir(self)
	onPath := false
	for _, p := range filepath.SplitList(pathEnv) {
		if p == dir {
			onPath = true
			break
		}
	}
	if !onPath {
		return checkResult{ID: "path", Status: statusFail,
			Detail: dir + " is not on $PATH",
			Remedy: fmt.Sprintf("add it to $PATH, e.g.: export PATH=%s:$PATH", dir)}
	}
	if len(shadows) > 0 {
		parts := make([]string, len(shadows))
		for i, s := range shadows {
			parts[i] = fmt.Sprintf("%s: %s shadows %s", s.name, s.resolved, s.prefix)
		}
		return checkResult{ID: "path", Status: statusWarn,
			Detail: dir + " is on $PATH, but " + strings.Join(parts, "; "),
			Remedy: "reorder PATH or remove the stale binary"}
	}
	return checkResult{ID: "path", Status: statusPass, Detail: dir + " is on $PATH"}
}
