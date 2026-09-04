package daemon

// Integration test for the periodic git-provider reconcile (issue #701
// D2): a repo discovered AFTER boot — via repodiscovery's pull-only TTL
// cache, which emits no config event — must gain its worktrees within
// one TTL. The reconciler re-lists on a tick and calls ApplyProjects.
//
// Uses a fake lister whose result set changes at runtime, a real
// gitprovider.Provider, and a short tick interval so the test is fast.

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	configprovider "github.com/drewdrewthis/orchardist/internal/server/providers/config"
	gitprovider "github.com/drewdrewthis/orchardist/internal/server/providers/git"
)

// fakeRepoLister is a gitProjectLister whose result set can be swapped
// at runtime to simulate post-boot discovery.
type fakeRepoLister struct {
	mu    sync.Mutex
	repos []configprovider.Repo
}

func (f *fakeRepoLister) set(repos []configprovider.Repo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = repos
}

func (f *fakeRepoLister) List(_ context.Context) ([]configprovider.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]configprovider.Repo, len(f.repos))
	copy(out, f.repos)
	return out, nil
}

func repoRow(t *testing.T, slug string) configprovider.Repo {
	t.Helper()
	return configprovider.Repo{ID: configprovider.RepoID(slug), Slug: slug, Path: setupHotReloadRepo(t)}
}

// TestGitPeriodicReconcile_DiscoversPostBootRepo covers #701 D2 (a): a
// repo added to the lister after boot is registered with the git
// provider within one tick, without a config edit — and the survivor's
// watcher is not respawned.
func TestGitPeriodicReconcile_DiscoversPostBootRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; reconcile test needs real git repos")
	}

	alpha := repoRow(t, "team/alpha")
	beta := repoRow(t, "team/beta")

	lister := &fakeRepoLister{}
	lister.set([]configprovider.Repo{alpha})

	gp := gitprovider.NewProvider(nil)
	t.Cleanup(gp.Stop)
	if err := gp.AddProject(gitprovider.Project{ID: string(alpha.ID), Dir: alpha.Path}); err != nil {
		t.Fatalf("AddProject alpha: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := newGitPeriodicReconciler(lister, gp, 20*time.Millisecond, nil)
	r.start(ctx)
	t.Cleanup(func() { cancel(); r.close() })

	// beta appears post-boot via discovery.
	lister.set([]configprovider.Repo{alpha, beta})

	waitUntil(t, "beta discovered", func() bool {
		return gp.HasProject("team/beta") && gp.HasProject("team/alpha")
	})
	if got := gp.SpawnCount("team/alpha"); got != 1 {
		t.Fatalf("SpawnCount(alpha) = %d; want 1 (survivor not respawned)", got)
	}
}

// TestGitPeriodicReconcile_NoChurnWhenUnchanged covers #701 D2 (b): once
// the discovered set is stable, further ticks do not re-invoke
// ApplyProjects and do not respawn watchers.
func TestGitPeriodicReconcile_NoChurnWhenUnchanged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; reconcile test needs real git repos")
	}

	alpha := repoRow(t, "team/alpha")
	lister := &fakeRepoLister{}
	lister.set([]configprovider.Repo{alpha})

	gp := gitprovider.NewProvider(nil)
	t.Cleanup(gp.Stop)
	if err := gp.AddProject(gitprovider.Project{ID: string(alpha.ID), Dir: alpha.Path}); err != nil {
		t.Fatalf("AddProject alpha: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := newGitPeriodicReconciler(lister, gp, 10*time.Millisecond, nil)
	r.start(ctx)
	t.Cleanup(func() { cancel(); r.close() })

	// First tick re-converges the (unchanged-from-seed) set exactly once.
	waitUntil(t, "first apply", func() bool { return r.ApplyCount() >= 1 })
	stable := r.ApplyCount()

	// Let several more ticks elapse with no change to the set.
	time.Sleep(80 * time.Millisecond)

	if got := r.ApplyCount(); got != stable {
		t.Fatalf("ApplyCount grew on unchanged set: was %d, now %d", stable, got)
	}
	if got := gp.SpawnCount("team/alpha"); got != 1 {
		t.Fatalf("SpawnCount(alpha) = %d; want 1 (no churn)", got)
	}
}

// TestGitPeriodicReconcile_StopsOnCancel covers #701 D2 (c): cancelling
// the context stops the ticker goroutine (close returns promptly).
func TestGitPeriodicReconcile_StopsOnCancel(t *testing.T) {
	lister := &fakeRepoLister{}
	gp := gitprovider.NewProvider(nil)
	t.Cleanup(gp.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	r := newGitPeriodicReconciler(lister, gp, 10*time.Millisecond, nil)
	r.start(ctx)

	cancel()
	done := make(chan struct{})
	go func() { r.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconciler did not stop within 2s of ctx cancel")
	}
}
