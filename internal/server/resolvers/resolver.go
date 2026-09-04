// Package resolvers wires the gqlgen-generated GraphQL surface to the
// providers under internal/server/providers.
package resolvers

import (
	"context"
	"time"

	gitdomain "github.com/drewdrewthis/orchardist/daemon/git"
	"github.com/drewdrewthis/orchardist/internal/server/loaders"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudeaccount"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudeprojects"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudesessions"
	configprovider "github.com/drewdrewthis/orchardist/internal/server/providers/config"
	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
	gitprovider "github.com/drewdrewthis/orchardist/internal/server/providers/git"
	"github.com/drewdrewthis/orchardist/internal/server/providers/host"
	"github.com/drewdrewthis/orchardist/internal/server/providers/hostservice"
	"github.com/drewdrewthis/orchardist/internal/server/providers/peerproxy"
	"github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	"github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// ReposLister is the narrow read-side contract the repo resolver depends on.
type ReposLister interface {
	List(ctx context.Context) ([]configprovider.Repo, error)
}

// Resolver is the dependency-injection root for GraphQL resolvers.
type Resolver struct {
	StartedAt time.Time
	// DaemonVersion is the binary version, injected at boot from the
	// -ldflags -X main.version=<semver> bake. Named DaemonVersion (not
	// Version) to avoid shadowing the Query.Version resolver method on
	// the embedded queryResolver. Defaults to "dev" when no ldflags
	// were used (plain `go build`).
	DaemonVersion  string
	HostProvider   *host.Provider
	ReposProvider  ReposLister
	Git            *gitprovider.Provider
	PS             *ps.Provider
	Tmux           *tmux.Provider
	ClaudeProjects *claudeprojects.Provider
	// ClaudeSessions is the live-REPL registry (~/.claude/sessions/<pid>.json).
	// When nil, sessionUuid resolution degrades to the cwd/nil fallback (#743).
	ClaudeSessions      *claudesessions.Provider
	ClaudeAccount       *claudeaccount.Provider
	HostServiceProvider *hostservice.Provider
	GH                  *gh.Provider
	PeerProxy           *peerproxy.Provider
	LocalEvents         *peerproxy.LocalInvalidator
	// GitMutations is the git-domain mutation resolver. When nil, worktreeRemove
	// and sibling git mutations return an "not configured" error. Wired at boot
	// via WithGitMutations.
	GitMutations *gitdomain.MutationResolver
}

// New constructs a Resolver with the daemon's start time captured.
// DaemonVersion defaults to "dev"; call WithVersion to inject a build-time semver.
func New(startedAt time.Time) *Resolver {
	return &Resolver{StartedAt: startedAt, DaemonVersion: "dev"}
}

// WithVersion injects the daemon binary version into the resolver so
// Query.version can surface it. The value is set by -ldflags at release
// time; callers pass the package-level `version` variable from main.
func (r *Resolver) WithVersion(v string) *Resolver {
	r.DaemonVersion = v
	return r
}

// WithHost wires the host provider.
func (r *Resolver) WithHost(h *host.Provider) *Resolver {
	r.HostProvider = h
	return r
}

// WithRepos wires the repos-listing dependency.
func (r *Resolver) WithRepos(p ReposLister) *Resolver {
	r.ReposProvider = p
	return r
}

// WithGit wires the git provider.
func (r *Resolver) WithGit(g *gitprovider.Provider) *Resolver {
	r.Git = g
	return r
}

// WithPS wires the ps provider.
func (r *Resolver) WithPS(p *ps.Provider) *Resolver {
	r.PS = p
	return r
}

// WithTmux wires the tmux provider.
func (r *Resolver) WithTmux(p *tmux.Provider) *Resolver {
	r.Tmux = p
	return r
}

// WithClaudeProjects wires the claudeprojects provider.
func (r *Resolver) WithClaudeProjects(p *claudeprojects.Provider) *Resolver {
	r.ClaudeProjects = p
	return r
}

// WithClaudeSessions wires the claudesessions provider (live-REPL registry).
func (r *Resolver) WithClaudeSessions(p *claudesessions.Provider) *Resolver {
	r.ClaudeSessions = p
	return r
}

// WithClaudeAccount wires the claudeaccount provider.
func (r *Resolver) WithClaudeAccount(p *claudeaccount.Provider) *Resolver {
	r.ClaudeAccount = p
	return r
}

// WithHostService wires the hostservice provider.
func (r *Resolver) WithHostService(p *hostservice.Provider) *Resolver {
	r.HostServiceProvider = p
	return r
}

// WithGH wires the gh provider.
func (r *Resolver) WithGH(p *gh.Provider) *Resolver {
	r.GH = p
	return r
}

// WithPeerProxy wires the federation provider that backs Host.peers,
// Subscription.peer, and the node-id forwarder behind Query.node.
func (r *Resolver) WithPeerProxy(p *peerproxy.Provider) *Resolver {
	r.PeerProxy = p
	return r
}

// WithLocalEvents wires the local-invalidation broker. When set, the
// `Subscription.peer(host: "*")` resolver streams local events out
// over the federation surface — this is what upstream peers
// subscribe to via their peerproxy adapter.
func (r *Resolver) WithLocalEvents(l *peerproxy.LocalInvalidator) *Resolver {
	r.LocalEvents = l
	return r
}

// WithGitMutations wires the git-domain mutation resolver (worktreeRemove, etc.).
// scriptRoot is the absolute path to the scripts/ directory. When empty,
// gitdomain.NewMutationResolver defaults to "scripts" (relative to cwd).
//
// AC-G2: if r.GH is already wired (WithGH was called before this), a
// ghPRStateAdapter is injected into the resolver so cleanupOne can look up
// the PR merged-state from the daemon's own gh service. Callers in daemon.go
// must order WithGh before WithGitMutations for the injection to fire.
func (r *Resolver) WithGitMutations(scriptRoot string) *Resolver {
	mr := gitdomain.NewMutationResolver(scriptRoot)
	if r.GH != nil {
		mr.WithPRStateLookup(&ghPRStateAdapter{gh: r.GH})
	}
	r.GitMutations = mr
	return r
}

// ghPRStateAdapter adapts *gh.Provider to gitdomain.PRStateLookup.
// It lists all PRs for the repo and returns the state of the most-relevant
// PR whose HeadRef matches the branch (open wins over closed/merged per the
// same precedence used by the Worktree.pr field resolver).
type ghPRStateAdapter struct {
	gh *gh.Provider
}

// PRStateByBranch satisfies gitdomain.PRStateLookup.
func (a *ghPRStateAdapter) PRStateByBranch(ctx context.Context, repoSlug, branch string) (string, error) {
	owner, name, err := gh.SplitRepo(repoSlug)
	if err != nil {
		// Slug not in owner/repo format — no PR lookup possible.
		return "", nil
	}
	prs, err := a.gh.ListPullRequests(ctx, owner, name, gh.PullRequestStateAll)
	if err != nil {
		return "", err
	}
	// Precedence: open PR > most-recent closed/merged PR (mirrors Worktree.pr).
	var best *gh.PullRequest
	for i := range prs {
		pr := &prs[i]
		if pr.HeadRef != branch {
			continue
		}
		if best == nil {
			best = pr
			continue
		}
		// Open beats any non-open.
		if pr.State == gh.PullRequestStateOpen && best.State != gh.PullRequestStateOpen {
			best = pr
		}
	}
	if best == nil {
		return "", nil
	}
	return string(best.State), nil
}

// LoaderBundle returns the read-side surface the request-scoped
// dataloaders need. Used as a fallback when no middleware-installed
// Loaders is on the context (e.g. internal subscription emissions).
func (r *Resolver) LoaderBundle() *loaders.ProvidersBundle {
	return &loaders.ProvidersBundle{
		Host:       r.HostProvider,
		Git:        r.Git,
		Ps:         r.PS,
		Tmux:       r.Tmux,
		Repos:      r.ReposProvider,
		GH:         r.GH,
		GHEnricher: r.GH,
	}
}
