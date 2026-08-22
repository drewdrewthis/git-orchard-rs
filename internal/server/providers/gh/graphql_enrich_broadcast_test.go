package gh_test

// T6 regression tests for enrichment-driven invalidation broadcasts (R16/M7).
//
// The gh provider exposes Provider.Subscribe and the schema declares
// Subscription.pullRequestChanged, but nothing in production ever emitted an
// invalidation: the only emitters lived behind the unmounted webhook handler.
// Every enrichment refresh that DID reach the cache went silent, so any
// subscriber sat on a dead channel while clients polled.
//
// The fix emits an invalidation event AFTER each successful enrichment cache
// write, keyed exactly as the webhook handler and the subscription resolvers
// format it: "PullRequest:<owner>/<name>#<number>".
//
// No PII fixtures: repos are `alice/repo`.

import (
	"context"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
)

func TestEnrichmentEmitsInvalidationAfterCacheWrite(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}

	body := enrichResponse("MERGEABLE", "CLEAN", nil, "SUCCESS", "bug")
	p, count := newEnrichProvider(t, body, clock.Now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := p.Subscribe(ctx)

	key := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 42}

	if _, err := p.EnrichPullRequest(context.Background(), key); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	// R16: by the time the subscriber sees the event, the cache write that
	// caused it must already be visible.
	select {
	case ev := <-events:
		want := "PullRequest:alice/repo#42"
		if ev.Key != want {
			t.Errorf("event key = %q, want %q (must match webhook + resolver format)", ev.Key, want)
		}
		if ev.Reason == "" {
			t.Errorf("event reason empty, want e.g. %q", "enrich")
		}
		if ts := p.ExportEnrichTimestamp(key); ts.IsZero() {
			t.Error("event arrived but enrichAt[key] is zero — emitted BEFORE the cache write (R16 violation)")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no invalidation event within 2s of a cached enrichment — emitter still missing")
	}

	if c := count.Load(); c != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", c)
	}
}

func TestEnrichmentWithoutCacheWriteEmitsNothing(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}

	// Open PR + UNKNOWN mergeable => #367 contract says never cached, so
	// nothing definitive changed and nothing may be broadcast.
	body := enrichResponse("UNKNOWN", "UNKNOWN", nil, "")
	p, _ := newEnrichProvider(t, body, clock.Now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := p.Subscribe(ctx)

	key := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 7}
	p.ExportSeedPRState(key, gh.PullRequestStateOpen)

	if _, err := p.EnrichPullRequest(context.Background(), key); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	select {
	case ev := <-events:
		t.Errorf("unexpected event for uncached enrichment: key=%q reason=%q", ev.Key, ev.Reason)
	case <-time.After(300 * time.Millisecond):
		// correct: silence
	}
}

func TestBatchEnrichmentEmitsPerWrittenKey(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}

	p, count := newEnrichProvider(t, batchEnrichResponse(2, "MERGEABLE"), clock.Now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := p.Subscribe(ctx)

	k1 := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 1}
	k2 := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 2}
	p.ExportSeedPRState(k1, gh.PullRequestStateMerged)
	p.ExportSeedPRState(k2, gh.PullRequestStateClosed)

	keys := []gh.PullRequestKey{k1, k2}
	if _, err := p.BatchEnrichPullRequests(context.Background(), keys); err != nil {
		t.Fatalf("batch enrich: %v", err)
	}

	got := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-events:
			got[ev.Key] = true
		case <-deadline:
			t.Fatalf("got %d batch events (%v), want 2 — one per written key", len(got), got)
		}
	}
	for _, want := range []string{"PullRequest:alice/repo#1", "PullRequest:alice/repo#2"} {
		if !got[want] {
			t.Errorf("missing event for %q; got %v", want, got)
		}
	}
	if c := count.Load(); c != 1 {
		t.Fatalf("expected 1 HTTP call for the whole batch, got %d", c)
	}
}
