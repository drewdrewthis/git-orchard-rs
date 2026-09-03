package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestComputeAheadBehind_NoUpstream verifies that a branch without a
// configured upstream returns (nil, nil) — #483 ahead/behind enrichment
// must be silent on failure, never blocking the worktree read.
func TestComputeAheadBehind_NoUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := initRepoWithCommit(t)
	ahead, behind := computeAheadBehind(repo, "main", false)
	if ahead != nil || behind != nil {
		t.Fatalf("no-upstream branch: got ahead=%v behind=%v, want nil/nil", ahead, behind)
	}
}

// TestComputeAheadBehind_DetachedOrBare verifies the cheap pre-checks.
func TestComputeAheadBehind_DetachedOrBare(t *testing.T) {
	ahead, behind := computeAheadBehind("/nonexistent", "", false)
	if ahead != nil || behind != nil {
		t.Errorf("detached: got ahead=%v behind=%v, want nil/nil", ahead, behind)
	}
	ahead, behind = computeAheadBehind("/nonexistent", "main", true)
	if ahead != nil || behind != nil {
		t.Errorf("bare: got ahead=%v behind=%v, want nil/nil", ahead, behind)
	}
}

// TestComputeAheadBehind_WithUpstream exercises the happy path against a
// real git repo: clone-and-diverge sets up known ahead/behind counts and
// the helper must parse them in the documented order (ahead, behind).
func TestComputeAheadBehind_WithUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	origin := initRepoWithCommit(t)
	// Add two more commits to origin so the clone can fall behind by 2.
	commitFile(t, origin, "second.txt", "two")
	commitFile(t, origin, "third.txt", "three")

	cloneDir := t.TempDir()
	runGitT(t, "", "clone", "--branch", "main", origin, cloneDir)

	// Roll the clone back two commits so it is BEHIND origin by 2.
	runGitT(t, cloneDir, "reset", "--hard", "HEAD~2")
	// Add one local commit to be AHEAD by 1.
	commitFile(t, cloneDir, "local.txt", "local")

	ahead, behind := computeAheadBehind(cloneDir, "main", false)
	if ahead == nil || behind == nil {
		t.Fatalf("ahead/behind nil: ahead=%v behind=%v", ahead, behind)
	}
	if *ahead != 1 {
		t.Errorf("ahead: got %d, want 1", *ahead)
	}
	if *behind != 2 {
		t.Errorf("behind: got %d, want 2", *behind)
	}
}

// initRepoWithCommit creates a real git repo at a temp dir with one
// initial commit on main. Returns the worktree path.
func initRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitT(t, "", "init", "--initial-branch=main", "-q", dir)
	commitFile(t, dir, "README.md", "hello")
	return dir
}

func commitFile(t *testing.T, repo, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	runGitT(t, repo, "add", name)
	runGitT(t, repo, "commit", "-q", "-m", "add "+name)
}

// gitIdentityArgs ride on every git invocation rather than being written into
// one repo's local config. A CI runner has no global user.name/user.email, and
// `git clone` does not copy the source repo's local config — so seeding only
// the origin left commits in the clone failing with "Author identity unknown".
// Passing the identity per-command makes every helper hermetic regardless of
// which repo it targets or what the ambient ~/.gitconfig holds.
var gitIdentityArgs = []string{
	"-c", "user.email=test@example.com",
	"-c", "user.name=Test",
	"-c", "commit.gpgsign=false",
}

func runGitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", append(gitIdentityArgs, args...)...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
