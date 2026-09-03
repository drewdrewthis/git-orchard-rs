package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// @scenario orchard shell doctor — daemon reachability check
//
// AC8: with the daemon stopped, the daemon check reports fail with a remedy
// containing "systemctl --user start orchard".
func TestCheckDaemon_Pass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s; want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"version":"1.4.0"}}`))
	}))
	defer srv.Close()

	got := checkDaemon(context.Background(), srv.URL)
	if got.Status != statusPass {
		t.Errorf("Status = %v; want pass (detail: %s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "1.4.0") {
		t.Errorf("Detail = %q; want it to mention the version", got.Detail)
	}
}

func TestCheckDaemon_NonOKStatusFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	got := checkDaemon(context.Background(), srv.URL)
	assertDaemonFailWithRemedy(t, got)
}

func TestCheckDaemon_MalformedBodyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	got := checkDaemon(context.Background(), srv.URL)
	assertDaemonFailWithRemedy(t, got)
}

func TestCheckDaemon_EmptyVersionFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"version":""}}`))
	}))
	defer srv.Close()

	got := checkDaemon(context.Background(), srv.URL)
	assertDaemonFailWithRemedy(t, got)
}

func TestCheckDaemon_UnreachableFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // closed before use: nothing is listening on url any more

	got := checkDaemon(context.Background(), url)
	assertDaemonFailWithRemedy(t, got)
}

func TestCheckDaemon_BadURLFails(t *testing.T) {
	got := checkDaemon(context.Background(), "http://\x7f")
	assertDaemonFailWithRemedy(t, got)
}

func assertDaemonFailWithRemedy(t *testing.T, got checkResult) {
	t.Helper()
	if got.Status != statusFail {
		t.Errorf("Status = %v; want fail", got.Status)
	}
	if got.ID != "daemon" {
		t.Errorf("ID = %q; want daemon", got.ID)
	}
	if !strings.Contains(got.Remedy, "systemctl --user start orchard") {
		t.Errorf("Remedy = %q; want it to contain the exact remedy command", got.Remedy)
	}
}
