package daemon

// Integration test for git provider hot-reload via the daemon-side
// subscriber (issue #571). The wiring exercised here is identical to
// runStart: a real configprovider.JSONFileAdapter + Provider + Subscribe,
// the gitConfigSubscriber bridge, and a real gitprovider.Provider.
//
// We rewrite the config file on disk, expect the subscriber to fire
// ApplyProjects, and assert the git provider's HasProject set converged.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	configprovider "github.com/drewdrewthis/orchardist/internal/server/providers/config"
	gitprovider "github.com/drewdrewthis/orchardist/internal/server/providers/git"
)

const hotReloadDeadline = 5 * time.Second

// TestGitHotReload_RemovesProjectOnConfigEdit walks the full chain:
//
//  1. Write config.json containing two repos (alpha + beta).
//  2. Start configprovider.Provider; build git provider against List();
//     AddProject for each.
//  3. Spin up gitConfigSubscriber on configProvider.Subscribe(ctx).
//  4. Rewrite config.json without beta. Expect:
//     - applyCount grows to >= 1
//     - HasProject(beta) flips to false within the deadline
//     - HasProject(alpha) remains true (and SpawnCount stays 1 — the
//     watcher was NOT restarted on the survivor).
//  5. Rewrite config.json adding gamma. Expect HasProject(gamma) flips
//     to true and HasProject(alpha) is still unchanged.
func TestGitHotReload_RemovesProjectOnConfigEdit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; hot-reload test needs real git repos")
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	alphaRepo := setupHotReloadRepo(t)
	betaRepo := setupHotReloadRepo(t)
	gammaRepo := setupHotReloadRepo(t)

	writeHotReloadConfig(t, cfgPath, []configprovider.RepoRow{
		{Slug: "team/alpha", Path: alphaRepo},
		{Slug: "team/beta", Path: betaRepo},
	})

	logger := slog.Default()
	adapter := configprovider.NewJSONFileAdapter(cfgPath, logger)
	cfgProvider := configprovider.NewProvider(adapter, logger)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := cfgProvider.Start(ctx); err != nil {
		t.Fatalf("config provider start: %v", err)
	}
	t.Cleanup(func() { _ = cfgProvider.Stop() })

	repos, err := cfgProvider.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("initial List len = %d; want 2", len(repos))
	}

	gp := gitprovider.NewProvider(logger)
	t.Cleanup(gp.Stop)
	for _, r := range repos {
		if err := gp.AddProject(gitprovider.Project{ID: string(r.ID), Dir: r.Path}); err != nil {
			t.Fatalf("AddProject %q: %v", r.ID, err)
		}
	}
	if !gp.HasProject("team/alpha") || !gp.HasProject("team/beta") {
		t.Fatalf("initial git provider state missing projects; alpha=%v beta=%v",
			gp.HasProject("team/alpha"), gp.HasProject("team/beta"))
	}

	// interval 0 disables the periodic tick so this test isolates the
	// config-event trigger; the tick path is covered in
	// git_reconcile_tick_test.go.
	subscriber := newGitConfigSubscriber(cfgProvider, gp, 0, logger)
	subscriber.start(ctx, cfgProvider.Subscribe(ctx))
	t.Cleanup(func() {
		cancel()
		subscriber.close()
	})

	// Step 4: drop beta.
	startApply := subscriber.ApplyCount()
	writeHotReloadConfig(t, cfgPath, []configprovider.RepoRow{
		{Slug: "team/alpha", Path: alphaRepo},
	})

	waitUntil(t, "beta removed", func() bool {
		return !gp.HasProject("team/beta") && gp.HasProject("team/alpha")
	})

	if got := subscriber.ApplyCount(); got <= startApply {
		t.Fatalf("ApplyCount did not grow after first edit: before=%d after=%d",
			startApply, got)
	}
	if got := gp.SpawnCount("team/alpha"); got != 1 {
		t.Fatalf("SpawnCount(alpha) = %d after first edit; want 1 (survivor not respawned)", got)
	}

	// Step 5: add gamma.
	writeHotReloadConfig(t, cfgPath, []configprovider.RepoRow{
		{Slug: "team/alpha", Path: alphaRepo},
		{Slug: "team/gamma", Path: gammaRepo},
	})

	waitUntil(t, "gamma added", func() bool {
		return gp.HasProject("team/gamma") && gp.HasProject("team/alpha") && !gp.HasProject("team/beta")
	})
	if got := gp.SpawnCount("team/alpha"); got != 1 {
		t.Fatalf("SpawnCount(alpha) = %d after second edit; want 1 (survivor not respawned)", got)
	}
}

// writeHotReloadConfig writes f atomically via rename so the fsnotify
// event semantics match production (orchard config add-repo also
// rename-replaces). Empty Slug → ID derived inside the provider; we
// pass an explicit Slug per row to keep IDs predictable.
func writeHotReloadConfig(t *testing.T, path string, repos []configprovider.RepoRow) {
	t.Helper()
	f := configprovider.File{Version: 1, Repos: repos}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
}

// setupHotReloadRepo creates a minimal git repo with one commit. We
// need real .git dirs so the git provider's per-project watcher can
// install fsnotify on `.git/HEAD`.
func setupHotReloadRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "hot-reload@example.com"},
		{"config", "user.name", "hot-reload"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return repo
}

// waitUntil polls cond until it returns true or the deadline elapses.
// Used to bridge "config rewrite → fsnotify burst → debounce →
// ApplyProjects" without a hard sleep.
func waitUntil(t *testing.T, label string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(hotReloadDeadline)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("waitUntil(%s) timed out after %s", label, hotReloadDeadline)
}
