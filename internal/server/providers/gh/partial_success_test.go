package gh_test

// Regression tests for GraphQL partial-success handling in the gh provider.
//
// A GraphQL 200 may carry BOTH populated data and a non-empty errors array:
// some leaves failed while others resolved. Treating that as a total failure
// discarded every resolved field over one failed leaf -- the daemon-side twin
// of the sidebar bug fixed in #737. Only a response with NO usable data is a
// total failure.
//
// Boundary preserved: data-absent responses keep erroring (see
// TestEnrichPullRequest_TotalFailureStillErrors and the pre-existing
// TestEnrichPullRequest_GraphQLError).
//
// No PII fixtures: repos are `alice/repo`.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
)

const rlMsg = "github API rate limited; resets at 1786742279"

// withErrors marshals body back with one errors[] entry injected.
func withErrors(t *testing.T, body []byte, msg string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("fixture unmarshal: %v", err)
	}
	m["errors"] = []any{map[string]any{"message": msg}}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("fixture remarshal: %v", err)
	}
	return out
}

func TestEnrichPullRequest_PartialErrorsKeepPopulatedData(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}

	body := withErrors(t, enrichResponse("MERGEABLE", "CLEAN", nil, "SUCCESS", "bug"), rlMsg)
	p, count := newEnrichProvider(t, body, clock.Now)

	key := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 42}
	pr, err := p.EnrichPullRequest(context.Background(), key)
	if err != nil {
		t.Fatalf("partial success returned error, want populated data: %v", err)
	}
	if pr.Mergeable != gh.MergeableStateMergeable {
		t.Errorf("mergeable = %q, want the resolved leaf kept (MERGEABLE)", pr.Mergeable)
	}
	if p.ExportEnrichTimestamp(key).IsZero() {
		t.Error("definitive enrichment from a partial response was not cached")
	}
	if c := count.Load(); c != 1 {
		t.Errorf("HTTP calls = %d, want 1", c)
	}
}

func TestBatchEnrichPullRequests_PartialErrorsKeepPopulatedData(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}

	body := withErrors(t, batchEnrichResponse(2, "MERGEABLE"), rlMsg)
	p, count := newEnrichProvider(t, body, clock.Now)

	k1 := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 1}
	k2 := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 2}
	p.ExportSeedPRState(k1, gh.PullRequestStateMerged)
	p.ExportSeedPRState(k2, gh.PullRequestStateClosed)

	if _, err := p.BatchEnrichPullRequests(context.Background(), []gh.PullRequestKey{k1, k2}); err != nil {
		t.Fatalf("batch partial success returned error, want populated aliases: %v", err)
	}
	for _, k := range []gh.PullRequestKey{k1, k2} {
		if p.ExportEnrichTimestamp(k).IsZero() {
			t.Errorf("key %v not cached despite resolving in a partial response", k)
		}
	}
	if c := count.Load(); c != 1 {
		t.Errorf("HTTP calls = %d, want 1", c)
	}
}

func TestEnrichIssueDependencies_PartialErrorsKeepPopulatedData(t *testing.T) {
	deps := `{
		"data": {"repository": {"issue": {
			"blockedByIssues": {"nodes": [
				{"number": 558, "title": "blocked-by-558", "repository": {"owner": {"login":"alice"}, "name":"repo"}}
			]},
			"blockingIssues": {"nodes": []},
			"subIssues": {"nodes": []},
			"parent": null
		}}}
	}`
	p, _, _ := newDepsProvider(t, withErrors(t, []byte(deps), rlMsg), time.Now)

	depsOut, err := p.EnrichIssueDependencies(context.Background(), gh.IssueKey{Owner: "alice", Name: "repo", Number: 544})
	if err != nil {
		t.Fatalf("deps partial success returned error, want populated edges: %v", err)
	}
	if len(depsOut.BlockedBy) != 1 || depsOut.BlockedBy[0].Number != 558 {
		t.Errorf("BlockedBy = %+v, want [#558]", depsOut.BlockedBy)
	}
}

func TestEnrichPullRequest_TotalFailureStillErrors(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"data":   nil,
		"errors": []any{map[string]any{"message": rlMsg}},
	})
	p, _ := newEnrichProvider(t, body, time.Now)

	_, err := p.EnrichPullRequest(context.Background(), gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 42})
	if err == nil {
		t.Fatal("errors with NO data must remain an error (total failure)")
	}
}
