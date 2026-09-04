package daemon

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	configprovider "github.com/drewdrewthis/orchardist/internal/server/providers/config"
	gitprovider "github.com/drewdrewthis/orchardist/internal/server/providers/git"
)

// gitConfigSubscriber converges gitprovider.Provider onto the current
// discovered project set via ApplyProjects. It has TWO triggers feeding
// one converger:
//
//   - config invalidation events (issue #571): an add/remove of a repo in
//     ~/.orchard/config.json applies without a daemon restart. Coalescing
//     comes "for free" from configprovider.Provider.run, which emits one
//     event per fsnotify burst.
//   - a periodic tick (issue #701 D2): repodiscovery is a pull-only TTL
//     cache with no emit channel, so a repo surfaced AFTER boot (e.g. via a
//     tmux pane cwd) never triggers a config event. Ticking at the
//     discovery TTL cadence re-lists and re-converges so such repos gain
//     their worktrees within one TTL.
//
// Both triggers call the same snapshot + ApplyProjects path.
// ApplyProjects is idempotent — it diffs the incoming set against the
// current one by ID+Dir and only Adds/Removes the delta, leaving
// survivors' watcher goroutines untouched (see Provider.ApplyProjects) —
// so an unchanged set produces no churn even when a tick re-applies it.
type gitConfigSubscriber struct {
	lister      gitProjectLister
	gitProvider *gitprovider.Provider
	interval    time.Duration // <= 0 disables the periodic tick
	logger      *slog.Logger
	doneCh      chan struct{}

	// applyCount is incremented immediately before each ApplyProjects
	// call. Exposed via ApplyCount() as a test seam.
	applyCount atomic.Int64
}

// newGitConfigSubscriber builds the converger. interval sets the periodic
// tick cadence; pass repodiscovery.DefaultTTL in production, or <= 0 to
// disable the tick (config events only).
func newGitConfigSubscriber(lister gitProjectLister, gitProvider *gitprovider.Provider, interval time.Duration, logger *slog.Logger) *gitConfigSubscriber {
	if logger == nil {
		logger = slog.Default()
	}
	return &gitConfigSubscriber{
		lister:      lister,
		gitProvider: gitProvider,
		interval:    interval,
		logger:      logger,
		doneCh:      make(chan struct{}),
	}
}

// start spawns the converger goroutine. It converges on every config
// invalidation read from events and, when interval > 0, on every tick.
// Returns when ctx is cancelled or the events channel closes. events may
// be nil (tick-only converger).
//
// start is not idempotent — call it once per subscriber.
func (s *gitConfigSubscriber) start(ctx context.Context, events <-chan configprovider.InvalidationEvent) {
	go s.run(ctx, events)
}

func (s *gitConfigSubscriber) run(ctx context.Context, events <-chan configprovider.InvalidationEvent) {
	defer close(s.doneCh)

	// A nil channel blocks forever in select, so a disabled tick simply
	// never fires without special-casing the loop.
	var tickC <-chan time.Time
	if s.interval > 0 {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		tickC = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			s.converge(ctx, "config reload")
		case <-tickC:
			s.converge(ctx, "periodic tick")
		}
	}
}

// converge re-lists the discovered repos and applies them to the git
// provider. Errors are logged and swallowed: a failed list keeps the
// existing project set rather than emptying the fleet.
func (s *gitConfigSubscriber) converge(ctx context.Context, trigger string) {
	projs, err := s.snapshotProjects(ctx)
	if err != nil {
		s.logger.Warn("git converge: lister failed; keeping existing projects",
			"trigger", trigger, "err", err)
		return
	}
	s.applyCount.Add(1)
	if err := s.gitProvider.ApplyProjects(projs); err != nil {
		s.logger.Warn("git converge: ApplyProjects error",
			"trigger", trigger, "err", err)
	}
}

// snapshotProjects calls the lister and converts the result into the
// git provider's Project shape. The ID is the repodiscovery-assigned
// slug — the same derivation buildGitProvider uses — so a project's ID
// is identical across the boot seed and both converge triggers.
func (s *gitConfigSubscriber) snapshotProjects(ctx context.Context) ([]gitprovider.Project, error) {
	repos, err := s.lister.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gitprovider.Project, 0, len(repos))
	for _, r := range repos {
		out = append(out, gitprovider.Project{ID: string(r.ID), Dir: r.Path})
	}
	return out, nil
}

// ApplyCount returns the number of times ApplyProjects has been called.
// Tests rely on this to assert convergence fired. Test seam.
func (s *gitConfigSubscriber) ApplyCount() int {
	return int(s.applyCount.Load())
}

// close waits for the run goroutine to exit. Callers must cancel the
// context they passed to start before calling close — start exits when
// the context is cancelled OR the events channel closes.
func (s *gitConfigSubscriber) close() {
	<-s.doneCh
}
