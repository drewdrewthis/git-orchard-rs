package claudeprojects

import (
	"context"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/adapter"
	"github.com/drewdrewthis/orchardist/internal/server/graphql"
)

// TestToGraphQLRecapSource_MapsToValidEnum asserts that every provider
// RecapSource value maps, via toGraphQLRecapSource, to a graphql.RecapSource
// for which IsValid() is true. toGraphQLRecapSource casts the provider's
// string enum across packages unguarded, so a value added to one side
// without the other would silently produce an invalid wire value.
func TestToGraphQLRecapSource_MapsToValidEnum(t *testing.T) {
	all := []RecapSource{RecapSourceCommand, RecapSourceAwaySummary}
	for _, s := range all {
		s := s
		got := toGraphQLRecapSource(&s)
		if got == nil {
			t.Fatalf("toGraphQLRecapSource(%q) = nil; want non-nil", s)
		}
		if !graphql.RecapSource(*got).IsValid() {
			t.Errorf("toGraphQLRecapSource(%q) = %q; not a valid graphql.RecapSource", s, *got)
		}
	}
}

// TestToGraphQL_JsonlPath asserts that ToGraphQL maps the in-memory
// Conversation.Path field onto the wire-level JsonlPath. This is the
// AC1 unit assertion: "jsonlPath resolver maps an in-memory Conversation
// to its on-disk path".
func TestToGraphQL_JsonlPath(t *testing.T) {
	const wantPath = "/Users/alice/.claude/projects/foo/bar.jsonl"
	const sessionUUID = "00000000-aaaa-bbbb-cccc-dddddddddddd"

	p := NewWith(nil, nil, time.Now, HeartbeatThreshold)

	c := Conversation{
		ID:           ConversationID{HostID: "test-host", SessionUUID: sessionUUID},
		Path:         wantPath,
		MessageCount: 1,
	}

	got := p.ToGraphQL(c)
	if got == nil {
		t.Fatal("ToGraphQL returned nil")
	}
	if got.JsonlPath != wantPath {
		t.Errorf("JsonlPath = %q, want %q", got.JsonlPath, wantPath)
	}
	// Sanity-check other fields are still populated.
	if got.SessionUUID != sessionUUID {
		t.Errorf("SessionUUID = %q, want %q", got.SessionUUID, sessionUUID)
	}
}

// TestToGraphQL_JsonlPath_Empty asserts that an empty Path produces an
// empty JsonlPath (not a panic or nil pointer).
func TestToGraphQL_JsonlPath_Empty(t *testing.T) {
	p := NewWith(nil, nil, time.Now, HeartbeatThreshold)

	c := Conversation{
		ID:   ConversationID{HostID: "test-host", SessionUUID: "empty-path-uuid"},
		Path: "",
	}

	got := p.ToGraphQL(c)
	if got == nil {
		t.Fatal("ToGraphQL returned nil")
	}
	if got.JsonlPath != "" {
		t.Errorf("JsonlPath = %q, want empty string for empty Path", got.JsonlPath)
	}
}

// TestPathForSessionUUID_Hit asserts that PathForSessionUUID finds a
// conversation by its session UUID in the provider's in-memory cache.
func TestPathForSessionUUID_Hit(t *testing.T) {
	const uuid = "test-uuid-001"
	const path = "/tmp/test.jsonl"

	p := NewWith(nil, nil, time.Now, HeartbeatThreshold)

	// Seed the cache directly (bypassing the adapter) so the test
	// does not need a real filesystem.
	id := ConversationID{HostID: "test-host", SessionUUID: uuid}
	p.cachePut(id, Conversation{ID: id, Path: path}, adapter.Freshness{})

	got, ok := p.PathForSessionUUID(context.Background(), uuid)
	if !ok {
		t.Fatalf("PathForSessionUUID returned ok=false, want true")
	}
	if got != path {
		t.Errorf("PathForSessionUUID = %q, want %q", got, path)
	}
}

// TestPathForSessionUUID_Miss asserts that PathForSessionUUID returns
// ("", false) when the session UUID is not in the cache.
func TestPathForSessionUUID_Miss(t *testing.T) {
	p := NewWith(nil, nil, time.Now, HeartbeatThreshold)

	got, ok := p.PathForSessionUUID(context.Background(), "nonexistent-uuid")
	if ok {
		t.Fatalf("PathForSessionUUID returned ok=true for unknown uuid, path=%q", got)
	}
	if got != "" {
		t.Errorf("PathForSessionUUID = %q, want empty string on miss", got)
	}
}
