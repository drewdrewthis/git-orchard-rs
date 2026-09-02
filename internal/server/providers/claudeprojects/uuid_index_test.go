package claudeprojects

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/adapter"
)

// seedCacheOnly writes an entry straight into p.cache without touching
// the uuid index. Any lookup that still finds it is scanning the cache
// rather than consulting the index.
func seedCacheOnly(p *Provider, c Conversation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache[c.ID] = c
}

// writeTranscripts creates <root>/<project>/<uuid>.jsonl for each uuid.
// Empty files are legitimate transcripts (a session that has not written
// a record yet), so the fixture stays independent of the record schema.
func writeTranscripts(t *testing.T, root, project string, uuids ...string) {
	t.Helper()
	dir := filepath.Join(root, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, u := range uuids {
		p := filepath.Join(dir, u+".jsonl")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
}

// TestPathForSessionUUID_UsesIndexNotCacheScan asserts the lookup reads
// the uuid index rather than iterating p.cache. An entry present in the
// cache but absent from the index must not be found — under the old
// linear scan it would be.
func TestPathForSessionUUID_UsesIndexNotCacheScan(t *testing.T) {
	p := NewWith(nil, nil, time.Now, HeartbeatThreshold)
	id := ConversationID{HostID: "test-host", SessionUUID: "unindexed-uuid"}
	seedCacheOnly(p, Conversation{ID: id, Path: "/tmp/unindexed.jsonl"})

	if got, ok := p.PathForSessionUUID(context.Background(), id.SessionUUID); ok {
		t.Errorf("PathForSessionUUID = (%q, true) for an unindexed entry; want a miss (lookup is scanning the cache)", got)
	}
}

// TestGetBySessionUUID_UsesIndexNotCacheScan is the sibling assertion for
// the Conversation-returning lookup, which shared the same O(N) scan.
func TestGetBySessionUUID_UsesIndexNotCacheScan(t *testing.T) {
	p := NewWith(nil, nil, time.Now, HeartbeatThreshold)
	id := ConversationID{HostID: "test-host", SessionUUID: "unindexed-uuid-2"}
	seedCacheOnly(p, Conversation{ID: id, Path: "/tmp/unindexed2.jsonl"})

	if _, ok := p.GetBySessionUUID(context.Background(), id.SessionUUID); ok {
		t.Error("GetBySessionUUID hit for an unindexed entry; want a miss (lookup is scanning the cache)")
	}
}

// TestPathForSessionUUID_CachePutIndexesEntry asserts the ordinary
// cache-miss write path (Get / GetMany / refreshOne) feeds the index.
func TestPathForSessionUUID_CachePutIndexesEntry(t *testing.T) {
	p := NewWith(nil, nil, time.Now, HeartbeatThreshold)
	id := ConversationID{HostID: "test-host", SessionUUID: "cacheput-uuid"}
	const want = "/tmp/cacheput.jsonl"
	p.cachePut(id, Conversation{ID: id, Path: want}, adapter.Freshness{})

	got, ok := p.PathForSessionUUID(context.Background(), id.SessionUUID)
	if !ok {
		t.Fatal("PathForSessionUUID missed an entry written through cachePut")
	}
	if got != want {
		t.Errorf("PathForSessionUUID = %q, want %q", got, want)
	}
}

// TestUUIDIndex_StaysCorrectAcrossRefresh drives the provider over a real
// projects root and asserts the index tracks Refresh in both directions —
// transcripts that appear become findable, transcripts that disappear stop
// being findable. Refresh replaces p.cache wholesale, which is where an
// index maintained only in cachePut would go stale.
func TestUUIDIndex_StaysCorrectAcrossRefresh(t *testing.T) {
	root := t.TempDir()
	const keep = "11111111-1111-1111-1111-111111111111"
	const drop = "22222222-2222-2222-2222-222222222222"
	const added = "33333333-3333-3333-3333-333333333333"
	writeTranscripts(t, root, "project-a", keep, drop)

	p := NewWith(NewFSAdapter(root, "test-host", nil), nil, time.Now, HeartbeatThreshold)
	ctx := context.Background()
	if err := p.Refresh(ctx); err != nil {
		t.Fatalf("initial Refresh: %v", err)
	}

	for _, u := range []string{keep, drop} {
		got, ok := p.PathForSessionUUID(ctx, u)
		if !ok {
			t.Fatalf("PathForSessionUUID(%q) missed after the initial Refresh", u)
		}
		if want := filepath.Join(root, "project-a", u+".jsonl"); got != want {
			t.Errorf("PathForSessionUUID(%q) = %q, want %q", u, got, want)
		}
	}

	// A new session appears and an old transcript is deleted.
	writeTranscripts(t, root, "project-a", added)
	if err := os.Remove(filepath.Join(root, "project-a", drop+".jsonl")); err != nil {
		t.Fatalf("remove transcript: %v", err)
	}
	if err := p.Refresh(ctx); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}

	if _, ok := p.PathForSessionUUID(ctx, added); !ok {
		t.Errorf("PathForSessionUUID(%q) missed a transcript added before the refresh", added)
	}
	if got, ok := p.PathForSessionUUID(ctx, drop); ok {
		t.Errorf("PathForSessionUUID(%q) = (%q, true) after the transcript was deleted; want a miss", drop, got)
	}
	if _, ok := p.PathForSessionUUID(ctx, keep); !ok {
		t.Errorf("PathForSessionUUID(%q) missed a transcript that is still on disk", keep)
	}
}

// TestUUIDIndex_RefreshOneRemoval_DropsFromIndex covers the watcher's
// fs.ErrNotExist path, which deletes from p.cache directly rather than
// going through reload.
func TestUUIDIndex_RefreshOneRemoval_DropsFromIndex(t *testing.T) {
	root := t.TempDir()
	const uuid = "44444444-4444-4444-4444-444444444444"

	p := NewWith(NewFSAdapter(root, "test-host", nil), nil, time.Now, HeartbeatThreshold)
	id := ConversationID{HostID: "test-host", SessionUUID: uuid}
	// Point the cache at a path that does not exist, so refreshOne takes
	// the fs.ErrNotExist branch the watcher takes on a deleted transcript.
	p.cachePut(id, Conversation{ID: id, Path: filepath.Join(root, "gone.jsonl")}, adapter.Freshness{})
	if _, ok := p.PathForSessionUUID(context.Background(), uuid); !ok {
		t.Fatal("precondition: entry should be indexed before the removal")
	}

	if err := p.refreshOne(context.Background(), id); err != nil {
		t.Fatalf("refreshOne: %v", err)
	}

	if got, ok := p.PathForSessionUUID(context.Background(), uuid); ok {
		t.Errorf("PathForSessionUUID = (%q, true) after the watcher dropped the entry; want a miss", got)
	}
}

// TestUUIDIndex_DropKeepsAnotherHostsEntry guards the federation case the
// issue calls out for keeping the cache keyed by (HostID, SessionUUID):
// removing one host's conversation must not blank a same-uuid entry that
// another host still owns.
func TestUUIDIndex_DropKeepsAnotherHostsEntry(t *testing.T) {
	const uuid = "shared-uuid"
	a := ConversationID{HostID: "host-a", SessionUUID: uuid}
	b := ConversationID{HostID: "host-b", SessionUUID: uuid}

	ix := uuidIndex{}
	ix.put(a)
	ix.put(b) // b now owns the uuid slot

	ix.drop(a) // a is stale; b must survive

	got, ok := ix.get(uuid)
	if !ok {
		t.Fatalf("index lost %q after dropping a different host's entry", uuid)
	}
	if got != b {
		t.Errorf("index resolves %q to %+v, want %+v", uuid, got, b)
	}

	ix.drop(b)
	if _, ok := ix.get(uuid); ok {
		t.Error("index still resolves the uuid after its owning entry was dropped")
	}
}

// BenchmarkPathForSessionUUID measures the lookup as the cache grows. The
// per-op cost should be flat across N; the linear scan it replaces grew
// with it. The GUI's tail-watcher polls this per pane every few seconds.
func BenchmarkPathForSessionUUID(b *testing.B) {
	for _, n := range []int{100, 10000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			p := NewWith(nil, nil, time.Now, HeartbeatThreshold)
			for i := 0; i < n; i++ {
				id := ConversationID{HostID: "test-host", SessionUUID: fmt.Sprintf("uuid-%06d", i)}
				p.cachePut(id, Conversation{ID: id, Path: fmt.Sprintf("/tmp/%06d.jsonl", i)}, adapter.Freshness{})
			}
			// Look up the last-inserted uuid: worst case for a scan,
			// identical to any other key for the index.
			target := fmt.Sprintf("uuid-%06d", n-1)
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := p.PathForSessionUUID(ctx, target); !ok {
					b.Fatal("lookup missed a seeded uuid")
				}
			}
		})
	}
}
