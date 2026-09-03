package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Shared test fixtures. Three of them were duplicated per test file before,
// which is how the two fake servers came to disagree about their own shape.

// viewOf composes the pane the way Update does and returns what View paints.
// View itself is a pure accessor (see compose.go), so a test that pokes model
// fields directly rather than driving Update composes explicitly.
func viewOf(m *model) string {
	m.compose()
	return m.View()
}

// setTmuxEnv installs a wrapper environment for one test and restores the
// previous one after. env is process-wide because the environment is.
func setTmuxEnv(t *testing.T, e tmuxEnv) {
	t.Helper()
	prev := env
	env = e
	t.Cleanup(func() { env = prev })
}

// stateHome points the sidebar's own files (layout, last launch, log) at a
// scratch directory, so a test never reads or writes the user's real state.
func stateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	return filepath.Join(dir, "orchard")
}

// writeStateFile drops raw bytes where the sidebar will look for one of its
// files — for the corrupt-file cases, which no writer would ever produce.
func writeStateFile(t *testing.T, name, body string) {
	t.Helper()
	p := stateFile(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeGraphQL points the daemon lanes at a server that answers every POST with
// one canned status and body.
func fakeGraphQL(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	prev := graphqlURL
	graphqlURL = srv.URL
	t.Cleanup(func() { graphqlURL = prev })
}

// wsScript is the server side of one fake graphql-transport-ws connection,
// run after the upgrade. Reading client frames is the script's job; returning
// closes the socket.
type wsScript func(t *testing.T, conn *websocket.Conn)

// fakeGqlws points wsURL at an httptest server and shrinks readWait for the
// test's duration — it owns both globals, and restores them only after the
// caller's runStream cleanup has joined the client goroutine (LIFO order).
func fakeGqlws(t *testing.T, wait time.Duration, script wsScript) {
	t.Helper()
	up := websocket.Upgrader{Subprotocols: []string{"graphql-transport-ws"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		script(t, conn)
	}))
	t.Cleanup(srv.Close)
	oldURL, oldWait := wsURL, readWait
	wsURL, readWait = "ws"+strings.TrimPrefix(srv.URL, "http"), wait
	t.Cleanup(func() { wsURL, readWait = oldURL, oldWait })
}
