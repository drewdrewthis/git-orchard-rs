// Package store provides the per-provider in-memory cache. Per ADR-011
// §4, every provider owns one Store[K, V]: a map plus per-entry
// freshness metadata. Hydrated by the watcher; expired by TTL backstop
// in the provider, not here.
//
// **No SQLite. No event log. No persistence here.** Snapshot persistence
// is opt-in per provider via the snapshot package (not in this PR).
package store

import (
	"sync"
	"time"

	provider "github.com/drewdrewthis/orchardist/internal/server/adapter"
)

// Store is a generic in-memory map of K → V plus per-entry freshness.
// Safe for concurrent use.
type Store[K provider.Key, V any] struct {
	mu      sync.RWMutex
	entries map[K]entry[V]
}

type entry[V any] struct {
	value     V
	freshness provider.Freshness
}

// New returns an empty store ready for use.
func New[K provider.Key, V any]() *Store[K, V] {
	return &Store[K, V]{entries: make(map[K]entry[V])}
}

// Put writes a value with freshness metadata.
func (s *Store[K, V]) Put(key K, value V, source provider.FreshnessSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = entry[V]{
		value: value,
		freshness: provider.Freshness{
			LastFetchedAt: time.Now(),
			Source:        source,
		},
	}
}

// Get reads a value plus freshness. The boolean is false if the key is
// not present (callers distinguish miss from "value is the zero value").
func (s *Store[K, V]) Get(key K) (V, provider.Freshness, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[key]
	if !ok {
		var zero V
		return zero, provider.Freshness{}, false
	}
	return e.value, e.freshness, true
}

// Keys returns every key currently in the store. Order is unspecified.
func (s *Store[K, V]) Keys() []K {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]K, 0, len(s.entries))
	for k := range s.entries {
		out = append(out, k)
	}
	return out
}

// Snapshot returns a copy of every entry. Callers can iterate without
// holding the store lock. Order is unspecified.
func (s *Store[K, V]) Snapshot() map[K]V {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[K]V, len(s.entries))
	for k, e := range s.entries {
		out[k] = e.value
	}
	return out
}

// Filter returns a copy of every value whose entry satisfies match. It
// allocates only for the values it returns, so a narrow read never pays
// the whole-population clone Snapshot() does — the #612 hot-path
// allocation. Order is unspecified; the result is nil when nothing
// matches.
//
// match runs while the read lock is held. It must be pure and must not
// call back into this store: sync.RWMutex is not reentrant, so a nested
// read deadlocks against a ReplaceAll waiting for the write lock.
func (s *Store[K, V]) Filter(match func(K, V) bool) []V {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []V
	for k, e := range s.entries {
		if match(k, e.value) {
			out = append(out, e.value)
		}
	}
	return out
}

// ReplaceAll atomically swaps the entire population to `next`, marking
// every entry with the given source. Keys present in the prior
// population but absent from `next` are evicted. Returns the set of
// keys that changed (added, removed, or value-modified per the
// `equals` predicate). Callers fan these out as InvalidationEvents.
func (s *Store[K, V]) ReplaceAll(next map[K]V, source provider.FreshnessSource, equals func(a, b V) bool) []K {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	changed := make([]K, 0)
	// Additions and modifications.
	for k, v := range next {
		prior, ok := s.entries[k]
		if !ok || !equals(prior.value, v) {
			changed = append(changed, k)
		}
		s.entries[k] = entry[V]{
			value: v,
			freshness: provider.Freshness{
				LastFetchedAt: now,
				Source:        source,
			},
		}
	}
	// Removals.
	for k := range s.entries {
		if _, ok := next[k]; !ok {
			delete(s.entries, k)
			changed = append(changed, k)
		}
	}
	return changed
}
