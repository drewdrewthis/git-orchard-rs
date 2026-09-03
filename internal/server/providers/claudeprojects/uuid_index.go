package claudeprojects

import "context"

// uuidIndex maps a SessionUUID onto the ConversationID that owns it, so
// the by-uuid lookups do not scan the cache.
//
// The cache stays keyed by the full ConversationID because federation
// (post-v1) needs HostID to disambiguate; this index is the secondary
// axis over it.
//
// NOT safe for concurrent use on its own. It must stay atomically
// consistent with Provider.cache, so the Provider's mutex guards both —
// a second lock in here would allow a torn read between them.
type uuidIndex map[string]ConversationID

// newUUIDIndex builds an index over an entire cache snapshot. Used
// wherever the cache is replaced wholesale rather than mutated.
func newUUIDIndex(cache map[ConversationID]Conversation) uuidIndex {
	ix := make(uuidIndex, len(cache))
	for id := range cache {
		ix.put(id)
	}
	return ix
}

// put records id under its session uuid. Two hosts sharing a uuid is
// possible in a federated deployment; last write wins, which matches the
// arbitrary-first-match the linear scan it replaced produced.
func (ix uuidIndex) put(id ConversationID) {
	ix[id.SessionUUID] = id
}

// drop removes id's uuid entry, but only when the entry still points at
// id. Without that check, removing one host's conversation would blank a
// same-uuid entry another host still owns.
func (ix uuidIndex) drop(id ConversationID) {
	if cur, ok := ix[id.SessionUUID]; ok && cur == id {
		delete(ix, id.SessionUUID)
	}
}

// get returns the ConversationID owning uuid, if any.
func (ix uuidIndex) get(uuid string) (ConversationID, bool) {
	id, ok := ix[uuid]
	return id, ok
}

// PathForSessionUUID returns the on-disk path for the conversation whose
// session UUID matches uuid. Returns ("", false) when the uuid is not
// currently known — the caller should infer nothing more than that, since
// the watcher may populate the cache later.
//
// O(1) via the uuid index. Locking mirrors cacheGet — RLock is sufficient
// because we only read.
func (p *Provider) PathForSessionUUID(_ context.Context, uuid string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.byUUID.get(uuid)
	if !ok {
		return "", false
	}
	c, ok := p.cache[id]
	if !ok {
		return "", false
	}
	return c.Path, true
}

// GetBySessionUUID returns the cached Conversation whose JSONL filename
// matches uuid (Claude Code names files by sessionId so this is the
// natural lookup key). Returns (zero, false) when not in cache. Used by
// the ClaudeInstance.conversation resolver to expose Conversation
// metadata without forcing a separate `conversations` query.
//
// O(1) via the uuid index. Locking mirrors PathForSessionUUID — RLock only.
func (p *Provider) GetBySessionUUID(_ context.Context, uuid string) (Conversation, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.byUUID.get(uuid)
	if !ok {
		return Conversation{}, false
	}
	c, ok := p.cache[id]
	if !ok {
		return Conversation{}, false
	}
	return c, true
}
