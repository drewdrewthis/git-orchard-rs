package gh

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/drewdrewthis/orchardist/internal/server/providers/git"
)

// ReadOriginURL reads `.git/config` for a working tree and returns the
// `origin` remote's URL. Mirrors the git provider's policy of reading
// the on-disk layout directly rather than shelling out to git.
//
// Returns errNoOriginRemote when the file exists but no `[remote
// "origin"]` block has a `url =` entry; errNoGitDir when there is no
// `.git` at the path; other I/O errors propagate.
func ReadOriginURL(workdir string) (string, error) {
	gitDir, err := ResolveGitDirForWorktree(workdir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errNoGitDir
		}
		return "", err
	}
	cfgPath := filepath.Join(gitDir, "config")
	file, err := os.Open(cfgPath) //nolint:gosec // trusted internal path
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errNoOriginRemote
		}
		return "", err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	inOrigin := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inOrigin = strings.EqualFold(line, `[remote "origin"]`)
			continue
		}
		if !inOrigin {
			continue
		}
		// Lines look like `url = https://...` or `url=git@...`.
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if strings.EqualFold(key, "url") && val != "" {
			return val, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errNoOriginRemote
}

// ParseGitHubURL extracts the owner and repo name from a GitHub
// remote URL. Handles the three common forms:
//
//   - https://github.com/owner/repo.git
//   - git@github.com:owner/repo.git
//   - ssh://git@github.com/owner/repo.git
//
// Returns ok=false for any URL that is not GitHub (or GHES — out of
// scope for v1). The `.git` suffix is stripped.
func ParseGitHubURL(raw string) (owner, name string, ok bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", false
	}

	// SSH shorthand: git@github.com:owner/repo[.git]
	if strings.HasPrefix(s, "git@github.com:") {
		s = strings.TrimPrefix(s, "git@github.com:")
		return splitOwnerName(stripGit(s))
	}

	// ssh:// or https:// — strip scheme + host.
	for _, prefix := range []string{
		"ssh://git@github.com/",
		"https://github.com/",
		"http://github.com/",
		"git://github.com/",
	} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			return splitOwnerName(stripGit(s))
		}
	}
	return "", "", false
}

func stripGit(s string) string {
	return strings.TrimSuffix(strings.TrimSpace(s), ".git")
}

func splitOwnerName(s string) (string, string, bool) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	// owner/name should not have further slashes; reject "owner/sub/name".
	if strings.Contains(parts[1], "/") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ResolveGitDirForWorktree resolves the git directory that owns the
// checkout at workdir, then follows the linked-worktree `commondir` so
// callers (notably ReadOriginURL and the default-branch resolver) read
// `config`/`HEAD` from the project root rather than the per-worktree
// directory.
//
// Base layout resolution — `.git` directory, `.git` gitfile, or a bare
// root — is delegated to git.ResolveGitDirInfo so this package no longer
// duplicates it. That delegation is also the bare-repo fix: the old
// hand-rolled logic here errored on a bare root (no `.git` entry), which
// now surfaces from D1's enumeration and is called for every worktree via
// the default-branch resolver (#701 D1/#4).
//
// For a linked worktree, git.ResolveGitDirInfo returns
// `<project>/.git/worktrees/<name>/`, which holds a `commondir` file whose
// content is a relative or absolute path back to the project's git dir.
func ResolveGitDirForWorktree(workdir string) (string, error) {
	gd, _, err := git.ResolveGitDirInfo(workdir)
	if err != nil {
		return "", err
	}

	// Linked-worktree case: follow `commondir` (if present) to the
	// project's git dir. A bare root or a normal repo has no commondir, so
	// gd is returned unchanged.
	commondirPath := filepath.Join(gd, "commondir")
	if raw, cdErr := os.ReadFile(commondirPath); cdErr == nil { //nolint:gosec
		cd := strings.TrimSpace(string(raw))
		if cd != "" {
			if !filepath.IsAbs(cd) {
				cd = filepath.Join(gd, cd)
			}
			gd = filepath.Clean(cd)
		}
	}

	return gd, nil
}
