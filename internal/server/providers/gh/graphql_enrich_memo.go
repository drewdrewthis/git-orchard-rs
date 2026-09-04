// Package gh — request-scoped PR enrichment memo (#813).
//
// The memo is a per-GraphQL-operation view of enrichment results, distinct
// from the durable per-key cache (p.prs / p.enrichAt). It is installed once
// per query/mutation operation via WithEnrichMemo (from the operation
// middleware) and discarded when the operation ends, so it never hardens a
// transient value across a websocket connection the way the connection-scoped
// loaders would.
//
// Why it exists: shouldCacheEnrichment correctly refuses to persist an open
// PR's UNKNOWN mergeable in the durable cache — UNKNOWN is uncomputed, not a
// verdict (#367). Without the memo, primeEnrichment's warm-up fetch leaves
// those keys cold, so the per-field dataloader batch re-fetches them: a second
// GitHub round-trip inside one operation (a hidden N+1). The memo captures the
// warm-up result for the life of the operation, so the field resolvers read it
// instead of the network, while the durable cache's #367 decline rule stays
// untouched.
package gh

import (
	"context"
	"sync"
)

// enrichMemo holds one operation's enrichment results. Concurrency-safe:
// gqlgen runs field resolvers on their own goroutines, so reads and writes
// race unless serialized.
type enrichMemo struct {
	mu sync.Mutex
	m  map[PullRequestKey]PullRequest
}

// enrichMemoKey is the private context key for the per-operation memo.
type enrichMemoKey struct{}

// WithEnrichMemo returns a child context carrying a fresh, empty enrichment
// memo. The operation middleware calls this once per query/mutation operation.
// Non-GraphQL callers (warm-up loops, subscription emissions) never install
// one, so the enrichment methods treat an absent memo as a no-op passthrough.
func WithEnrichMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, enrichMemoKey{}, &enrichMemo{m: map[PullRequestKey]PullRequest{}})
}

// enrichMemoFromContext returns the operation's memo, or nil when none is
// installed. A nil memo's get/put are no-ops, so callers need not nil-check.
func enrichMemoFromContext(ctx context.Context) *enrichMemo {
	m, _ := ctx.Value(enrichMemoKey{}).(*enrichMemo)
	return m
}

// get returns the memoized enrichment for key. A nil receiver is a miss.
func (m *enrichMemo) get(key PullRequestKey) (PullRequest, bool) {
	if m == nil {
		return PullRequest{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.m[key]
	return v, ok
}

// put records the enrichment for key. A nil receiver is a no-op.
func (m *enrichMemo) put(key PullRequestKey, pr PullRequest) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[key] = pr
}
