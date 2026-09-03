// E2E coverage for Subscription.nodeChanged dispatch.
//
// Boots a tiny daemon via httptest. Drives provider invalidations
// through stateful CommandRunners (tmux, ps) and on-disk JSONL writes
// (claudeprojects). Asserts the websocket subscriber receives the
// freshly-loaded node payload for each id-prefix the dispatcher claims
// to support.
//
// Mirrors the shape of internal/server/providers/peerproxy/peerproxy_e2e_test.go.
package resolvers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/drewdrewthis/orchardist/internal/server"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudeprojects"
	psprovider "github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	tmuxprovider "github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// stubRunner is a stateful CommandRunner. Each call returns the next
// queued response (or the last one indefinitely) — so two Refresh calls
// return two different snapshots and the provider fan-out fires.
type stubRunner struct {
	mu       sync.Mutex
	state    int
	rules    []func(name string, args ...string) ([]byte, error)
	advance  chan struct{}
	advanced bool
}

func newStubRunner(rules ...func(name string, args ...string) ([]byte, error)) *stubRunner {
	return &stubRunner{rules: rules}
}

func (s *stubRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.mu.Lock()
	idx := s.state
	if idx >= len(s.rules) {
		idx = len(s.rules) - 1
	}
	rule := s.rules[idx]
	s.mu.Unlock()
	return rule(name, args...)
}

func (s *stubRunner) advanceState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state < len(s.rules)-1 {
		s.state++
	}
}

const e2eHostID = "test-host"

// pollOK returns true if cond becomes true before deadline. The poll
// loop is intentionally tight — provider fan-outs land on the order of
// milliseconds, but websocket frame delivery occasionally takes longer.
func pollOK(deadline time.Duration, cond func() bool) bool {
	expire := time.Now().Add(deadline)
	for {
		if cond() {
			return true
		}
		if time.Now().After(expire) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// firstNonFlagArg picks the first positional (non-flag, non-flag-value)
// argument out of a tmux command line. tmux always passes `-S <socket>`
// before the subcommand, so we strip those before inspecting.
func firstNonFlagArg(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-S", "-L":
			i++ // skip the value
			continue
		}
		return args[i]
	}
	return ""
}

// stubTmuxRunner returns a CommandRunner whose snapshots flip between
// two states — empty and one-session — so the second Refresh broadcasts
// a TmuxSession invalidation. The TmuxServer subscription rides the
// same fan-out (sessions firehose) because the briefing's mapping
// proves the wiring exists.
//
// Per #464 the adapter coalesces session/window/pane discovery into a
// single `list-panes -a -F <combined-format>` call; the stub returns
// the combined-format line on list-panes (and only that) for the
// one-session state.
func stubTmuxRunner() *stubRunner {
	const fieldSep = tmuxprovider.FieldSep
	emptyState := func(name string, args ...string) ([]byte, error) {
		// `tmux info` -> "alive" probe. Always succeed.
		if firstNonFlagArg(args) == "info" {
			return []byte("ok\n"), nil
		}
		// list-* all return empty in this state.
		return []byte(""), nil
	}
	oneSession := func(name string, args ...string) ([]byte, error) {
		switch firstNonFlagArg(args) {
		case "info":
			return []byte("ok\n"), nil
		case "list-panes":
			// Combined format from adapter.go listAllFormat:
			// session_name, session_created, session_attached, session_activity,
			// session_windows, session_window_index, window_index, window_name,
			// window_active, window_panes, window_active_pane, pane_id,
			// pane_title, pane_current_command, pane_pid, pane_width,
			// pane_height, pane_dead.
			fields := []string{
				"alpha", "1700000000", "0", "1700000010", "1", "0",
				"0", "main", "1", "1", "%0",
				"%0", "main", "bash", "1234", "80", "24", "0",
			}
			return []byte(strings.Join(fields, fieldSep) + "\n"), nil
		case "list-sessions", "list-windows", "list-clients":
			// Legacy paths — kept for any caller that still routes through
			// these (e.g. parser unit tests). Hot path uses list-panes.
			return []byte(""), nil
		}
		return []byte(""), nil
	}
	return newStubRunner(emptyState, oneSession)
}

// stubPsRunner alternates between an empty process table and one
// containing pid 4242. Same idea as stubTmuxRunner.
func stubPsRunner() *stubRunner {
	headerOnly := func(name string, args ...string) ([]byte, error) {
		// macOS ps emits "PID PPID USER ..." header line.
		return []byte("  PID  PPID USER             TT  %CPU    RSS                 STARTED COMMAND\n"), nil
	}
	withProcess := func(name string, args ...string) ([]byte, error) {
		// Mirror the column layout the parser expects: PID PPID USER TT %CPU RSS STARTED COMMAND.
		// `STARTED` is a multi-token date — parsePs handles `Mon Mar  3 12:34:56 2026` shape.
		header := "  PID  PPID USER             TT  %CPU    RSS                 STARTED COMMAND\n"
		row := " 4242     1 testuser         ??   0.0    100 Mon Jan  1 00:00:00 2024 testproc\n"
		return []byte(header + row), nil
	}
	return newStubRunner(headerOnly, withProcess)
}

// writeJSONL writes a synthetic Claude Code transcript to disk so the
// claudeprojects provider can pick it up on the next Refresh.
func writeJSONL(t *testing.T, dir, sessionUUID string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, sessionUUID+".jsonl")
	contents := ""
	for _, l := range lines {
		contents += l + "\n"
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return path
}

// startNodeChangedDaemon wires a daemon with tmux, ps, and
// claudeprojects providers backed by stub adapters. Returned values
// let tests advance state and recompute snapshots.
func startNodeChangedDaemon(t *testing.T) (*httptest.Server, *tmuxprovider.Provider, *psprovider.Provider, *claudeprojects.Provider, string, *stubRunner, *stubRunner) {
	t.Helper()

	tmuxStub := stubTmuxRunner()
	tmuxAdapter := tmuxprovider.NewAdapter(e2eHostID).WithRunner(tmuxStub).WithSocket("/tmp/orchard-test-fake.sock")
	tmuxProv := tmuxprovider.New(tmuxAdapter, nil)

	psStub := stubPsRunner()
	psAdapter := psprovider.NewAdapter(e2eHostID).WithRunner(psStub)
	psProv := psprovider.New(psAdapter, nil)

	projectsRoot := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(filepath.Join(projectsRoot, "alpha"), 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	cpAdapter := claudeprojects.NewFSAdapter(projectsRoot, e2eHostID, nil)
	cpProv := claudeprojects.NewWith(cpAdapter, nil, time.Now, 60*time.Second)

	srv := server.New("",
		nil,
		server.WithTmux(tmuxProv),
		server.WithPS(psProv),
		server.WithClaudeProjects(cpProv),
	)

	ctx := context.Background()
	if err := srv.StartHostProvider(ctx); err != nil {
		t.Fatalf("start host provider: %v", err)
	}
	if err := tmuxProv.Start(ctx); err != nil {
		t.Fatalf("start tmux: %v", err)
	}
	if err := psProv.Start(ctx); err != nil {
		t.Fatalf("start ps: %v", err)
	}
	if err := cpProv.Start(ctx); err != nil {
		t.Fatalf("start claudeprojects: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/graphql", srv.GraphQLHandler())
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	t.Cleanup(func() {
		_ = cpProv.Stop()
	})

	return ts, tmuxProv, psProv, cpProv, projectsRoot, tmuxStub, psStub
}

// dialSubscription opens a graphql-transport-ws websocket and sends the
// connection_init / connection_ack handshake.
func dialSubscription(t *testing.T, addr string) *websocket.Conn {
	t.Helper()
	wsURL := "ws://" + addr + "/graphql"
	dialer := *websocket.DefaultDialer
	dialer.Subprotocols = []string{"graphql-transport-ws"}
	dialer.HandshakeTimeout = 5 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{"type": "connection_init"}); err != nil {
		t.Fatalf("write init: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack["type"] != "connection_ack" {
		t.Fatalf("expected connection_ack, got %v", ack["type"])
	}
	return conn
}

// stripScheme returns the host:port portion of a URL.
func stripScheme(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("strip scheme: %v", err)
	}
	return u.Host
}

// frame is the minimal graphql-transport-ws shape for `next` payloads.
type frame struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// startReader pumps frames off the websocket through a channel so the
// caller can range with timeouts.
func startReader(t *testing.T, conn *websocket.Conn) <-chan frame {
	t.Helper()
	frames := make(chan frame, 16)
	go func() {
		defer close(frames)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			var f frame
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			frames <- f
		}
	}()
	return frames
}

// readPayload pulls frames until one matches subID and is type "next",
// then JSON-decodes its payload into v.
func readPayload(t *testing.T, frames <-chan frame, subID string, deadline time.Duration, v any) error {
	t.Helper()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return errors.New("ws closed before payload arrived")
			}
			if f.ID != subID {
				continue
			}
			if f.Type == "error" {
				return fmt.Errorf("subscription error: %s", string(f.Payload))
			}
			if f.Type == "complete" {
				return errors.New("subscription completed without payload")
			}
			if f.Type != "next" {
				continue
			}
			if err := json.Unmarshal(f.Payload, v); err != nil {
				return fmt.Errorf("decode payload: %w (%s)", err, string(f.Payload))
			}
			return nil
		case <-timer.C:
			return errors.New("timeout waiting for payload")
		}
	}
}

// readError pulls frames until one matches subID and is type "error" or
// "next" containing GraphQL errors, then returns the encoded payload.
func readError(t *testing.T, frames <-chan frame, subID string, deadline time.Duration) (string, error) {
	t.Helper()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return "", errors.New("ws closed before error arrived")
			}
			if f.ID != subID {
				continue
			}
			if f.Type == "error" {
				return string(f.Payload), nil
			}
			if f.Type == "next" {
				// Some servers wrap errors inside `next`. Inspect for an `errors` field.
				var env struct {
					Errors []struct {
						Message string `json:"message"`
					} `json:"errors"`
				}
				if err := json.Unmarshal(f.Payload, &env); err == nil && len(env.Errors) > 0 {
					return env.Errors[0].Message, nil
				}
			}
		case <-timer.C:
			return "", errors.New("timeout waiting for error")
		}
	}
}

// subscribe sends a subscribe message for the given query.
func subscribe(t *testing.T, conn *websocket.Conn, subID, query string) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{
		"id":      subID,
		"type":    "subscribe",
		"payload": map[string]any{"query": query},
	}); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}
}

// TestNodeChanged_DispatchesByPrefix proves the dispatch table:
// TmuxSession, Conversation, and Process subscriptions each receive a
// payload with the correct id when their owning provider invalidates.
func TestNodeChanged_DispatchesByPrefix(t *testing.T) {
	ts, tmuxProv, psProv, cpProv, projectsRoot, tmuxStub, psStub := startNodeChangedDaemon(t)
	addr := stripScheme(t, ts.URL)

	conn := dialSubscription(t, addr)
	defer func() { _ = conn.Close() }()
	frames := startReader(t, conn)

	// --- TmuxSession subscription (rides tmux Sessions firehose). ---
	const tmuxSessionID = "TmuxSession:" + e2eHostID + ":alpha"
	subscribe(t, conn, "tmux-1", fmt.Sprintf(
		`subscription { nodeChanged(id: %q) { __typename ... on TmuxSession { id name } } }`,
		tmuxSessionID,
	))

	// --- Conversation subscription. ---
	const convoUUID = "abcd-1234-test"
	const convoID = "Conversation:" + convoUUID
	subscribe(t, conn, "conv-1", fmt.Sprintf(
		`subscription { nodeChanged(id: %q) { __typename ... on Conversation { id sessionUuid } } }`,
		convoID,
	))

	// --- Process subscription. ---
	const procID = e2eHostID + ":4242"
	subscribe(t, conn, "proc-1", fmt.Sprintf(
		`subscription { nodeChanged(id: %q) { __typename ... on Process { id pid } } }`,
		procID,
	))

	// Give the websocket transport a beat to register all three
	// subscribers before we fire invalidations.
	time.Sleep(150 * time.Millisecond)

	// Trigger fan-outs.
	tmuxStub.advanceState()
	if err := tmuxProv.Refresh(context.Background()); err != nil {
		t.Fatalf("tmux refresh: %v", err)
	}

	psStub.advanceState()
	if err := psProv.Refresh(context.Background()); err != nil {
		t.Fatalf("ps refresh: %v", err)
	}

	// Write a Conversation transcript and trigger refresh.
	writeJSONL(t, filepath.Join(projectsRoot, "alpha"), convoUUID, []string{
		`{"type":"user","timestamp":"2026-01-01T00:00:00Z","cwd":"/tmp"}`,
	})
	if err := cpProv.Refresh(context.Background()); err != nil {
		t.Fatalf("claudeprojects refresh: %v", err)
	}

	type tmuxPayload struct {
		Data struct {
			NodeChanged struct {
				Typename string `json:"__typename"`
				ID       string `json:"id"`
				Name     string `json:"name"`
			} `json:"nodeChanged"`
		} `json:"data"`
	}
	type convoPayload struct {
		Data struct {
			NodeChanged struct {
				Typename    string `json:"__typename"`
				ID          string `json:"id"`
				SessionUUID string `json:"sessionUuid"`
			} `json:"nodeChanged"`
		} `json:"data"`
	}
	type procPayload struct {
		Data struct {
			NodeChanged struct {
				Typename string `json:"__typename"`
				ID       string `json:"id"`
				Pid      int64  `json:"pid"`
			} `json:"nodeChanged"`
		} `json:"data"`
	}

	got := map[string]bool{}
	for !(got["tmux-1"] && got["conv-1"] && got["proc-1"]) {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("ws closed; got=%v", got)
			}
			if f.Type == "error" {
				t.Fatalf("subscription error %s: %s", f.ID, string(f.Payload))
			}
			if f.Type != "next" {
				continue
			}
			switch f.ID {
			case "tmux-1":
				var p tmuxPayload
				if err := json.Unmarshal(f.Payload, &p); err != nil {
					t.Fatalf("decode tmux: %v", err)
				}
				if p.Data.NodeChanged.ID != tmuxSessionID {
					t.Fatalf("tmux id=%q want %q", p.Data.NodeChanged.ID, tmuxSessionID)
				}
				if p.Data.NodeChanged.Typename != "TmuxSession" {
					t.Fatalf("tmux typename=%q want TmuxSession", p.Data.NodeChanged.Typename)
				}
				got["tmux-1"] = true
			case "conv-1":
				var p convoPayload
				if err := json.Unmarshal(f.Payload, &p); err != nil {
					t.Fatalf("decode conv: %v", err)
				}
				if p.Data.NodeChanged.ID != convoID {
					t.Fatalf("conv id=%q want %q", p.Data.NodeChanged.ID, convoID)
				}
				if p.Data.NodeChanged.Typename != "Conversation" {
					t.Fatalf("conv typename=%q want Conversation", p.Data.NodeChanged.Typename)
				}
				got["conv-1"] = true
			case "proc-1":
				var p procPayload
				if err := json.Unmarshal(f.Payload, &p); err != nil {
					t.Fatalf("decode proc: %v", err)
				}
				if p.Data.NodeChanged.ID != procID {
					t.Fatalf("proc id=%q want %q", p.Data.NodeChanged.ID, procID)
				}
				if p.Data.NodeChanged.Typename != "Process" {
					t.Fatalf("proc typename=%q want Process", p.Data.NodeChanged.Typename)
				}
				if p.Data.NodeChanged.Pid != 4242 {
					t.Fatalf("proc pid=%d want 4242", p.Data.NodeChanged.Pid)
				}
				got["proc-1"] = true
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for subscriptions; got=%v", got)
		}
	}
}

// TestNodeChanged_UnknownPrefix proves AC-2: an unknown id prefix
// surfaces a single GraphQL error and the subscription closes cleanly
// (no hang).
func TestNodeChanged_UnknownPrefix(t *testing.T) {
	ts, _, _, _, _, _, _ := startNodeChangedDaemon(t)
	addr := stripScheme(t, ts.URL)

	conn := dialSubscription(t, addr)
	defer func() { _ = conn.Close() }()
	frames := startReader(t, conn)

	subscribe(t, conn, "bogus-1",
		`subscription { nodeChanged(id: "Bogus:foo:bar") { __typename } }`,
	)

	msg, err := readError(t, frames, "bogus-1", 3*time.Second)
	if err != nil {
		t.Fatalf("expected error frame: %v", err)
	}
	if want := `unknown id prefix`; !contains(msg, want) {
		t.Fatalf("error %q does not mention %q", msg, want)
	}
}

// TestNodeChanged_DoesNotHang is a belt-and-braces check that the
// dispatcher closes the channel promptly for unknown prefixes — a
// regression of the original `git provider not configured` bug would
// look like a hang here, not an explicit error.
func TestNodeChanged_DoesNotHang(t *testing.T) {
	ts, _, _, _, _, _, _ := startNodeChangedDaemon(t)
	addr := stripScheme(t, ts.URL)

	conn := dialSubscription(t, addr)
	defer func() { _ = conn.Close() }()
	frames := startReader(t, conn)

	subscribe(t, conn, "bogus-2",
		`subscription { nodeChanged(id: "Wat:lol") { __typename } }`,
	)

	if !pollOK(3*time.Second, func() bool {
		select {
		case f, ok := <-frames:
			if !ok {
				return false
			}
			if f.ID != "bogus-2" {
				return false
			}
			return f.Type == "error" || f.Type == "complete" || f.Type == "next"
		default:
			return false
		}
	}) {
		t.Fatalf("dispatcher hung on unknown prefix")
	}
}

// contains is a tiny strings.Contains alias kept here so the assertion
// site reads like English.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
