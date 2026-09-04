package git

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGitBare shells out to git and fails the test on non-zero exit.
// Shell-outs are permitted for fixture setup only.
func runGitBare(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
}

// makeSourceRepo builds a normal repo with one commit and returns its path.
func makeSourceRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGitBare(t, repo, "init", "-b", "main")
	runGitBare(t, repo, "config", "user.email", "i701@example.com")
	runGitBare(t, repo, "config", "user.name", "i701")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGitBare(t, repo, "add", "README.md")
	runGitBare(t, repo, "commit", "-m", "initial")
	return repo
}

// requireGit skips when git is unavailable.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; bare fixtures require real git")
	}
}

// TestResolveGitDirInfo_Bare covers #701 D1 (a): a bare repository's
// working dir IS its gitdir and is flagged bare.
func TestResolveGitDirInfo_Bare(t *testing.T) {
	requireGit(t)
	src := makeSourceRepo(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	runGitBare(t, ".", "clone", "--bare", src, bare)

	gd, err := resolveGitDirInfo(bare)
	if err != nil {
		t.Fatalf("resolveGitDirInfo: %v", err)
	}
	if !gd.bare {
		t.Errorf("bare: got false, want true")
	}
	if gd.dir != filepath.Clean(bare) {
		t.Errorf("dir: got %q, want %q", gd.dir, filepath.Clean(bare))
	}
}

// TestFetchAll_BareWithLinkedWorktrees covers #701 D1 (b): a bare clone
// plus 2 linked worktrees enumerates as 3 entries — the bare root
// (Bare=true, Branch="") plus the 2 linked checkouts.
func TestFetchAll_BareWithLinkedWorktrees(t *testing.T) {
	requireGit(t)
	src := makeSourceRepo(t)
	bareParent := t.TempDir()
	bare := filepath.Join(bareParent, "bare.git")
	runGitBare(t, ".", "clone", "--bare", src, bare)

	wt1 := filepath.Join(bareParent, "bare-wt1")
	wt2 := filepath.Join(bare, "worktrees-checkout", "bwt2")
	runGitBare(t, bare, "worktree", "add", "-b", "feat/a", wt1)
	runGitBare(t, bare, "worktree", "add", "-b", "feat/b", wt2)

	a := NewGitWorktreeAdapter(func() []Project {
		return []Project{{ID: "barep", Dir: bare}}
	})
	all, err := a.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("worktree count: got %d, want 3: %+v", len(all), all)
	}

	root, ok := all[NewWorktreeID("barep", MainWorktreeName)]
	if !ok {
		t.Fatalf("bare root entry missing")
	}
	if !root.Bare {
		t.Errorf("bare root Bare: got false, want true")
	}
	if root.Branch != "" {
		t.Errorf("bare root Branch: got %q, want empty", root.Branch)
	}

	// git records a linked worktree's path symlink-resolved (on macOS
	// t.TempDir() is /var/... → /private/var/...), while a path passed
	// straight through — the bare root / main worktree — stays
	// unresolved: production applies filepath.Clean only (CleanPath),
	// consistent with a normal repo's main worktree. So compare linked
	// paths through EvalSymlinks on BOTH sides rather than adding new
	// normalization to production.
	gotPaths := map[string]bool{}
	for _, w := range all {
		gotPaths[resolvePath(t, w.Path)] = true
	}
	for _, want := range []string{wt1, wt2} {
		key := resolvePath(t, want)
		if !gotPaths[key] {
			t.Errorf("linked worktree path %q (resolved %q) missing from %v", want, key, gotPaths)
		}
	}
}

// resolvePath canonicalises a path via EvalSymlinks for cross-side
// comparison; falls back to filepath.Clean when the path cannot be
// resolved.
func resolvePath(t *testing.T, p string) string {
	t.Helper()
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// TestFetchAll_NormalRepoUnchanged covers #701 D1 (c): the .git-path
// regression guard — a non-bare repo still enumerates main (Bare=false)
// plus its linked worktrees.
func TestFetchAll_NormalRepoUnchanged(t *testing.T) {
	requireGit(t)
	repo := makeSourceRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	runGitBare(t, repo, "worktree", "add", "-b", "feature/x", wt)

	a := NewGitWorktreeAdapter(func() []Project {
		return []Project{{ID: "demo", Dir: repo}}
	})
	all, err := a.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("worktree count: got %d, want 2: %+v", len(all), all)
	}
	main, ok := all[NewWorktreeID("demo", MainWorktreeName)]
	if !ok {
		t.Fatalf("main worktree entry missing")
	}
	if main.Bare {
		t.Errorf("main Bare: got true, want false")
	}
	if main.Branch != "main" {
		t.Errorf("main Branch: got %q, want %q", main.Branch, "main")
	}
}
