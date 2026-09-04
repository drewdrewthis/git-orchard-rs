package daemon

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	gitprovider "github.com/drewdrewthis/orchardist/internal/server/providers/git"
	"github.com/drewdrewthis/orchardist/internal/server/providers/repodiscovery"
)

// gitPeriodicReconciler re-converges the git provider's project set on a
// fixed cadence so repos discovered AFTER boot gain their worktrees
// without a config.json edit (issue #701 D2).
//
// The config hot-reload path (gitConfigSubscriber) only fires on
// ~/.orchard/config.json fsnotify events. Repo discovery, however, is a
// pull-only TTL cache (repodiscovery.Provider) with no emit channel: a
// repo surfaced by a tmux pane cwd after boot never triggers a config
// event, so ApplyProjects was never re-run for it and ListByProject
// returned nil forever.
//
// This reconciler ticks at the repodiscovery TTL cadence, re-lists via
// the same lister buildGitProvider/gitConfigSubscriber use, and calls
// ApplyProjects — reusing the exact snapshot/diff machinery. ApplyProjects
// is idempotent (it diffs and only Adds/Removes deltas), and this driver
// additionally fingerprints the project set so an unchanged set is not
// re-applied at all: no churn when nothing changed.
type gitPeriodicReconciler struct {
	lister      gitProjectLister
	gitProvider *gitprovider.Provider
	interval    time.Duration
	logger      *slog.Logger
	doneCh      chan struct{}

	// applyCount is incremented immediately before each ApplyProjects
	// call. Exposed via ApplyCount() as a test seam. It rises only when
	// the discovered set actually changed.
	applyCount atomic.Int64

	// lastKey fingerprints the last-applied project set so an unchanged
	// tick is skipped. Written only from the run goroutine.
	lastKey string
}

func newGitPeriodicReconciler(lister gitProjectLister, gitProvider *gitprovider.Provider, interval time.Duration, logger *slog.Logger) *gitPeriodicReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = repodiscovery.DefaultTTL
	}
	return &gitPeriodicReconciler{
		lister:      lister,
		gitProvider: gitProvider,
		interval:    interval,
		logger:      logger,
		doneCh:      make(chan struct{}),
	}
}

// start spawns the ticker goroutine. Returns when ctx is cancelled.
// Not idempotent — call once.
func (r *gitPeriodicReconciler) start(ctx context.Context) {
	go r.run(ctx)
}

func (r *gitPeriodicReconciler) run(ctx context.Context) {
	defer close(r.doneCh)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce re-lists the discovered repos and, when the set differs
// from the last-applied fingerprint, re-converges the git provider.
func (r *gitPeriodicReconciler) reconcileOnce(ctx context.Context) {
	projs, err := r.snapshotProjects(ctx)
	if err != nil {
		r.logger.Warn("git periodic reconcile: lister failed; keeping existing projects", "err", err)
		return
	}
	key := fingerprintProjects(projs)
	if key == r.lastKey {
		return
	}
	r.lastKey = key
	r.applyCount.Add(1)
	if err := r.gitProvider.ApplyProjects(projs); err != nil {
		r.logger.Warn("git periodic reconcile: ApplyProjects error", "err", err)
	}
}

// snapshotProjects lists repos and maps them into the git provider's
// Project shape — the same derivation buildGitProvider and
// gitConfigSubscriber use, so a repo's project ID is identical across
// the boot seed, the config hot-reload, and this periodic path (the ID
// is the repodiscovery-assigned slug; there is one canonical source).
func (r *gitPeriodicReconciler) snapshotProjects(ctx context.Context) ([]gitprovider.Project, error) {
	repos, err := r.lister.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gitprovider.Project, 0, len(repos))
	for _, repo := range repos {
		out = append(out, gitprovider.Project{ID: string(repo.ID), Dir: repo.Path})
	}
	return out, nil
}

// fingerprintProjects produces an order-independent key for a project
// set so an unchanged discovery result skips ApplyProjects entirely.
func fingerprintProjects(projs []gitprovider.Project) string {
	parts := make([]string, 0, len(projs))
	for _, p := range projs {
		parts = append(parts, p.ID+"\x00"+p.Dir)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x01")
}

// ApplyCount returns how many times ApplyProjects has been invoked by
// this reconciler (i.e. how many ticks saw a changed set). Test seam.
func (r *gitPeriodicReconciler) ApplyCount() int {
	return int(r.applyCount.Load())
}

// close waits for the run goroutine to exit. Callers must cancel the
// context passed to start first.
func (r *gitPeriodicReconciler) close() {
	<-r.doneCh
}
