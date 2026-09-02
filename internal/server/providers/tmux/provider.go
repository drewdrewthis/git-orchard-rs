// Provider wires the tmux Adapter, an in-memory Store, and the watcher
// loop into the Provider[K, V] surface. Per ADR-011 the resolver layer
// depends on this provider through its node-typed Provider interfaces;
// the adapter is private to this package.

package tmux

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	provider "github.com/drewdrewthis/orchardist/internal/server/adapter"
	"github.com/drewdrewthis/orchardist/internal/server/store"
)

// DefaultPollInterval is the watcher tick rate per ADR-011 §12 line 413.
// Tunable via WithPollInterval — the E2E test runs faster.
const DefaultPollInterval = 1 * time.Second

// ActiveAttachedWindow is how recent a client's last_activity must be
// for TmuxSession.activeAttached to be true. ADR §5.1 spec.
const ActiveAttachedWindow = 5 * time.Minute

// Provider is the orchard-side facade over the tmux Adapter. It owns
// the in-memory cache (one Store per node type), the poll/watch loop,
// and invalidation fanout. Resolvers reach through Snapshot() to read
// joined state; the four typed Provider[K, V] facets satisfy the
// generic Provider contract for each node type.
type Provider struct {
	adapter      *Adapter
	pollInterval time.Duration
	logger       *slog.Logger

	sessions *store.Store[SessionKey, Session]
	windows  *store.Store[WindowKey, Window]
	panes    *store.Store[PaneKey, Pane]
	clients  *store.Store[ClientKey, Client]

	mu     sync.RWMutex
	server ServerInfo

	// snapshotCalls counts Snapshot() invocations so the O9 hot-path
	// allocation audit has a number to assert on. Snapshot() clones the
	// whole cached graph; a field resolver that calls it is the #612
	// regression, and TestTmuxFieldResolvers_NoSnapshotClone_Issue612
	// asserts this counter stays at zero across a field traversal.
	snapshotCalls atomic.Int64

	subsMu       sync.Mutex
	sessionSubs  []chan provider.InvalidationEvent[SessionKey]
	windowSubs   []chan provider.InvalidationEvent[WindowKey]
	paneSubs     []chan provider.InvalidationEvent[PaneKey]
	clientSubs   []chan provider.InvalidationEvent[ClientKey]
	tickerSignal chan struct{} // tests pulse this to force a refresh
}

// New constructs a Provider for the given adapter. The watcher does not
// start until Start is called.
func New(a *Adapter, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{
		adapter:      a,
		pollInterval: DefaultPollInterval,
		logger:       logger,
		sessions:     store.New[SessionKey, Session](),
		windows:      store.New[WindowKey, Window](),
		panes:        store.New[PaneKey, Pane](),
		clients:      store.New[ClientKey, Client](),
		tickerSignal: make(chan struct{}, 1),
	}
}

// WithPollInterval lets tests bypass the 1s default — passing 50ms gives
// E2E tests a sub-second roundtrip without burning CPU in production.
func (p *Provider) WithPollInterval(d time.Duration) *Provider {
	if d > 0 {
		p.pollInterval = d
	}
	return p
}

// Adapter exposes the wrapped adapter for tests / introspection.
func (p *Provider) Adapter() *Adapter { return p.adapter }

// Host returns the host id every key carries.
func (p *Provider) Host() HostID { return p.adapter.Host() }

// Start performs an initial fetch and kicks off the poll loop. Returns
// the first-fetch error so the caller (the daemon entry point) sees a
// fundamentally broken environment at boot rather than later.
func (p *Provider) Start(ctx context.Context) error {
	if err := p.refresh(ctx); err != nil {
		return fmt.Errorf("tmux provider: initial fetch: %w", err)
	}
	go p.pollLoop(ctx)
	return nil
}

// Refresh runs an immediate fetch — useful in tests that want to skip
// the poll wait.
func (p *Provider) Refresh(ctx context.Context) error { return p.refresh(ctx) }

func (p *Provider) refresh(ctx context.Context) error {
	snap, err := p.adapter.FetchAll(ctx)
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.server = snap.Server
	p.mu.Unlock()

	now := time.Now()
	sessChanged := p.sessions.ReplaceAll(snap.Sessions, provider.SourcePoll, sessionsEqual)
	winChanged := p.windows.ReplaceAll(snap.Windows, provider.SourcePoll, windowsEqual)
	paneChanged := p.panes.ReplaceAll(snap.Panes, provider.SourcePoll, panesEqual)
	clientChanged := p.clients.ReplaceAll(snap.Clients, provider.SourcePoll, clientsEqual)

	for _, k := range sessChanged {
		p.fanoutSession(provider.InvalidationEvent[SessionKey]{Key: k, Reason: "poll", At: now})
	}
	for _, k := range winChanged {
		p.fanoutWindow(provider.InvalidationEvent[WindowKey]{Key: k, Reason: "poll", At: now})
	}
	for _, k := range paneChanged {
		p.fanoutPane(provider.InvalidationEvent[PaneKey]{Key: k, Reason: "poll", At: now})
	}
	for _, k := range clientChanged {
		p.fanoutClient(provider.InvalidationEvent[ClientKey]{Key: k, Reason: "poll", At: now})
	}
	return nil
}

func (p *Provider) pollLoop(ctx context.Context) {
	t := time.NewTicker(p.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.refresh(ctx); err != nil {
				p.logger.Warn("tmux provider: refresh failed", "err", err)
			}
		case <-p.tickerSignal:
			if err := p.refresh(ctx); err != nil {
				p.logger.Warn("tmux provider: forced refresh failed", "err", err)
			}
		}
	}
}

// PokeRefresh forces the poll loop to refresh outside its tick. The
// fsnotify watcher hooks into this so socket-directory events translate
// into immediate fetches.
func (p *Provider) PokeRefresh() {
	select {
	case p.tickerSignal <- struct{}{}:
	default:
	}
}

// ----------------------------------------------------------------------
// Snapshot — resolver-friendly read of the entire cached graph.
// ----------------------------------------------------------------------

// Snapshot is a pointer-free view of the provider's cache, taken under
// each store's read lock independently — callers should treat the
// snapshot as a single point in time even though the four reads are
// not transactional. Mutations after the call do not bleed into the
// returned maps.
type RuntimeSnapshot struct {
	Server   ServerInfo
	Sessions map[SessionKey]Session
	Windows  map[WindowKey]Window
	Panes    map[PaneKey]Pane
	Clients  map[ClientKey]Client
}

// Snapshot returns the cached graph. Empty when no tmux daemon is
// running (poll loop puts EmptySnapshot through the stores in that case).
func (p *Provider) Snapshot() RuntimeSnapshot {
	p.snapshotCalls.Add(1)
	p.mu.RLock()
	server := p.server
	p.mu.RUnlock()
	return RuntimeSnapshot{
		Server:   server,
		Sessions: p.sessions.Snapshot(),
		Windows:  p.windows.Snapshot(),
		Panes:    p.panes.Snapshot(),
		Clients:  p.clients.Snapshot(),
	}
}

// SnapshotCalls returns how many times Snapshot() has been called on this
// provider. Snapshot() clones the entire cached graph, so this counter is
// the O9 hot-path allocation audit for the tmux domain: it must not grow
// while GraphQL field resolvers run (R3). Mirrors the loader package's
// PaneByIDBatchCount() observability hook.
func (p *Provider) SnapshotCalls() int64 { return p.snapshotCalls.Load() }

// Server returns the cached ServerInfo. Provided separately so resolvers
// for TmuxServer.alive / .pid don't pay the snapshot copy cost.
func (p *Provider) Server() ServerInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.server
}

// ----------------------------------------------------------------------
// Typed secondary-axis accessors (ADR-022: Pane is the node, one
// snapshot read per accessor, no N+1 in the callers).
// ----------------------------------------------------------------------

// PaneByID returns the pane whose stable pane id (e.g. "%26") matches on
// the given host, or (Pane{}, false) when not found.
func (p *Provider) PaneByID(host, paneID string) (Pane, bool) {
	snap := p.panes.Snapshot()
	key := PaneKey{Host: HostID(host), PaneID: paneID}
	pn, ok := snap[key]
	return pn, ok
}

// PanesByCwd returns every pane on host whose foreground-process cwd
// equals cwd exactly or has cwd+"/" as a prefix. The cwd is resolved via
// the supplied psGetter; panes whose cwd cannot be resolved are silently
// skipped. Returns [] (never nil).
//
// The psGetter is a narrow interface satisfied by *psprovider.Provider
// via a thin adapter; it is passed in rather than stored on Provider to
// keep the tmux package free of a ps import.
func (p *Provider) PanesByCwd(host, cwd string, ps PanePsGetter) []Pane {
	if cwd == "" {
		return []Pane{}
	}
	snap := p.panes.Snapshot()
	var out []Pane
	for _, pn := range snap {
		if string(pn.Key.Host) != host {
			continue
		}
		if pn.CurrentPid <= 0 {
			continue
		}
		paneCwd := ps.CwdForPid(host, pn.CurrentPid)
		if paneCwd == "" {
			continue
		}
		if paneCwd != cwd && !strings.HasPrefix(paneCwd, cwd+"/") {
			continue
		}
		out = append(out, pn)
	}
	if out == nil {
		return []Pane{}
	}
	return out
}

// PanesByCommand returns every pane on host whose foreground command
// basename contains basenameContains (case-insensitive). The command is
// cross-checked via the supplied psGetter so node-wrapped CLIs (e.g.
// `node /usr/local/bin/claude`) resolve to their real basename instead of
// the raw tmux pane_current_command string. Returns [] (never nil).
func (p *Provider) PanesByCommand(host, basenameContains string, ps PanePsGetter) []Pane {
	if basenameContains == "" {
		return []Pane{}
	}
	needle := strings.ToLower(basenameContains)
	snap := p.panes.Snapshot()
	var out []Pane
	for _, pn := range snap {
		if string(pn.Key.Host) != host {
			continue
		}
		if paneCommandMatchesClaude(pn, host, ps, needle) {
			out = append(out, pn)
		}
	}
	if out == nil {
		return []Pane{}
	}
	return out
}

// PaneRunsCommand reports whether pn is running needle, matched
// case-insensitively against the command basename. It is the same detection
// PanesByCommand applies, exported so a resolver holding a single already-known
// pane can ask the question without re-deriving it — one source of truth for
// "is this pane running claude".
func PaneRunsCommand(pn Pane, host string, ps PanePsGetter, needle string) bool {
	if needle == "" {
		return false
	}
	return paneCommandMatchesClaude(pn, host, ps, strings.ToLower(needle))
}

// PanesBySession returns every pane whose tmux session name equals
// sessionName on the given host. Returns [] (never nil).
func (p *Provider) PanesBySession(host, sessionName string) []Pane {
	if sessionName == "" {
		return []Pane{}
	}
	snap := p.panes.Snapshot()
	var out []Pane
	for _, pn := range snap {
		if string(pn.Key.Host) != host {
			continue
		}
		if pn.WindowKey.Session != sessionName {
			continue
		}
		out = append(out, pn)
	}
	if out == nil {
		return []Pane{}
	}
	return out
}

// paneCommandMatchesClaude reports whether a pane is running needle
// (lower-case), consulting two independent signals.
//
// CurrentPid is tmux's pane_pid — the pane's ROOT process, NOT its foreground
// process. A session started as `bash -> claude` has a bash root pid, so asking
// ps about it answers "bash" even though claude is very much running. tmux's own
// pane_current_command resolves the foreground through the shell and answers
// "claude" correctly.
//
// So the two signals are complementary, not ranked: ps sees through wrapper
// processes tmux reports verbatim, and tmux sees through shells ps cannot.
// Either one matching is a match. Previously a non-empty ps answer returned
// early and shadowed tmux's, which made every shell-wrapped claude session
// invisible to Query.claudeInstances — and the concurrency cap that reads it
// counted 0 workers while workers were running (#706).
func paneCommandMatchesClaude(pn Pane, host string, ps PanePsGetter, needle string) bool {
	if ps != nil && pn.CurrentPid > 0 {
		cmd := ps.CommandForPid(host, pn.CurrentPid)
		if cmd != "" && strings.Contains(strings.ToLower(filepath.Base(cmd)), needle) {
			return true
		}
	}
	// tmux pane_current_command (may be a version string on macOS, hence ps above).
	return strings.Contains(strings.ToLower(filepath.Base(pn.CurrentCommand)), needle)
}

// PanePsGetter is the narrow ps surface the secondary-axis accessors need.
// *psprovider.Provider satisfies this via a thin adapter (see loaders package).
// Tests implement it inline.
type PanePsGetter interface {
	// CwdForPid returns the working directory of the process with the given
	// pid on the host, or "" when unavailable.
	CwdForPid(host string, pid int) string
	// CommandForPid returns the command basename (e.g. "claude") for the
	// given pid on the host, or "" when unavailable.
	CommandForPid(host string, pid int) string
}

// CapturePane shells out via the adapter; not cached. The schema docs
// the on-demand cost.
func (p *Provider) CapturePane(ctx context.Context, key PaneKey, start, end int, full, stripAnsi bool) (string, error) {
	return p.adapter.CapturePane(ctx, key, start, end, full, stripAnsi)
}

// CapturePaneTail wraps the adapter's tail-capture for the schema
// `content(lines:)` field.
func (p *Provider) CapturePaneTail(ctx context.Context, key PaneKey, lines int, stripAnsi bool) (string, error) {
	return p.adapter.CapturePaneTail(ctx, key, lines, stripAnsi)
}

// ----------------------------------------------------------------------
// Provider[K,V] facets — one per node type. Resolvers depend on these
// generic interfaces, not on *Provider directly.
// ----------------------------------------------------------------------

// Sessions returns a Provider facet over SessionKey/Session.
func (p *Provider) Sessions() provider.Provider[SessionKey, Session] {
	return sessionsFacet{p}
}

// Windows returns a Provider facet over WindowKey/Window.
func (p *Provider) Windows() provider.Provider[WindowKey, Window] { return windowsFacet{p} }

// Panes returns a Provider facet over PaneKey/Pane.
func (p *Provider) Panes() provider.Provider[PaneKey, Pane] { return panesFacet{p} }

// Clients returns a Provider facet over ClientKey/Client.
func (p *Provider) Clients() provider.Provider[ClientKey, Client] { return clientsFacet{p} }

type sessionsFacet struct{ p *Provider }

func (f sessionsFacet) Get(_ context.Context, k SessionKey) (Session, provider.Freshness, error) {
	v, fr, ok := f.p.sessions.Get(k)
	if !ok {
		return Session{}, provider.Freshness{}, fmt.Errorf("tmux session %q not found", k.Name)
	}
	return v, fr, nil
}

func (f sessionsFacet) GetMany(_ context.Context, keys []SessionKey) (map[SessionKey]Session, map[SessionKey]provider.Freshness, error) {
	values := make(map[SessionKey]Session, len(keys))
	freshness := make(map[SessionKey]provider.Freshness, len(keys))
	for _, k := range keys {
		if v, fr, ok := f.p.sessions.Get(k); ok {
			values[k] = v
			freshness[k] = fr
		}
	}
	return values, freshness, nil
}

func (f sessionsFacet) Keys(_ context.Context) ([]SessionKey, error) {
	return f.p.sessions.Keys(), nil
}

func (f sessionsFacet) Subscribe(ctx context.Context) <-chan provider.InvalidationEvent[SessionKey] {
	ch := make(chan provider.InvalidationEvent[SessionKey], 8)
	f.p.subsMu.Lock()
	f.p.sessionSubs = append(f.p.sessionSubs, ch)
	f.p.subsMu.Unlock()
	go func() {
		<-ctx.Done()
		f.p.subsMu.Lock()
		defer f.p.subsMu.Unlock()
		for i, c := range f.p.sessionSubs {
			if c == ch {
				f.p.sessionSubs = append(f.p.sessionSubs[:i], f.p.sessionSubs[i+1:]...)
				close(ch)
				return
			}
		}
	}()
	return ch
}

type windowsFacet struct{ p *Provider }

func (f windowsFacet) Get(_ context.Context, k WindowKey) (Window, provider.Freshness, error) {
	v, fr, ok := f.p.windows.Get(k)
	if !ok {
		return Window{}, provider.Freshness{}, fmt.Errorf("tmux window %s:%d not found", k.Session, k.Index)
	}
	return v, fr, nil
}

func (f windowsFacet) GetMany(_ context.Context, keys []WindowKey) (map[WindowKey]Window, map[WindowKey]provider.Freshness, error) {
	values := make(map[WindowKey]Window, len(keys))
	freshness := make(map[WindowKey]provider.Freshness, len(keys))
	for _, k := range keys {
		if v, fr, ok := f.p.windows.Get(k); ok {
			values[k] = v
			freshness[k] = fr
		}
	}
	return values, freshness, nil
}

func (f windowsFacet) Keys(_ context.Context) ([]WindowKey, error) { return f.p.windows.Keys(), nil }

func (f windowsFacet) Subscribe(ctx context.Context) <-chan provider.InvalidationEvent[WindowKey] {
	ch := make(chan provider.InvalidationEvent[WindowKey], 8)
	f.p.subsMu.Lock()
	f.p.windowSubs = append(f.p.windowSubs, ch)
	f.p.subsMu.Unlock()
	go func() {
		<-ctx.Done()
		f.p.subsMu.Lock()
		defer f.p.subsMu.Unlock()
		for i, c := range f.p.windowSubs {
			if c == ch {
				f.p.windowSubs = append(f.p.windowSubs[:i], f.p.windowSubs[i+1:]...)
				close(ch)
				return
			}
		}
	}()
	return ch
}

type panesFacet struct{ p *Provider }

func (f panesFacet) Get(_ context.Context, k PaneKey) (Pane, provider.Freshness, error) {
	v, fr, ok := f.p.panes.Get(k)
	if !ok {
		return Pane{}, provider.Freshness{}, fmt.Errorf("tmux pane %s not found", k.PaneID)
	}
	return v, fr, nil
}

func (f panesFacet) GetMany(_ context.Context, keys []PaneKey) (map[PaneKey]Pane, map[PaneKey]provider.Freshness, error) {
	values := make(map[PaneKey]Pane, len(keys))
	freshness := make(map[PaneKey]provider.Freshness, len(keys))
	for _, k := range keys {
		if v, fr, ok := f.p.panes.Get(k); ok {
			values[k] = v
			freshness[k] = fr
		}
	}
	return values, freshness, nil
}

func (f panesFacet) Keys(_ context.Context) ([]PaneKey, error) { return f.p.panes.Keys(), nil }

func (f panesFacet) Subscribe(ctx context.Context) <-chan provider.InvalidationEvent[PaneKey] {
	ch := make(chan provider.InvalidationEvent[PaneKey], 8)
	f.p.subsMu.Lock()
	f.p.paneSubs = append(f.p.paneSubs, ch)
	f.p.subsMu.Unlock()
	go func() {
		<-ctx.Done()
		f.p.subsMu.Lock()
		defer f.p.subsMu.Unlock()
		for i, c := range f.p.paneSubs {
			if c == ch {
				f.p.paneSubs = append(f.p.paneSubs[:i], f.p.paneSubs[i+1:]...)
				close(ch)
				return
			}
		}
	}()
	return ch
}

type clientsFacet struct{ p *Provider }

func (f clientsFacet) Get(_ context.Context, k ClientKey) (Client, provider.Freshness, error) {
	v, fr, ok := f.p.clients.Get(k)
	if !ok {
		return Client{}, provider.Freshness{}, fmt.Errorf("tmux client %q not found", k.ClientName)
	}
	return v, fr, nil
}

func (f clientsFacet) GetMany(_ context.Context, keys []ClientKey) (map[ClientKey]Client, map[ClientKey]provider.Freshness, error) {
	values := make(map[ClientKey]Client, len(keys))
	freshness := make(map[ClientKey]provider.Freshness, len(keys))
	for _, k := range keys {
		if v, fr, ok := f.p.clients.Get(k); ok {
			values[k] = v
			freshness[k] = fr
		}
	}
	return values, freshness, nil
}

func (f clientsFacet) Keys(_ context.Context) ([]ClientKey, error) { return f.p.clients.Keys(), nil }

func (f clientsFacet) Subscribe(ctx context.Context) <-chan provider.InvalidationEvent[ClientKey] {
	ch := make(chan provider.InvalidationEvent[ClientKey], 8)
	f.p.subsMu.Lock()
	f.p.clientSubs = append(f.p.clientSubs, ch)
	f.p.subsMu.Unlock()
	go func() {
		<-ctx.Done()
		f.p.subsMu.Lock()
		defer f.p.subsMu.Unlock()
		for i, c := range f.p.clientSubs {
			if c == ch {
				f.p.clientSubs = append(f.p.clientSubs[:i], f.p.clientSubs[i+1:]...)
				close(ch)
				return
			}
		}
	}()
	return ch
}

// ----------------------------------------------------------------------
// Compile-time assertions — fast feedback when a Provider[K, V]
// signature drifts.
// ----------------------------------------------------------------------

var (
	_ provider.Provider[SessionKey, Session] = sessionsFacet{}
	_ provider.Provider[WindowKey, Window]   = windowsFacet{}
	_ provider.Provider[PaneKey, Pane]       = panesFacet{}
	_ provider.Provider[ClientKey, Client]   = clientsFacet{}
)

// ----------------------------------------------------------------------
// Subscribe fanout. Caller MUST NOT hold a store lock — the send is
// best-effort but the goroutine still races other producers, so we want
// minimum critical sections.
// ----------------------------------------------------------------------

func (p *Provider) fanoutSession(ev provider.InvalidationEvent[SessionKey]) {
	p.subsMu.Lock()
	subs := append([]chan provider.InvalidationEvent[SessionKey](nil), p.sessionSubs...)
	p.subsMu.Unlock()
	for _, c := range subs {
		select {
		case c <- ev:
		default:
		}
	}
}

func (p *Provider) fanoutWindow(ev provider.InvalidationEvent[WindowKey]) {
	p.subsMu.Lock()
	subs := append([]chan provider.InvalidationEvent[WindowKey](nil), p.windowSubs...)
	p.subsMu.Unlock()
	for _, c := range subs {
		select {
		case c <- ev:
		default:
		}
	}
}

func (p *Provider) fanoutPane(ev provider.InvalidationEvent[PaneKey]) {
	p.subsMu.Lock()
	subs := append([]chan provider.InvalidationEvent[PaneKey](nil), p.paneSubs...)
	p.subsMu.Unlock()
	for _, c := range subs {
		select {
		case c <- ev:
		default:
		}
	}
}

func (p *Provider) fanoutClient(ev provider.InvalidationEvent[ClientKey]) {
	p.subsMu.Lock()
	subs := append([]chan provider.InvalidationEvent[ClientKey](nil), p.clientSubs...)
	p.subsMu.Unlock()
	for _, c := range subs {
		select {
		case c <- ev:
		default:
		}
	}
}

// ----------------------------------------------------------------------
// Equality helpers used by Store.ReplaceAll to compute the "changed"
// set per tick. Comparing every field would force the watcher to fan
// out a no-op on every poll; comparing the fields a client cares about
// (state + activity timestamps) keeps fanout proportional to real change.
// ----------------------------------------------------------------------

func sessionsEqual(a, b Session) bool {
	return a.Key == b.Key &&
		a.Attached == b.Attached &&
		a.AttachedCount == b.AttachedCount &&
		a.WindowCount == b.WindowCount &&
		a.CurrentWindow == b.CurrentWindow &&
		a.LastActivityAt.Equal(b.LastActivityAt) &&
		a.CreatedAt.Equal(b.CreatedAt)
}

func windowsEqual(a, b Window) bool {
	return a.Key == b.Key &&
		a.Name == b.Name &&
		a.Active == b.Active &&
		a.PaneCount == b.PaneCount &&
		a.CurrentPane == b.CurrentPane
}

func panesEqual(a, b Pane) bool {
	return a.Key == b.Key &&
		a.WindowKey == b.WindowKey &&
		a.Title == b.Title &&
		a.CurrentCommand == b.CurrentCommand &&
		a.CurrentPid == b.CurrentPid &&
		a.Width == b.Width &&
		a.Height == b.Height &&
		a.Dead == b.Dead
}

func clientsEqual(a, b Client) bool {
	return a.Key == b.Key &&
		a.Session == b.Session &&
		a.TTY == b.TTY &&
		a.Hostname == b.Hostname &&
		a.TermName == b.TermName &&
		a.Readonly == b.Readonly &&
		a.CurrentWindow == b.CurrentWindow &&
		a.CurrentPane == b.CurrentPane &&
		a.AttachedAt.Equal(b.AttachedAt) &&
		a.LastActivityAt.Equal(b.LastActivityAt)
}
