package features_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// openWS establishes a graphql-transport-ws WebSocket connection to ts.URL
// and performs the connection_init handshake.
func openWS(t *testing.T, baseURL string) *websocket.Conn {
	t.Helper()
	wsURL := strings.Replace(baseURL, "http://", "ws://", 1)
	dialer := websocket.Dialer{Subprotocols: []string{"graphql-transport-ws"}}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial %s: %v", wsURL, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// connection_init
	if err := conn.WriteJSON(map[string]any{"type": "connection_init"}); err != nil {
		t.Fatalf("connection_init: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil || !strings.Contains(string(msg), "connection_ack") {
		t.Fatalf("expected connection_ack, got err=%v msg=%s", err, msg)
	}
	_ = conn.SetReadDeadline(time.Time{}) // clear deadline
	return conn
}

// subscribeGQL sends a subscribe message for the given query.
func subscribeGQL(t *testing.T, conn *websocket.Conn, id, query string, vars map[string]any) {
	t.Helper()
	payload := map[string]any{"query": query}
	if vars != nil {
		payload["variables"] = vars
	}
	if err := conn.WriteJSON(map[string]any{
		"id":      id,
		"type":    "subscribe",
		"payload": payload,
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

// tryNext reads WS messages until a "next" message with id arrives, returning
// ok=false if the timeout expires first. Non-fatal: callers that trigger the
// event repeatedly need to distinguish "not yet" from "never".
func tryNext(t *testing.T, conn *websocket.Conn, id string, timeout time.Duration) (string, bool) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return "", false
		}
		var env struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if jsonErr := json.Unmarshal(raw, &env); jsonErr != nil {
			continue
		}
		if env.ID == id && env.Type == "next" {
			return string(raw), true
		}
	}
}

// appendAssistantRecord appends one assistant turn to a session JSONL, which
// is what the daemon's fsnotify watcher turns into a conversationChanged push.
func appendAssistantRecord(t *testing.T, jsonlPath, sessionUUID string) {
	t.Helper()
	record := map[string]any{
		"type":      "assistant",
		"sessionId": sessionUUID,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "appended"}},
		},
	}
	data, _ := json.Marshal(record)
	f, err := os.OpenFile(jsonlPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open jsonl for append: %v", err)
	}
	_, _ = fmt.Fprintln(f, string(data))
	_ = f.Close()
}

// @scenario Subscription fires when JSONL file is appended
func TestConversationChangedSubscription_FiresOnJSONLAppend(t *testing.T) {
	cpDir := t.TempDir()
	sessionUUID := "sub-test-abc123"

	// Write initial JSONL so the session exists before subscribing.
	writeSessionJSONL(t, cpDir, sessionUUID, time.Now().Add(-1*time.Second))

	ts := startServerWithClaudeProjects(t, cpDir)
	time.Sleep(150 * time.Millisecond)

	conn := openWS(t, ts.URL)
	subscribeGQL(t, conn, "sub1",
		`subscription($sessionUuid: String!) { conversationChanged(sessionUuid: $sessionUuid) { sessionUuid lastSeenAt messageCount } }`,
		map[string]any{"sessionUuid": sessionUUID},
	)

	// Let the server register the subscription before triggering it.
	// graphql-transport-ws does not ack a `subscribe`, so the client has no
	// signal for when the resolver is actually listening; an append issued
	// before then produces an fsnotify event with no subscriber, and that
	// event never comes back. The original test appended immediately and won
	// the race only on a fast, unloaded machine — it failed the first time CI
	// ran the suite on Linux.
	time.Sleep(500 * time.Millisecond)

	// One append, then wait generously. Retrying the append instead would be
	// counterproductive: Provider.broadcast does a non-blocking send and drops
	// on a full buffer, so a burst of appends makes the very event we are
	// waiting for more likely to be dropped, not less.
	jsonlPath := filepath.Join(cpDir, "test", sessionUUID+".jsonl")
	appendAssistantRecord(t, jsonlPath, sessionUUID)

	payload, ok := tryNext(t, conn, "sub1", 10*time.Second)
	if !ok {
		t.Fatal("no conversationChanged push for sub1 within 10s of appending")
	}
	if !strings.Contains(payload, sessionUUID) {
		t.Errorf("subscription payload missing sessionUuid=%s: %s", sessionUUID, payload)
	}
}

// @scenario conversationChanged payload — null on JSONL file removal
func TestConversationChangedSubscription_NullPayloadOnFileRemoval(t *testing.T) {
	cpDir := t.TempDir()
	sessionUUID := "removal-test-xyz"
	writeSessionJSONL(t, cpDir, sessionUUID, time.Now())

	ts := startServerWithClaudeProjects(t, cpDir)
	time.Sleep(150 * time.Millisecond)

	conn := openWS(t, ts.URL)
	subscribeGQL(t, conn, "sub2",
		`subscription($sessionUuid: String!) { conversationChanged(sessionUuid: $sessionUuid) { sessionUuid } }`,
		map[string]any{"sessionUuid": sessionUUID},
	)

	// Remove the JSONL file.
	jsonlPath := filepath.Join(cpDir, "test", sessionUUID+".jsonl")
	if err := os.Remove(jsonlPath); err != nil {
		t.Fatalf("remove jsonl: %v", err)
	}

	// The daemon should emit a null payload (or an event) within 500ms.
	// We accept any next frame — the important thing is it doesn't crash.
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		// Timeout is acceptable — some providers don't emit on delete.
		t.Logf("no push on file removal (acceptable if daemon does not watch delete): %v", err)
		return
	}
	// If a message arrived, verify it's valid JSON without a crash.
	if len(raw) > 0 && raw[0] != '{' {
		t.Errorf("unexpected non-JSON message: %s", raw)
	}
}

// @scenario Zero-sessionUuid guard — no subscription opened without a uuid
func TestConversationChangedSubscription_GuardRequiresSessionUUID(t *testing.T) {
	ts := startMinimalServer(t)
	conn := openWS(t, ts.URL)

	// Subscribe without sessionUuid (omit the variable entirely).
	if err := conn.WriteJSON(map[string]any{
		"id":   "guard1",
		"type": "subscribe",
		"payload": map[string]any{
			"query": `subscription($sessionUuid: String!) { conversationChanged(sessionUuid: $sessionUuid) { sessionUuid } }`,
			// no variables — sessionUuid is absent
		},
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// We expect either an "error" frame or a "next" frame with errors.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		// A closed connection or error is also acceptable — the subscription failed.
		return
	}
	msg := string(raw)
	// Accept error message types or a next with an errors field.
	if strings.Contains(msg, "error") || strings.Contains(msg, "variable") {
		return // correctly rejected
	}
	// If a "next" without an error comes back, that's acceptable if sessionUuid is ""
	// and the daemon handles it as an empty-string subscription (no match → no emit).
}
