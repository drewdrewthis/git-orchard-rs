package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// realDashboardHandler builds the daemon's HTTP handler exactly as server.New
// wires it — the SAME path this PR adds mux.Handle("/", dashboardHandler()) to.
// The tests below therefore exercise the real route table, so a future change
// to the registration in server.go (renamed/added/dropped route) that shadowed
// an endpoint would fail here rather than silently pass against a hand-copied
// mirror. That matters: there is no Go CI job (#712), so this is the only
// automated guard against that drift. Mirrors conversations_jsonl_mount_test.go.
func realDashboardHandler(t *testing.T) http.Handler {
	t.Helper()
	lookup := &stubPathLookup{m: map[string]string{}}
	srv := New("", slog.Default(), WithConversationsJSONL(lookup, t.TempDir()))
	return srv.HTTPHandler()
}

func TestDashboard_ServesRootWithMarker(t *testing.T) {
	rr := httptest.NewRecorder()
	realDashboardHandler(t).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("GET / Content-Type = %q, want to contain text/html", ct)
	}
	if !strings.Contains(rr.Body.String(), `id="orchard-dashboard"`) {
		t.Errorf(`GET / body is missing the marker id="orchard-dashboard"`)
	}
}

func TestDashboard_NotFoundForNonRoot(t *testing.T) {
	// The "/" catch-all must 404 unmatched non-root paths rather than serve the
	// page (guards against an open file server).
	rr := httptest.NewRecorder()
	realDashboardHandler(t).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /nope status = %d, want 404", rr.Code)
	}
	if strings.Contains(rr.Body.String(), `id="orchard-dashboard"`) {
		t.Errorf("GET /nope served the dashboard page; it must 404 (no open file server)")
	}
}

func TestDashboard_RejectsNonReadMethods(t *testing.T) {
	// The dashboard is read-only: only GET/HEAD serve the page. A write method
	// on "/" must be rejected (405), never fall through to a 200 page — this is
	// the guard behind the PR's "no write actions from the page" claim.
	h := realDashboardHandler(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(m, "/", nil))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s / status = %d, want 405", m, rr.Code)
		}
		if strings.Contains(rr.Body.String(), `id="orchard-dashboard"`) {
			t.Errorf("%s / served the dashboard page; a write method must not", m)
		}
	}
}

func TestDashboard_DoesNotShadowExistingRoutes(t *testing.T) {
	h := realDashboardHandler(t)

	// /health must still be served by its own handler, not shadowed by "/".
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("GET /health status = %d, want 200 (shadowed by dashboard \"/\"?)", rr.Code)
	}
	if strings.Contains(rr.Body.String(), `id="orchard-dashboard"`) {
		t.Errorf("GET /health served the dashboard page — route shadowed")
	}

	// /graphql (a POST introspection) must still resolve to the GraphQL handler.
	body, _ := json.Marshal(map[string]string{"query": "{ __typename }"})
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("POST /graphql status = %d, want 200 (shadowed?)", rr.Code)
	}
	if strings.Contains(rr.Body.String(), `id="orchard-dashboard"`) {
		t.Errorf("POST /graphql served the dashboard page — route shadowed")
	}

	// /v1/conversations/<uuid>/jsonl must route to the conversations handler
	// (unknown uuid -> 404 there), NOT to the dashboard page.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/conversations/unknown-uuid/jsonl", nil))
	if strings.Contains(rr.Body.String(), `id="orchard-dashboard"`) {
		t.Errorf("GET /v1/conversations/... served the dashboard page — route shadowed")
	}
}
