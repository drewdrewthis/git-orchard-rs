package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// getJSONL issues a GET for uuid against h and returns the recorder.
func getJSONL(t *testing.T, h http.Handler, uuid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/conversations/"+uuid+"/jsonl", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestConversationsJSONL_RootSymlinkedAfterStart_StillServes is the #513
// regression. The daemon boots before ~/.claude/projects exists, so the
// root cannot be symlink-resolved and falls back to Clean(Abs). When the
// user later creates that path as a symlink to storage elsewhere, every
// candidate resolves through the symlink while the root does not, so
// filepath.Rel yields a ".."-prefixed result and the handler 404s every
// conversation for the life of the process.
func TestConversationsJSONL_RootSymlinkedAfterStart_StillServes(t *testing.T) {
	const uuid = "root-symlinked-later"
	base := t.TempDir()
	root := filepath.Join(base, "projects") // deliberately absent
	store := filepath.Join(base, "real-projects")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}

	// Construct while the root is missing — the fresh-install path.
	lookup := &stubPathLookup{m: map[string]string{
		uuid: filepath.Join(root, uuid+".jsonl"),
	}}
	h := NewConversationsJSONLHandler(lookup, root, slog.Default())

	// The user installs Claude Code and points the projects dir elsewhere.
	content := []byte(`{"type":"user","message":"hello"}` + "\n")
	if err := os.WriteFile(filepath.Join(store, uuid+".jsonl"), content, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if err := os.Symlink(store, root); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	rr := getJSONL(t, h, uuid)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the root became a symlink", rr.Code)
	}
	if got := rr.Body.Bytes(); !bytes.Equal(got, content) {
		t.Errorf("body = %q, want %q", got, content)
	}
}

// TestConversationsJSONL_RootCreatedAfterStart_StillServes covers the
// #513 acceptance criterion that fresh-install ergonomics survive: the
// daemon must still start with the root missing, and must serve once the
// directory shows up — whether or not a symlink is involved.
func TestConversationsJSONL_RootCreatedAfterStart_StillServes(t *testing.T) {
	const uuid = "root-created-later"
	root := filepath.Join(t.TempDir(), "projects") // deliberately absent

	lookup := &stubPathLookup{m: map[string]string{
		uuid: filepath.Join(root, uuid+".jsonl"),
	}}
	h := NewConversationsJSONLHandler(lookup, root, slog.Default())

	// Before the root exists the uuid simply does not resolve — 404, and
	// crucially not a 500 or a dead daemon.
	if rr := getJSONL(t, h, uuid); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d before root exists, want 404", rr.Code)
	}

	content := []byte(`{"type":"assistant"}` + "\n")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, uuid+".jsonl"), content, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	rr := getJSONL(t, h, uuid)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the root exists", rr.Code)
	}
	if got := rr.Body.Bytes(); !bytes.Equal(got, content) {
		t.Errorf("body = %q, want %q", got, content)
	}
}

// TestConversationsJSONL_SymlinkedRoot_OutsidePathStillRejected asserts the
// lazy root re-resolution did not weaken containment: with the root itself a
// symlink, a candidate that resolves outside it is still refused.
func TestConversationsJSONL_SymlinkedRoot_OutsidePathStillRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "projects")
	store := filepath.Join(base, "real-projects")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	if err := os.Symlink(store, root); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	// A real file outside the store, reached by a symlink placed inside it.
	outside := filepath.Join(base, "secret.jsonl")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(store, "escape.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	h := NewConversationsJSONLHandler(
		&stubPathLookup{m: map[string]string{"escape-uuid": link}}, root, slog.Default())

	if rr := getJSONL(t, h, "escape-uuid"); rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a path escaping a symlinked root", rr.Code)
	}
}
