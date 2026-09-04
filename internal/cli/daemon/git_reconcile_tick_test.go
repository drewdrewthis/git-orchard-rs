package daemon

// Tick-path coverage for the merged git converger (gitConfigSubscriber,
// issue #701 D2): a repo surfaced AFTER boot via repodiscovery's pull-only
// TTL cache — which emits no config event — must gain its worktrees on a
// periodic tick, without a config edit. Uses a fake lister whose result
// set changes at runtime, a real gitprovider.Provider, and a short tick
// interval so the test is fast. The config-event trigger is exercised
// separately in git_hot_reload_test.go.

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

// TestGitConverger_TickDiscoversPostBootRepo covers #701 D2: a repo added
// to the lister after boot is registered with the git provider on a tick,
// without a config event — and the survivor's watcher is not respawned.
func TestGitConverger_TickDiscoversPostBootRepo(t *testing.T) {
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
	s := newGitConfigSubscriber(lister, gp, 20*time.Millisecond, nil)
	// Tick-only converger: pass a nil events channel.
	s.start(ctx, nil)
	t.Cleanup(func() { cancel(); s.close() })

	// beta appears post-boot via discovery.
	lister.set([]configprovider.Repo{alpha, beta})

	waitUntil(t, "beta discovered", func() bool {
		return gp.HasProject("team/beta") && gp.HasProject("team/alpha")
	})
	if got := gp.SpawnCount("team/alpha"); got != 1 {
		t.Fatalf("SpawnCount(alpha) = %d; want 1 (survivor not respawned)", got)
	}
}

// TestGitConverger_StopsOnCancel covers #701 D2: cancelling the context
// stops the ticker goroutine (close returns promptly).
func TestGitConverger_StopsOnCancel(t *testing.T) {
	lister := &fakeRepoLister{}
	gp := gitprovider.NewProvider(nil)
	t.Cleanup(gp.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	s := newGitConfigSubscriber(lister, gp, 10*time.Millisecond, nil)
	s.start(ctx, nil)

	cancel()
	done := make(chan struct{})
	go func() { s.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("converger did not stop within 2s of ctx cancel")
	}
}
