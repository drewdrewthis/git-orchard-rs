package resolvers_test

// E2E coverage for the #813 enrichment memo's OPERATION scope, driven through
// the real production seam: two GraphQL operations on ONE websocket
// connection. The websocket connection captures a single connection-scoped
// *Loaders at open (loaders.Middleware wraps the whole /graphql handler), so
// holding the loader constant across both operations isolates the memo as the
// thing under test — a plain pair of HTTP POSTs would get a fresh *Loaders each
// time and could not distinguish "memo is per-operation" from "loader is
// per-request".
//
//	AC2 (pr-enrichment-unknown-mergeable.feature): the memo is request-scoped,
//	    not connection-scoped — each operation performs its own fetch.
//	AC4: after mergeable flips UNKNOWN→MERGEABLE, a fresh operation on the same
//	    socket sees the new value — the memo must not harden UNKNOWN.
//
// The websocket query path reuses the graphql-transport-ws helpers
// (dialSubscription/subscribe/startReader/readPayload) from
// nodechanged_e2e_test.go; gqlgen's websocket transport runs query operations
// one-shot, emitting one `next` frame with the response then `complete`.
//
// No PII fixtures: repo is `alice/repo`, users are `bob` and `carol`.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestPRReviewSurface_UnknownMergeable_MemoIsPerOperation runs two identical
// enrichment queries over one websocket connection against a set of open,
// UNKNOWN-mergeable PRs. The per-operation memo collapses each operation to a
// single GitHub GraphQL round-trip, but must NOT leak across operations: the
// call counter reads 1 after the first op and 2 after the second. A
// connection-scoped memo would leave it at 1.
func TestPRReviewSurface_UnknownMergeable_MemoIsPerOperation(t *testing.T) {
	api, graphqlCalls := stubUnknownMergeableReviewAPI(t, 3)
	srv := newReviewDaemon(t, newReviewProvider(t, api))

	conn := dialSubscription(t, stripScheme(t, srv.URL))
	defer func() { _ = conn.Close() }()
	frames := startReader(t, conn)

	const q = `query {
		pullRequests(repo: "alice/repo", state: OPEN) {
			number
			mergeable
			headRefOid
			reviews { authorLogin }
			reviewThreads { path }
		}
	}`

	// --- operation 1 ---
	subscribe(t, conn, "op1", q)
	var r1 map[string]any
	if err := readPayload(t, frames, "op1", 5*time.Second, &r1); err != nil {
		t.Fatalf("op1 payload: %v", err)
	}
	if errs, ok := r1["errors"]; ok {
		t.Fatalf("op1 graphql errors: %v", errs)
	}
	if got := graphqlCalls.Load(); got != 1 {
		t.Fatalf("after op1: GitHub GraphQL calls = %d, want 1 (memo collapses the op to one round-trip)", got)
	}

	// --- operation 2 on the SAME connection (shares one *Loaders) ---
	subscribe(t, conn, "op2", q)
	var r2 map[string]any
	if err := readPayload(t, frames, "op2", 5*time.Second, &r2); err != nil {
		t.Fatalf("op2 payload: %v", err)
	}
	if errs, ok := r2["errors"]; ok {
		t.Fatalf("op2 graphql errors: %v", errs)
	}
	if got := graphqlCalls.Load(); got != 2 {
		t.Fatalf("after op2 on the same connection: GitHub GraphQL calls = %d, want 2 (memo is per-operation, not connection-scoped, #813)", got)
	}
}

// TestPRReviewSurface_UnknownMergeable_FreshOperationSeesFlip proves the #367
// socket-safety contract through the same real seam: op1 observes an open PR
// with mergeable == UNKNOWN; the stub then flips to MERGEABLE; op2 on the SAME
// connection observes MERGEABLE. Because the memo dies with op1, it cannot
// serve the stale UNKNOWN to op2.
func TestPRReviewSurface_UnknownMergeable_FreshOperationSeesFlip(t *testing.T) {
	api, body := stubSwappableReviewAPI(t, 1, "UNKNOWN")
	srv := newReviewDaemon(t, newReviewProvider(t, api))

	conn := dialSubscription(t, stripScheme(t, srv.URL))
	defer func() { _ = conn.Close() }()
	frames := startReader(t, conn)

	const q = `query {
		pullRequests(repo: "alice/repo", state: OPEN) {
			number
			mergeable
		}
	}`

	// --- operation 1: mergeable is still being computed ---
	subscribe(t, conn, "op1", q)
	if got := mergeableFromWS(t, frames, "op1"); got != "UNKNOWN" {
		t.Fatalf("op1 mergeable = %q, want UNKNOWN", got)
	}

	// GitHub finished computing mergeability.
	body.set(reviewEnrichBody(t, 1, "MERGEABLE"))

	// --- operation 2 on the SAME connection: must see the fresh value ---
	subscribe(t, conn, "op2", q)
	if got := mergeableFromWS(t, frames, "op2"); got != "MERGEABLE" {
		t.Fatalf("op2 mergeable = %q, want MERGEABLE (memo must not harden UNKNOWN across the socket, #367/#813)", got)
	}
}

// mergeableFromWS reads the next `next` frame for subID and returns the first
// PR's `mergeable` enum value.
func mergeableFromWS(t *testing.T, frames <-chan frame, subID string) string {
	t.Helper()
	var resp map[string]any
	if err := readPayload(t, frames, subID, 5*time.Second, &resp); err != nil {
		t.Fatalf("%s payload: %v", subID, err)
	}
	if errs, ok := resp["errors"]; ok {
		t.Fatalf("%s graphql errors: %v", subID, errs)
	}
	data, _ := resp["data"].(map[string]any)
	prs, _ := data["pullRequests"].([]any)
	if len(prs) == 0 {
		t.Fatalf("%s: no pull requests in response: %v", subID, resp)
	}
	pr, _ := prs[0].(map[string]any)
	got, _ := pr["mergeable"].(string)
	return got
}

// swapReviewBody is a mutex-guarded GraphQL response body a stub can flip
// between operations — mirrors swappableBody in
// providers/gh/graphql_enrich_memo_test.go (a sibling package, so not shared).
type swapReviewBody struct {
	mu   sync.Mutex
	body []byte
}

func (s *swapReviewBody) set(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = b
}

func (s *swapReviewBody) get() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body
}

// reviewEnrichBody renders the aliased batch-enrichment body (`data.r0.pr0..`)
// for n PRs, every alias carrying the shared fixture with the given mergeable.
func reviewEnrichBody(t *testing.T, n int, mergeable string) []byte {
	t.Helper()
	fixture := reviewFixture(t)
	fixture["mergeable"] = mergeable
	prs := map[string]any{}
	for i := 0; i < n; i++ {
		prs[fmt.Sprintf("pr%d", i)] = fixture
	}
	body, err := json.Marshal(map[string]any{"data": map[string]any{"r0": prs}})
	if err != nil {
		t.Fatalf("marshal graphql fixture: %v", err)
	}
	return body
}

// stubSwappableReviewAPI is stubUnknownMergeableReviewAPI's twin whose GraphQL
// body can be flipped mid-test (returns the swapper instead of a call counter),
// so the #367 flip contract can be exercised across two operations.
func stubSwappableReviewAPI(t *testing.T, n int, initialMergeable string) (*httptest.Server, *swapReviewBody) {
	t.Helper()
	sb := &swapReviewBody{body: reviewEnrichBody(t, n, initialMergeable)}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/alice/repo/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, reviewPullsREST(n))
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(sb.get())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
		http.NotFound(w, r)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, sb
}
