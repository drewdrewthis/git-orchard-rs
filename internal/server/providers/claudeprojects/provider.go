package claudeprojects

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/adapter"
	"github.com/drewdrewthis/orchardist/internal/server/graphql"
)

// HeartbeatThreshold is the cutoff for `open: Boolean!`. A
// Conversation is reported open when its last record was written
// within this window, computed against the provider's clock.
//
// Default 60 seconds — large enough that human typing pauses do not
// flicker the field, small enough that an abandoned session settles
// to closed within a minute. Tuneable via NewWith.
const HeartbeatThreshold = 60 * time.Second

// Provider exposes Conversation nodes to the GraphQL resolver layer.
//
// Owns:
//   - one FSAdapter (filesystem walk + jsonl parse).
//   - one fsnotify watcher (started by adapter.Watch).
//   - an in-memory cache keyed by ConversationID, plus a SessionUUID
//     index over it (uuid_index.go) for the by-uuid lookups.
//   - a fan-out for Subscribers.
//
// Per ADR-011 §2 the surface is read-only — writes happen out-of-band
// (Claude Code itself appends to the JSONL); the watcher turns those
// writes into invalidation events, and the next read picks up the
// fresh metadata.
type Provider struct {
	adapter   *FSAdapter
	logger    *slog.Logger
	clock     func() time.Time
	heartbeat time.Duration

	mu     sync.RWMutex
	cache  map[ConversationID]Conversation
	byUUID uuidIndex // SessionUUID -> ConversationID; see uuid_index.go
	fresh  map[ConversationID]adapter.Freshness
	loaded bool

	subMu sync.Mutex
	subs  map[chan adapter.InvalidationEvent[ConversationID]]struct{}

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// New constructs a Provider with sensible production defaults: real
// filesystem adapter, real wall-clock, default heartbeat threshold.
func New(root, hostID string, logger *slog.Logger) *Provider {
	return NewWith(NewFSAdapter(root, hostID, logger), logger, time.Now, HeartbeatThreshold)
}

// NewWith is the test-friendly constructor. Callers inject the adapter,
// clock, and heartbeat so unit tests can drive openness deterministically
// without touching the real filesystem clock.
func NewWith(a *FSAdapter, logger *slog.Logger, clock func() time.Time, heartbeat time.Duration) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = time.Now
	}
	if heartbeat <= 0 {
		heartbeat = HeartbeatThreshold
	}
	return &Provider{
		adapter:   a,
		logger:    logger,
		clock:     clock,
		heartbeat: heartbeat,
		cache:     map[ConversationID]Conversation{},
		byUUID:    uuidIndex{},
		fresh:     map[ConversationID]adapter.Freshness{},
		subs:      map[chan adapter.InvalidationEvent[ConversationID]]struct{}{},
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Start hydrates the cache from the adapter and launches the watcher
// goroutine. Subsequent calls are no-ops; one provider, one lifecycle.
func (p *Provider) Start(ctx context.Context) error {
	var startErr error
	p.startOnce.Do(func() {
		if err := p.reload(ctx, "boot", adapter.SourcePoll); err != nil {
			startErr = fmt.Errorf("hydrate claudeprojects cache: %w", err)
			return
		}
		ch, err := p.adapter.Watch(ctx)
		if err != nil {
			startErr = fmt.Errorf("start watcher: %w", err)
			return
		}
		go p.run(ctx, ch)
	})
	return startErr
}

// Stop tears down the watcher and any subscribers. Idempotent.
func (p *Provider) Stop() error {
	var err error
	p.stopOnce.Do(func() {
		close(p.stopCh)
		<-p.doneCh
		err = p.adapter.Close()
		p.subMu.Lock()
		for ch := range p.subs {
			close(ch)
			delete(p.subs, ch)
		}
		p.subMu.Unlock()
	})
	return err
}

// Get returns one conversation by ID plus its freshness. Cache hit is
// the common path; on miss the adapter reads only the affected file.
func (p *Provider) Get(ctx context.Context, key ConversationID) (Conversation, adapter.Freshness, error) {
	if v, f, ok := p.cacheGet(key); ok {
		return v, f, nil
	}
	v, err := p.adapter.Fetch(ctx, key)
	if err != nil {
		return Conversation{}, adapter.Freshness{}, err
	}
	f := adapter.Freshness{LastFetchedAt: p.clock(), Source: adapter.SourcePoll}
	p.cachePut(key, v, f)
	return v, f, nil
}

// GetMany is the DataLoader-friendly batch read. Unique keys hit the
// cache first; misses share a single FetchAll call.
func (p *Provider) GetMany(ctx context.Context, keys []ConversationID) (map[ConversationID]Conversation, map[ConversationID]adapter.Freshness, error) {
	out := make(map[ConversationID]Conversation, len(keys))
	freshness := make(map[ConversationID]adapter.Freshness, len(keys))

	missing := make([]ConversationID, 0)
	seen := make(map[ConversationID]struct{}, len(keys))
	for _, k := range keys {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		if v, f, ok := p.cacheGet(k); ok {
			out[k] = v
			freshness[k] = f
			continue
		}
		missing = append(missing, k)
	}
	if len(missing) == 0 {
		return out, freshness, nil
	}

	// One FetchAll covers all misses — the per-file cost dominates
	// regardless of whether we ask for one or many ids.
	all, err := p.adapter.FetchAll(ctx)
	if err != nil {
		return nil, nil, err
	}
	now := p.clock()
	for _, k := range missing {
		v, ok := all[k]
		if !ok {
			continue
		}
		f := adapter.Freshness{LastFetchedAt: now, Source: adapter.SourcePoll}
		p.cachePut(k, v, f)
		out[k] = v
		freshness[k] = f
	}
	return out, freshness, nil
}

// Keys returns every cached ConversationID. Cold boot returns an
// empty slice; the watcher hydrates the cache before any resolver
// calls in well-formed setups.
func (p *Provider) Keys(_ context.Context) ([]ConversationID, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]ConversationID, 0, len(p.cache))
	for k := range p.cache {
		out = append(out, k)
	}
	return out, nil
}

// List returns every cached Conversation, sorted descending by
// LastSeenAt (so the most-recently active conversation is first).
// Conversations with a nil LastSeenAt sort to the end. This is what
// the Query.conversations resolver calls.
func (p *Provider) List(_ context.Context) ([]Conversation, error) {
	p.mu.RLock()
	out := make([]Conversation, 0, len(p.cache))
	for _, v := range p.cache {
		out = append(out, v)
	}
	p.mu.RUnlock()

	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].LastSeenAt, out[j].LastSeenAt
		switch {
		case ti == nil && tj == nil:
			return out[i].ID.SessionUUID < out[j].ID.SessionUUID
		case ti == nil:
			return false
		case tj == nil:
			return true
		default:
			return ti.After(*tj)
		}
	})
	return out, nil
}

// Subscribe returns a buffered channel that receives invalidation
// events for as long as ctx is alive. Closing ctx (or calling Stop)
// cleans the subscription up.
func (p *Provider) Subscribe(ctx context.Context) <-chan adapter.InvalidationEvent[ConversationID] {
	ch := make(chan adapter.InvalidationEvent[ConversationID], 8)
	p.subMu.Lock()
	p.subs[ch] = struct{}{}
	p.subMu.Unlock()

	go func() {
		<-ctx.Done()
		p.subMu.Lock()
		if _, ok := p.subs[ch]; ok {
			delete(p.subs, ch)
			close(ch)
		}
		p.subMu.Unlock()
	}()
	return ch
}

// IsOpen returns whether a conversation is "open" (heartbeat) at the
// provider's current clock. Exported so the resolver can compute the
// flag at read time — the cache stores raw timestamps, the heartbeat
// horizon is a presentation concern.
//
// **doc-as-code**: a Conversation is reported open when its
// LastSeenAt timestamp is within HeartbeatThreshold (default 60s) of
// the provider's clock. Conversations with a nil LastSeenAt (an
// empty JSONL file) are reported closed. The provider does not
// inspect the Claude process — a long-quiet session whose process
// happens to still be running will still report closed once the
// JSONL falls silent.
func (p *Provider) IsOpen(c Conversation) bool {
	if c.LastSeenAt == nil {
		return false
	}
	return p.clock().Sub(*c.LastSeenAt) < p.heartbeat
}

// ToGraphQL maps an in-memory Conversation onto the wire-level
// graphql.Conversation type the resolver returns.
//
// `recap` is derived directly from the JSONL: the daemon scans for the
// latest `/recap` slash-command invocation and surfaces the
// `<local-command-stdout>` text it produced. No plugin involvement —
// the daemon owns the fold (see recap.go).
func (p *Provider) ToGraphQL(c Conversation) *graphql.Conversation {
	return &graphql.Conversation{
		ID:           c.ID.GraphQLID(),
		SessionUUID:  c.ID.SessionUUID,
		Cwd:          c.Cwd,
		FirstSeenAt:  c.FirstSeenAt,
		LastSeenAt:   c.LastSeenAt,
		MessageCount: c.MessageCount,
		Open:         p.IsOpen(c),
		Recap:        c.Recap,
		RecapSource:  toGraphQLRecapSource(c.RecapSource),
		JsonlPath:    c.Path,
		CustomTitle:  c.CustomTitle,
		AgentName:    c.AgentName,
	}
}

// toGraphQLRecapSource maps the provider's RecapSource onto the generated
// GraphQL enum. The string values are identical, so this is a plain
// conversion that preserves nil (recapSource is null exactly when recap
// is null).
func toGraphQLRecapSource(s *RecapSource) *graphql.RecapSource {
	if s == nil {
		return nil
	}
	g := graphql.RecapSource(*s)
	return &g
}

// Refresh forces a full re-walk of the projects root and updates the
// cache from disk. Useful in tests that want a deterministic read
// without relying on fsnotify timing — production callers should let
// the watcher do this work.
func (p *Provider) Refresh(ctx context.Context) error {
	return p.reload(ctx, "manual-refresh", adapter.SourcePoll)
}

// run drains the adapter's Watch channel, refreshes affected entries,
// and broadcasts invalidations to subscribers. Exits on ctx
// cancellation, on stopCh, or when the adapter closes the channel.
func (p *Provider) run(ctx context.Context, ch <-chan ConversationID) {
	defer close(p.doneCh)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case key, ok := <-ch:
			if !ok {
				return
			}
			if err := p.refreshOne(ctx, key); err != nil {
				p.logger.Warn("claudeprojects: refresh failed",
					"session_uuid", key.SessionUUID, "err", err)
			}
		}
	}
}

// refreshOne re-reads metadata for one conversation after a watcher
// event. The conversation's path is derived from the cache; if the
// cache has no entry yet (a brand-new session was created), we fall
// back to a FetchAll to discover and slot it.
//
// On fs.ErrNotExist (the file was removed), the entry is dropped
// from the cache and an invalidation event is broadcast.
func (p *Provider) refreshOne(ctx context.Context, key ConversationID) error {
	p.mu.RLock()
	cached, hit := p.cache[key]
	p.mu.RUnlock()

	now := p.clock()

	if !hit {
		// New conversation — reload everything. We could try to find
		// the path from the watcher event, but FetchAll handles the
		// case where multiple new files appeared in a burst.
		return p.reload(ctx, "watcher-create", adapter.SourceWatcherPush)
	}

	v, err := p.adapter.FetchOne(ctx, cached.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			p.mu.Lock()
			delete(p.cache, key)
			p.byUUID.drop(key)
			delete(p.fresh, key)
			p.mu.Unlock()
			p.broadcast([]ConversationID{key}, "watcher-remove", now)
			return nil
		}
		return err
	}
	f := adapter.Freshness{LastFetchedAt: now, Source: adapter.SourceWatcherPush}
	p.cachePut(key, v, f)
	p.broadcast([]ConversationID{key}, "watcher-write", now)
	return nil
}

// reload fetches every conversation, replaces the cache, and
// broadcasts invalidations for added/changed/removed keys.
func (p *Provider) reload(ctx context.Context, reason string, source adapter.FreshnessSource) error {
	all, err := p.adapter.FetchAll(ctx)
	if err != nil {
		return err
	}
	now := p.clock()

	p.mu.Lock()
	old := p.cache
	p.cache = all
	p.byUUID = newUUIDIndex(all)
	p.fresh = make(map[ConversationID]adapter.Freshness, len(all))
	for k := range all {
		p.fresh[k] = adapter.Freshness{LastFetchedAt: now, Source: source}
	}
	p.loaded = true
	p.mu.Unlock()

	changed := make([]ConversationID, 0)
	for k, v := range all {
		ov, had := old[k]
		if !had || !conversationsEqual(ov, v) {
			changed = append(changed, k)
		}
	}
	for k := range old {
		if _, still := all[k]; !still {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 && reason == "boot" {
		return nil
	}
	p.broadcast(changed, reason, now)
	return nil
}

// conversationsEqual returns true when two Conversations have
// identical metadata — pointer-derefs included. We use it to suppress
// invalidation broadcasts for no-op refreshes (a fsnotify Create that
// did not actually change anything we care about).
func conversationsEqual(a, b Conversation) bool {
	if a.ID != b.ID || a.Path != b.Path || a.MessageCount != b.MessageCount {
		return false
	}
	if !timesEqual(a.FirstSeenAt, b.FirstSeenAt) {
		return false
	}
	if !timesEqual(a.LastSeenAt, b.LastSeenAt) {
		return false
	}
	if !stringsEqual(a.Cwd, b.Cwd) {
		return false
	}
	return true
}

func timesEqual(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}

func stringsEqual(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// broadcast fans an InvalidationEvent out to every subscriber. The
// per-channel buffer is small; we drop on a full buffer rather than
// block the watcher goroutine — subscribers that fall behind miss
// events but stay alive.
func (p *Provider) broadcast(keys []ConversationID, reason string, at time.Time) {
	p.subMu.Lock()
	defer p.subMu.Unlock()
	for _, k := range keys {
		ev := adapter.InvalidationEvent[ConversationID]{Key: k, Reason: reason, At: at}
		for ch := range p.subs {
			select {
			case ch <- ev:
			default:
				p.logger.Warn("claudeprojects: subscriber lagging, dropping event",
					"session_uuid", k.SessionUUID)
			}
		}
	}
}

func (p *Provider) cacheGet(k ConversationID) (Conversation, adapter.Freshness, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	v, ok := p.cache[k]
	if !ok {
		return Conversation{}, adapter.Freshness{}, false
	}
	return v, p.fresh[k], true
}

func (p *Provider) cachePut(k ConversationID, v Conversation, f adapter.Freshness) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache[k] = v
	p.byUUID.put(k)
	p.fresh[k] = f
}
