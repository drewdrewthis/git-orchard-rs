package git

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// gitDirInfo carries a resolved git directory plus whether the project
// directory is itself a bare repository (the directory IS the gitdir).
type gitDirInfo struct {
	dir  string
	bare bool
}

// resolveGitDirInfo resolves the git directory for a project rooted at
// workdir, distinguishing three on-disk layouts:
//
//   - normal repo:  <workdir>/.git is a directory
//   - gitfile:      <workdir>/.git is a regular file "gitdir: <path>"
//     (submodules and some worktree configurations)
//   - bare repo:    no <workdir>/.git; <workdir> itself holds HEAD +
//     objects/ + worktrees/ — the bare repo IS its own gitdir
//
// A bare clone (`git clone --bare`) has no `.git` entry, so the old
// `<workdir>/.git` stat returned fs.ErrNotExist and FetchAll skipped
// the whole project (issue #701 D1). Detecting the bare layout lets the
// bare root and its linked worktrees enumerate normally.
func resolveGitDirInfo(workdir string) (gitDirInfo, error) {
	candidate := filepath.Join(workdir, ".git")
	info, err := os.Stat(candidate)
	if err != nil {
		// No `.git` entry: the directory may itself be a bare repo.
		if errors.Is(err, fs.ErrNotExist) && isBareRepo(workdir) {
			return gitDirInfo{dir: filepath.Clean(workdir), bare: true}, nil
		}
		return gitDirInfo{}, err
	}
	if info.IsDir() {
		return gitDirInfo{dir: candidate}, nil
	}
	// gitfile: a regular file whose contents are `gitdir: <path>`.
	body, err := readTrimmed(candidate)
	if err != nil {
		return gitDirInfo{}, err
	}
	gd := strings.TrimSpace(strings.TrimPrefix(body, "gitdir:"))
	if gd == "" {
		return gitDirInfo{}, fmt.Errorf("malformed gitfile at %q", candidate)
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(workdir, gd)
	}
	return gitDirInfo{dir: filepath.Clean(gd)}, nil
}

// isBareRepo reports whether dir is a bare git repository: it holds a
// HEAD file and an objects/ directory (the minimal markers of a git
// object store) but no `.git` entry. Detection is pure stdlib
// filesystem probing rather than a `git rev-parse --is-bare-repository`
// shellout, matching the adapter's read style (no CLI exec in read hot
// paths).
func isBareRepo(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil || fi.IsDir() {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, "objects")); err != nil || !fi.IsDir() {
		return false
	}
	return true
}
