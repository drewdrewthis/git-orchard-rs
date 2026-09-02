// Tests for outbound-GitHub-call logging (issue #749).
//
// **No PII fixtures.** Repos are `alice/repo`, users are `bob`.
// **No real API calls.** Every test drives the Client through a fake
// http.RoundTripper — no listener, no network, no rate-limit budget spent.
//
// The load-bearing assertion is the LEVEL: before #749 the only per-call
// logging here was Debug, which the daemon's slog.Default() dropped. These
// tests pin the call and cache lines at Info and the backoff decisions at Warn.

package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeTransport is the stand-in for GitHub's HTTPS surface. respond is called
// once per outbound request with the 1-based request ordinal so a test can
// script a multi-page Link chain.
type fakeTransport struct {
	respond func(n int, req *http.Request) (*http.Response, error)
	calls   int
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls++
	return f.respond(f.calls, req)
}

// jsonResponse builds a 200 response carrying body plus the supplied headers.
func jsonResponse(body string, headers map[string]string) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// logCapture is a slog logger writing JSON records into a buffer, plus the
// decoding helpers the assertions need.
type logCapture struct {
	buf    bytes.Buffer
	logger *slog.Logger
}

// newLogCapture returns a capture whose handler filters at level, so a test
// can prove a record survives Info (rather than only existing at Debug).
func newLogCapture(level slog.Level) *logCapture {
	c := &logCapture{}
	c.logger = slog.New(slog.NewJSONHandler(&c.buf, &slog.HandlerOptions{Level: level}))
	return c
}

// withMessage returns the captured records whose msg equals msg.
func (c *logCapture) withMessage(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("undecodable log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// newLoggedClient wires a Client to the fake transport and the capture.
func newLoggedClient(tr *fakeTransport, cap *logCapture) *Client {
	c := NewClient("https://api.github.test", "token")
	c.HTTP = &http.Client{Transport: tr}
	c.Logger = cap.logger
	return c
}

// newLoggedProvider builds a Provider whose lazily-created Client already
// talks to tr. Forcing the client build first is what makes the HTTP swap
// land on the instance the provider will actually use.
func newLoggedProvider(t *testing.T, tr *fakeTransport, cap *logCapture, clock func() time.Time) *Provider {
	t.Helper()
	p := NewWith(cap.logger, "https://api.github.test", &StaticAuthSource{TokenValue: "token"}, clock)
	if _, err := p.httpClient(context.Background()); err != nil {
		t.Fatalf("build client: %v", err)
	}
	p.client.HTTP = &http.Client{Transport: tr}
	return p
}

// requireOne fails unless exactly one record with msg was captured.
func requireOne(t *testing.T, cap *logCapture, msg string) map[string]any {
	t.Helper()
	recs := cap.withMessage(t, msg)
	if len(recs) != 1 {
		t.Fatalf("got %d %q records, want exactly 1; captured:\n%s", len(recs), msg, cap.buf.String())
	}
	return recs[0]
}

// TestClientLogsOneInfoLinePerRESTCall is the headline #749 assertion for the
// single-shot REST path: one Info record per outbound call, carrying the call
// kind, endpoint, repo, duration and remaining rate-limit budget.
func TestClientLogsOneInfoLinePerRESTCall(t *testing.T) {
	t.Parallel()

	tr := &fakeTransport{respond: func(_ int, _ *http.Request) (*http.Response, error) {
		return jsonResponse(`{"default_branch":"main"}`, map[string]string{
			"X-RateLimit-Remaining": "4321",
		}), nil
	}}
	cap := newLogCapture(slog.LevelInfo)
	c := newLoggedClient(tr, cap)

	if _, err := c.GetRepo(context.Background(), "alice", "repo"); err != nil {
		t.Fatalf("GetRepo: %v", err)
	}

	rec := requireOne(t, cap, callLogMessage)
	if rec["level"] != "INFO" {
		t.Errorf("call log level = %v, want INFO — Debug lines are dropped by the daemon's default level (#749)", rec["level"])
	}
	if got, want := rec["kind"], callKindREST; got != want {
		t.Errorf("kind = %v, want %v", got, want)
	}
	if got, want := rec["endpoint"], "/repos/alice/repo"; got != want {
		t.Errorf("endpoint = %v, want %v", got, want)
	}
	if got, want := rec["repo"], "alice/repo"; got != want {
		t.Errorf("repo = %v, want %v", got, want)
	}
	if got, want := rec["rate_limit_remaining"], float64(4321); got != want {
		t.Errorf("rate_limit_remaining = %v, want %v", got, want)
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Errorf("call log has no duration_ms attribute; got %v", rec)
	}
}

// TestClientLogsOneInfoLinePerPaginatedPage asserts each page of a paginated
// list is its own log line. Each page is a separate request against the same
// rate-limit budget, so collapsing them would under-report call volume.
func TestClientLogsOneInfoLinePerPaginatedPage(t *testing.T) {
	t.Parallel()

	tr := &fakeTransport{respond: func(n int, _ *http.Request) (*http.Response, error) {
		headers := map[string]string{"X-RateLimit-Remaining": fmt.Sprintf("%d", 100-n)}
		if n == 1 {
			headers["Link"] = `<https://api.github.test/repos/alice/repo/pulls?page=2>; rel="next"`
		}
		return jsonResponse(`[]`, headers), nil
	}}
	cap := newLogCapture(slog.LevelInfo)
	c := newLoggedClient(tr, cap)

	if _, err := c.ListPulls(context.Background(), "alice", "repo", PullRequestStateOpen); err != nil {
		t.Fatalf("ListPulls: %v", err)
	}
	if tr.calls != 2 {
		t.Fatalf("fake transport saw %d requests, want 2 (the Link chain has two pages)", tr.calls)
	}

	recs := cap.withMessage(t, callLogMessage)
	if len(recs) != 2 {
		t.Fatalf("got %d %q records, want 1 per page (2); captured:\n%s", len(recs), callLogMessage, cap.buf.String())
	}
	for i, rec := range recs {
		if got, want := rec["kind"], callKindRESTPage; got != want {
			t.Errorf("record %d kind = %v, want %v", i, got, want)
		}
		if got, want := rec["page"], float64(i+1); got != want {
			t.Errorf("record %d page = %v, want %v", i, got, want)
		}
	}
}

// TestClientLogsOneInfoLinePerGraphQLCall asserts the GraphQL surface is
// audited too, with the repo lifted from the query variables since the
// endpoint path is always /graphql.
func TestClientLogsOneInfoLinePerGraphQLCall(t *testing.T) {
	t.Parallel()

	tr := &fakeTransport{respond: func(_ int, _ *http.Request) (*http.Response, error) {
		return jsonResponse(`{"data":{}}`, map[string]string{"X-RateLimit-Remaining": "17"}), nil
	}}
	cap := newLogCapture(slog.LevelInfo)
	c := newLoggedClient(tr, cap)

	vars := map[string]any{"owner": "alice", "name": "repo", "number": 7}
	if _, err := c.GraphQL(context.Background(), "query { viewer { login } }", vars); err != nil {
		t.Fatalf("GraphQL: %v", err)
	}

	rec := requireOne(t, cap, callLogMessage)
	if rec["level"] != "INFO" {
		t.Errorf("call log level = %v, want INFO", rec["level"])
	}
	if got, want := rec["kind"], callKindGraphQL; got != want {
		t.Errorf("kind = %v, want %v", got, want)
	}
	if got, want := rec["endpoint"], graphqlPath; got != want {
		t.Errorf("endpoint = %v, want %v", got, want)
	}
	if got, want := rec["repo"], "alice/repo"; got != want {
		t.Errorf("repo = %v, want %v", got, want)
	}
}

// TestClientLogsCallWhenTransportFails asserts a failed round trip still
// produces its call line — a failed call still consumed an attempt.
func TestClientLogsCallWhenTransportFails(t *testing.T) {
	t.Parallel()

	tr := &fakeTransport{respond: func(_ int, _ *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}}
	cap := newLogCapture(slog.LevelInfo)
	c := newLoggedClient(tr, cap)

	if _, err := c.GetRepo(context.Background(), "alice", "repo"); err == nil {
		t.Fatal("GetRepo returned nil error despite a failing transport")
	}

	rec := requireOne(t, cap, callLogMessage)
	if _, ok := rec["err"]; !ok {
		t.Errorf("failed call log has no err attribute; got %v", rec)
	}
}

// TestClientOmitsRateLimitAttrWhenHeaderAbsent asserts the attribute is not
// fabricated. A hard-coded 0 would read as "quota exhausted" in an audit.
func TestClientOmitsRateLimitAttrWhenHeaderAbsent(t *testing.T) {
	t.Parallel()

	tr := &fakeTransport{respond: func(_ int, _ *http.Request) (*http.Response, error) {
		return jsonResponse(`{"default_branch":"main"}`, nil), nil
	}}
	cap := newLogCapture(slog.LevelInfo)
	c := newLoggedClient(tr, cap)

	if _, err := c.GetRepo(context.Background(), "alice", "repo"); err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if v, ok := requireOne(t, cap, callLogMessage)["rate_limit_remaining"]; ok {
		t.Errorf("rate_limit_remaining = %v, want the attribute omitted when GitHub sent no header", v)
	}
}

// TestListPullRequestsLogsCacheHitAtInfo pins the O4 hit/miss pair at one
// level: a hit and the call line it replaced must both be visible in the same
// log, otherwise a call-volume audit cannot tell a quiet daemon from a
// well-cached one.
func TestListPullRequestsLogsCacheHitAtInfo(t *testing.T) {
	t.Parallel()

	tr := &fakeTransport{respond: func(_ int, _ *http.Request) (*http.Response, error) {
		return jsonResponse(`[{"number":7,"title":"t","state":"open"}]`, nil), nil
	}}
	cap := newLogCapture(slog.LevelInfo)
	p := newLoggedProvider(t, tr, cap, nil)

	ctx := context.Background()
	if _, err := p.ListPullRequests(ctx, "alice", "repo", PullRequestStateOpen); err != nil {
		t.Fatalf("first ListPullRequests: %v", err)
	}
	if _, err := p.ListPullRequests(ctx, "alice", "repo", PullRequestStateOpen); err != nil {
		t.Fatalf("second ListPullRequests: %v", err)
	}
	if tr.calls != 1 {
		t.Fatalf("fake transport saw %d requests, want 1 (the second read must hit cache)", tr.calls)
	}

	rec := requireOne(t, cap, cacheHitLogMessage)
	if rec["level"] != "INFO" {
		t.Errorf("cache hit log level = %v, want INFO", rec["level"])
	}
	if got, want := rec["op"], "ListPullRequests"; got != want {
		t.Errorf("op = %v, want %v", got, want)
	}
	if got, want := rec["repo"], "alice/repo"; got != want {
		t.Errorf("repo = %v, want %v", got, want)
	}
}

// TestEnrichPullRequestLogsCooldownAtWarn drives the GraphQL rate-limit path:
// a 200 carrying only errors[] must arm the cooldown AND surface it at Warn.
// The cooldown silently suppresses calls for minutes; at Debug that window was
// invisible and the sidebar just looked broken.
func TestEnrichPullRequestLogsCooldownAtWarn(t *testing.T) {
	t.Parallel()

	tr := &fakeTransport{respond: func(_ int, _ *http.Request) (*http.Response, error) {
		return jsonResponse(`{"errors":[{"message":"API rate limit exceeded for user ID 1"}]}`, nil), nil
	}}
	cap := newLogCapture(slog.LevelInfo)
	p := newLoggedProvider(t, tr, cap, nil)

	key := PullRequestKey{Owner: "alice", Name: "repo", Number: 7}
	if _, err := p.EnrichPullRequest(context.Background(), key); err == nil {
		t.Fatal("EnrichPullRequest returned nil error for a rate-limited response")
	}

	rec := requireOne(t, cap, cooldownLogMessage)
	if rec["level"] != "WARN" {
		t.Errorf("cooldown log level = %v, want WARN", rec["level"])
	}
	if got, want := rec["site"], "EnrichPullRequest"; got != want {
		t.Errorf("site = %v, want %v", got, want)
	}

	p.prMu.RLock()
	until := p.rateLimitedUntil
	p.prMu.RUnlock()
	if until.IsZero() {
		t.Error("cooldown was logged but the timer was not armed")
	}
}

// TestServeStaleEnrichmentLogsAtWarn asserts the other half of the backoff
// story: answering from stale cache instead of GitHub is visible without
// turning on Debug. The cache is seeded directly so the test turns on the
// stale-serve decision alone, not on the enrichment wire shape.
func TestServeStaleEnrichmentLogsAtWarn(t *testing.T) {
	t.Parallel()

	tr := &fakeTransport{respond: func(_ int, _ *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}}
	cap := newLogCapture(slog.LevelInfo)
	now := time.Now()
	p := newLoggedProvider(t, tr, cap, func() time.Time { return now })

	key := PullRequestKey{Owner: "alice", Name: "repo", Number: 7}
	// Enriched long enough ago to be stale, recently enough to still be served.
	p.prMu.Lock()
	p.prs[key] = prEntry{value: PullRequest{RepoOwner: "alice", RepoName: "repo", Number: 7}, at: now}
	p.enrichAt[key] = now.Add(-2 * enrichmentTTL)
	p.prMu.Unlock()

	got, err := p.EnrichPullRequest(context.Background(), key)
	if err != nil {
		t.Fatalf("EnrichPullRequest should have served stale, got error: %v", err)
	}
	if got.Number != 7 {
		t.Fatalf("EnrichPullRequest returned %+v, want the cached PR #7", got)
	}

	rec := requireOne(t, cap, staleLogMessage)
	if rec["level"] != "WARN" {
		t.Errorf("stale-serve log level = %v, want WARN", rec["level"])
	}
	if got, want := rec["key"], key.String(); got != want {
		t.Errorf("key = %v, want %v", got, want)
	}
}

// TestRepoFromRESTPath pins the endpoint→repo projection the call log relies
// on, including the paths that carry no repo at all.
func TestRepoFromRESTPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path string
		want string
	}{
		{path: "/repos/alice/repo", want: "alice/repo"},
		{path: "/repos/alice/repo/pulls", want: "alice/repo"},
		{path: "/repos/alice/repo/pulls/7/reviews", want: "alice/repo"},
		{path: "/user", want: ""},
		{path: "/repos/alice", want: ""},
		{path: "", want: ""},
	}
	for _, tc := range cases {
		if got := repoFromRESTPath(tc.path); got != tc.want {
			t.Errorf("repoFromRESTPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestRepoFromGraphQLVariables pins the variables→repo projection, including
// the batch-enrich case where repos are inlined in the query and the variables
// map is nil.
func TestRepoFromGraphQLVariables(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		vars map[string]any
		want string
	}{
		{name: "owner and name", vars: map[string]any{"owner": "alice", "name": "repo"}, want: "alice/repo"},
		{name: "nil variables", vars: nil, want: ""},
		{name: "owner only", vars: map[string]any{"owner": "alice"}, want: ""},
		{name: "non-string values", vars: map[string]any{"owner": 1, "name": 2}, want: ""},
	}
	for _, tc := range cases {
		if got := repoFromGraphQLVariables(tc.vars); got != tc.want {
			t.Errorf("%s: repoFromGraphQLVariables(%v) = %q, want %q", tc.name, tc.vars, got, tc.want)
		}
	}
}
