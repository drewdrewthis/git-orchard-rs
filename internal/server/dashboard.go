package server

import (
	_ "embed"
	"net/http"
)

// dashboardHTML is the self-contained read-only dashboard page. It is a
// static HTML+CSS+vanilla-JS document that polls the daemon's own
// same-origin POST /graphql for live data — no build step, no external
// resources, no write actions. See dashboard.html.
//
//go:embed dashboard.html
var dashboardHTML []byte

// dashboardHandler serves the embedded dashboard at the EXACT path "/" and
// returns 404 for every other unmatched path. It is mounted on the daemon
// mux as the "/" catch-all (ServeMux longest-prefix keeps /graphql,
// /health, and /v1/conversations/ winning); the exact-path guard below is
// what stops that catch-all from turning the daemon into an open file
// server that leaks arbitrary paths.
func dashboardHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// The page is fully self-contained (inline <style>/<script>, no external
		// resources) and only talks back to its own origin's /graphql. Lock it
		// down: deny everything by default, allow only inline script/style and
		// same-origin fetch. Belt-and-suspenders around the esc()/safeHref()
		// output encoding in dashboard.html.
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
				"connect-src 'self'; base-uri 'none'; form-action 'none'")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(dashboardHTML)
	})
}
