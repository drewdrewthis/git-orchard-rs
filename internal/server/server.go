// Package server hosts the orchard daemon's HTTP surface.
//
// Workstream A: stub Health resolver, /health, graceful shutdown.
// Workstream B-host: host Provider constructed and started in Run.
// Workstream B-config: Option pattern (WithFoo) wires optional providers.
// Workstream B-git: WithGit wires the git provider.
// Workstream B-ps: WithPS attaches a ps provider; Run() starts it.
// Workstream B-tmux: WithTmux + tmux watcher.
// Workstream B-claudeprojects: WithClaudeProjects starts on Run().
// Workstream B-claudeaccount: WithClaudeAccount starts on Run().
// Workstream B-hostservice: WithHostService starts on Run().
// Workstream D-gh: WithGh wires the gh provider; Run() calls Start to prime
// the auth bootstrap (failure is non-fatal — gh-derived fields surface
// per-field GraphQL errors when auth is unavailable).
// Workstream F: WithPeerProxy attaches the federation provider. Peer
// authentication is delegated to the transport (TLS + boxd subdomain
// allowlists); the daemon does not enforce a bearer-secret guard
// (issue #412).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	gqlws "github.com/gorilla/websocket"

	gql "github.com/drewdrewthis/orchardist/internal/server/graphql"
	"github.com/drewdrewthis/orchardist/internal/server/loaders"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudeaccount"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudeinstance"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudeprojects"
	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
	gitprovider "github.com/drewdrewthis/orchardist/internal/server/providers/git"
	"github.com/drewdrewthis/orchardist/internal/server/providers/host"
	"github.com/drewdrewthis/orchardist/internal/server/providers/hostservice"
	"github.com/drewdrewthis/orchardist/internal/server/providers/peerproxy"
	"github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	"github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
	"github.com/drewdrewthis/orchardist/internal/server/resolvers"
)

// DefaultAddr is where the daemon listens.
const DefaultAddr = "localhost:7777"

// claudeProjectsRootEnv overrides the Claude transcripts root path.
const claudeProjectsRootEnv = "CLAUDE_PROJECTS_ROOT"

// convoJSONLConfig holds the two values needed to mount the conversations
// jsonl handler: the PathLookup provider and the projects root used for
// path-traversal validation.
type convoJSONLConfig struct {
	provider PathLookup
	root     string
}

// Server wraps the http.Server plus the resolver root and provider set.
type Server struct {
	addr           string
	startedAt      time.Time
	logger         *slog.Logger
	httpSrv        *http.Server
	host           *host.Provider
	resolver       *resolvers.Resolver
	gitProv        *gitprovider.Provider
	psProv         *ps.Provider
	tmuxProv       *tmux.Provider
	claudeProjects *claudeprojects.Provider
	claudeAccount  *claudeaccount.Provider
	hostService    *hostservice.Provider
	gh             *gh.Provider
	peerProxy      *peerproxy.Provider
	localEvents    *peerproxy.LocalInvalidator
	convoJSONL     *convoJSONLConfig
}

// LocalEvents exposes the configured local-invalidation broker for
// callers (typically tests) that need to fire synthetic events.
// Returns nil when no broker is wired.
func (s *Server) LocalEvents() *peerproxy.LocalInvalidator { return s.localEvents }

// Option mutates a Server during construction.
type Option func(*Server, *resolvers.Resolver)

// WithRepos wires a repos provider.
func WithRepos(p resolvers.ReposLister) Option {
	return func(_ *Server, r *resolvers.Resolver) { r.WithRepos(p) }
}

// WithGit wires a git provider. Run() owns the provider's lifecycle —
// it does not start anything (NewProvider already kicks off the
// per-project watchers), but Stop is called on shutdown so watcher
// goroutines drain cleanly before the daemon exits.
func WithGit(g *gitprovider.Provider) Option {
	return func(s *Server, r *resolvers.Resolver) {
		s.gitProv = g
		r.WithGit(g)
	}
}

// WithPS attaches a ps provider.
func WithPS(p *ps.Provider) Option {
	return func(s *Server, r *resolvers.Resolver) {
		s.psProv = p
		r.WithPS(p)
	}
}

// WithTmux attaches a tmux provider.
func WithTmux(p *tmux.Provider) Option {
	return func(s *Server, r *resolvers.Resolver) {
		s.tmuxProv = p
		r.WithTmux(p)
	}
}

// WithClaudeProjects attaches a claudeprojects provider.
func WithClaudeProjects(p *claudeprojects.Provider) Option {
	return func(s *Server, r *resolvers.Resolver) {
		s.claudeProjects = p
		r.WithClaudeProjects(p)
	}
}

// WithConversationsJSONL mounts the conversations jsonl file-server
// handler on the same listener as /graphql. The handler is registered
// at /v1/conversations/ (trailing slash — ServeMux prefix match) and
// serves GET /v1/conversations/:sessionUuid/jsonl. Requires a PathLookup
// provider (typically *claudeprojects.Provider) for uuid-to-path lookup,
// and the projectsRoot string used for path-traversal validation.
//
// Asymmetric vs other With* options: this option stashes a config rather
// than handing the dependency to the resolver, because the handler
// attaches to the daemon's HTTP mux (built later in New) rather than to
// the GraphQL resolver tree. New() reads s.convoJSONL after option
// application and registers the route on the same mux as /graphql.
func WithConversationsJSONL(p PathLookup, projectsRoot string) Option {
	return func(s *Server, _ *resolvers.Resolver) {
		s.convoJSONL = &convoJSONLConfig{provider: p, root: projectsRoot}
	}
}

// WithClaudeAccount attaches a claudeaccount provider.
func WithClaudeAccount(p *claudeaccount.Provider) Option {
	return func(s *Server, r *resolvers.Resolver) {
		s.claudeAccount = p
		r.WithClaudeAccount(p)
	}
}

// WithHostService attaches a hostservice provider.
func WithHostService(p *hostservice.Provider) Option {
	return func(s *Server, r *resolvers.Resolver) {
		s.hostService = p
		r.WithHostService(p)
	}
}

// WithGh wires a gh provider. Run() calls Start to prime the auth
// bootstrap; any failure there is non-fatal and surfaces as per-field
// GraphQL errors on gh-derived queries (ADR-011 §6 / §12).
func WithGh(p *gh.Provider) Option {
	return func(s *Server, r *resolvers.Resolver) {
		s.gh = p
		r.WithGH(p)
	}
}

// WithPeerProxy attaches the federation provider that surfaces remote
// orchard daemons as peers. Run() starts the per-peer probe + subscription
// supervisor; the resolver gains node-id forwarding and Subscription.peer
// tunneling.
func WithPeerProxy(p *peerproxy.Provider) Option {
	return func(s *Server, r *resolvers.Resolver) {
		s.peerProxy = p
		r.WithPeerProxy(p)
	}
}

// WithLocalEvents wires a LocalInvalidator into the resolver so the
// federation `Subscription.peer(host: "*")` field can fan local node
// updates out to upstream peers.
func WithLocalEvents(l *peerproxy.LocalInvalidator) Option {
	return func(s *Server, r *resolvers.Resolver) {
		s.localEvents = l
		r.WithLocalEvents(l)
	}
}

// WithVersion injects the daemon binary version so Query.version can
// surface it. Callers pass the package-level `version` variable from
// cmd/orchard-daemon/main.go; the value is "dev" when no -ldflags were used.
func WithVersion(v string) Option {
	return func(_ *Server, r *resolvers.Resolver) { r.WithVersion(v) }
}

// WithGitMutations wires the git-domain mutation resolver (Mutation.worktreeRemove, etc.).
// scriptRoot is the absolute path to the scripts/ directory that holds
// scripts/git/worktree-remove.sh and siblings. When empty,
// gitdomain.NewMutationResolver defaults to "scripts" (relative to cwd).
func WithGitMutations(scriptRoot string) Option {
	return func(_ *Server, r *resolvers.Resolver) { r.WithGitMutations(scriptRoot) }
}

// New constructs a Server bound to addr.
func New(addr string, logger *slog.Logger, opts ...Option) *Server {
	if addr == "" {
		addr = DefaultAddr
	}
	if logger == nil {
		logger = slog.Default()
	}
	startedAt := time.Now()

	hostProvider := host.New()
	res := resolvers.New(startedAt).WithHost(hostProvider)

	srv := &Server{
		addr:      addr,
		startedAt: startedAt,
		logger:    logger,
		host:      hostProvider,
		resolver:  res,
	}
	for _, opt := range opts {
		opt(srv, res)
	}

	mux := http.NewServeMux()
	mux.Handle("/health", healthHandler(startedAt))
	// Wrap the GraphQL handler with the request-scoped DataLoader middleware.
	// The bundle is built from the resolver's provider set so the Pr resolver
	// can batch ListPullRequests calls across all worktrees in one request.
	mux.Handle("/graphql", loaders.Middleware(res.LoaderBundle(), graphqlHandlerFor(res)))

	// Mount the conversations jsonl file-server on the same mux as /graphql
	// so it inherits the daemon's loopback bind address. The trailing slash
	// on the pattern makes ServeMux treat it as a prefix match, which the
	// handler's parseSessionUUID already expects.
	if srv.convoJSONL != nil {
		mux.Handle("/v1/conversations/",
			NewConversationsJSONLHandler(srv.convoJSONL.provider, srv.convoJSONL.root, logger),
		)
	}

	// Serve the read-only dashboard page at the exact "/" (see dashboard.go).
	// Registered last as the "/" catch-all; ServeMux longest-prefix keeps the
	// routes above winning, and the handler 404s any non-"/" path so this does
	// not become an open file server.
	mux.Handle("/", dashboardHandler())

	srv.httpSrv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	return srv
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.addr }

// HTTPHandler returns the underlying HTTP mux for tests.
func (s *Server) HTTPHandler() http.Handler { return s.httpSrv.Handler }

// Resolver returns the resolver root.
func (s *Server) Resolver() *resolvers.Resolver { return s.resolver }

// StartHostProvider hydrates the host provider's identity + load
// caches. Run() does this implicitly; tests that mount GraphQLHandler
// directly call this to make Query.host return useful data.
func (s *Server) StartHostProvider(ctx context.Context) error {
	if s.host == nil {
		return nil
	}
	return s.host.Start(ctx)
}

// GraphQLHandler returns a fresh handler bound to the server's resolver.
// Tests that need to mount the handler with custom middleware should
// call this and wrap the result; production wires the handler via the
// daemon's mux in New().
func (s *Server) GraphQLHandler() http.Handler {
	return graphqlHandlerFor(s.resolver)
}

// Run starts providers, the HTTP server, and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	if err := s.host.Start(ctx); err != nil {
		return fmt.Errorf("start host provider: %w", err)
	}
	if s.psProv != nil {
		if err := s.psProv.Start(ctx); err != nil {
			return fmt.Errorf("ps provider start: %w", err)
		}
	}
	if s.tmuxProv != nil {
		if err := s.tmuxProv.Start(ctx); err != nil {
			return fmt.Errorf("tmux provider start: %w", err)
		}
		if err := tmux.StartWatcher(ctx, s.tmuxProv, s.logger); err != nil {
			s.logger.Warn("tmux watcher start failed; continuing poll-only", "err", err)
		}
	}
	if s.claudeProjects != nil {
		if err := s.claudeProjects.Start(ctx); err != nil {
			return fmt.Errorf("start claudeprojects provider: %w", err)
		}
	}
	if s.claudeAccount != nil {
		if err := s.claudeAccount.Start(ctx); err != nil {
			return fmt.Errorf("start claudeaccount provider: %w", err)
		}
	}
	if s.hostService != nil {
		if err := s.hostService.Start(ctx); err != nil {
			return fmt.Errorf("start hostservice provider: %w", err)
		}
	}
	if s.gh != nil {
		// gh.Provider.Start primes the auth bootstrap and intentionally
		// returns nil even when auth is unavailable — failures surface
		// per-field via the resolver layer. We still propagate any
		// unexpected non-nil error so an obvious wiring bug is loud.
		if err := s.gh.Start(ctx); err != nil {
			return fmt.Errorf("start gh provider: %w", err)
		}
	}
	// Run the sidecar janitor at startup to remove orphaned heartbeat files
	// left by the old hook script. Errors are non-blocking — the janitor
	// logs and continues.
	janitor := claudeinstance.NewSidecarJanitor(
		claudeinstance.ResolveDir(),
		func(_ context.Context) (map[string]bool, error) {
			if s.tmuxProv == nil {
				return nil, errors.New("tmux provider unavailable; skipping sidecar sweep")
			}
			snap := s.tmuxProv.Snapshot()
			live := make(map[string]bool, len(snap.Sessions))
			for k := range snap.Sessions {
				live[k.Name] = true
			}
			return live, nil
		},
		s.logger,
	)
	_ = janitor.Sweep(ctx)

	if s.peerProxy != nil {
		if err := s.peerProxy.Start(ctx); err != nil {
			return fmt.Errorf("peerproxy start: %w", err)
		}
		defer func() { _ = s.peerProxy.Stop() }()
	}
	if s.gitProv != nil {
		// The git provider's watchers are spawned by AddProject; Stop
		// drains them on shutdown.
		defer s.gitProv.Stop()
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("orchard daemon listening", "addr", s.addr)
		errCh <- s.httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		if s.claudeProjects != nil {
			_ = s.claudeProjects.Stop()
		}
		s.logger.Info("orchard daemon stopped")
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// claudeProjectsRoot returns the directory the daemon should watch.
func claudeProjectsRoot() string {
	if v := os.Getenv(claudeProjectsRootEnv); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".claude", "projects")
	}
	return filepath.Join(home, ".claude", "projects")
}

// healthHandler reflects the server's uptime as JSON.
func healthHandler(startedAt time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uptime := int64(time.Since(startedAt).Round(time.Second).Seconds())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"uptimeS": uptime,
		})
	}
}

// graphqlHandlerFor wires the gqlgen executable schema with an already-constructed Resolver root.
//
// The websocket transport carries Subscription operations (Workstream F).
//
// Origin policy & write-mutation perimeter
// ----------------------------------------
// The daemon binds 127.0.0.1:7777. Cross-host reachability is delegated
// to Tailscale (tailnet identity + ACL — see "Peer authentication is
// delegated to the transport" above). The daemon itself does NOT hold a
// bearer secret; the transport is the perimeter.
//
// That contract held when subscriptions were read-only. The introduction
// of Mutation.sendTextToPane (writes into a tmux pane) widens the surface:
// a browser on a tailnet-paired device can reach :7777, and without origin
// gating any webpage that browser loads could fetch the daemon via JS and
// drive tmux. CSRF, not auth bypass.
//
// CheckOrigin therefore allowlists the GUI's served origins (Tauri's
// tauri://localhost, the desktop dev origin, and the configured PWA
// origins) rather than `return true`. A non-GUI origin is refused.
//
// For new write mutations: keep the perimeter Tailscale-shaped, and make
// sure the GUI origin allowlist covers any new served surface that needs
// to call them. If a non-GUI client legitimately needs to drive a write
// mutation, gate that mutation explicitly (capability token, pane
// allowlist) — don't loosen CheckOrigin.
func graphqlHandlerFor(res *resolvers.Resolver) http.Handler {
	cfg := gql.Config{Resolvers: res}
	srv := handler.New(gql.NewExecutableSchema(cfg))
	srv.AddTransport(transport.POST{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.Websocket{
		KeepAlivePingInterval: 10 * time.Second,
		Upgrader: gqlws.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     checkGUIOrigin,
			Subprotocols:    []string{"graphql-transport-ws", "graphql-ws"},
		},
	})
	// Introspection is gated by env var. ON by default — the daemon binds
	// to localhost (federation runs over SSH tunnels per issue #474), so
	// schema introspection is local-only and worth the ergonomic win.
	// Operators on shared hosts can disable with ORCHARD_INTROSPECTION=0.
	// Resolves issue #469 F4 (and reverses the original gate from #401
	// now that auth is delegated to the SSH transport).
	if introspectionEnabled() {
		srv.Use(extension.Introspection{})
	}
	return srv
}

// introspectionEnabled returns true when the daemon should respond to
// `__schema` / `__type` queries. Defaults to ON; ORCHARD_INTROSPECTION=0
// (or false/no/off) opts out. The toggle is intentionally an env var
// rather than a CLI flag so daemons started by launchd / systemd can
// opt out without a wrapper script that re-execs with a custom flag.
func introspectionEnabled() bool {
	switch os.Getenv("ORCHARD_INTROSPECTION") {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// checkGUIOrigin gates websocket upgrades by Origin header. See the
// graphqlHandlerFor doc-comment for the rationale; tl;dr Tailscale is the
// auth perimeter but CheckOrigin closes the CSRF gap that opened when
// sendTextToPane (a write mutation) shipped.
//
// Allowed origins:
//   - missing Origin header (native clients: Tauri webview, curl, codegen)
//   - tauri://localhost (Tauri's served origin)
//   - http(s)://localhost:* and http(s)://127.0.0.1:* (dev servers,
//     vite preview, Playwright runs)
//
// Operators can extend the allowlist with ORCHARD_ALLOWED_ORIGINS (comma-
// separated; exact match against the Origin header). Use for tunneled
// dev surfaces (e.g. https://orchard.example.trycloudflare.com) the GUI
// is actually served from. Tailnet hostnames don't need entries — they
// reach the daemon over the tunnel but the served GUI's origin is what
// the browser sends.
func checkGUIOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if origin == "tauri://localhost" {
		return true
	}
	// localhost / 127.0.0.1 on any port.
	for _, scheme := range []string{"http://localhost", "https://localhost", "http://127.0.0.1", "https://127.0.0.1"} {
		if origin == scheme || (len(origin) > len(scheme)+1 && origin[:len(scheme)+1] == scheme+":") {
			return true
		}
	}
	if extra := os.Getenv("ORCHARD_ALLOWED_ORIGINS"); extra != "" {
		for _, allowed := range splitAndTrim(extra, ',') {
			if origin == allowed {
				return true
			}
		}
	}
	return false
}

// splitAndTrim splits s on sep and trims ASCII whitespace from each part.
// Empty parts are dropped.
func splitAndTrim(s string, sep byte) []string {
	out := make([]string, 0, 4)
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == sep {
			part := s[start:i]
			// trim
			for len(part) > 0 && (part[0] == ' ' || part[0] == '\t') {
				part = part[1:]
			}
			for len(part) > 0 && (part[len(part)-1] == ' ' || part[len(part)-1] == '\t') {
				part = part[:len(part)-1]
			}
			if len(part) > 0 {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}
