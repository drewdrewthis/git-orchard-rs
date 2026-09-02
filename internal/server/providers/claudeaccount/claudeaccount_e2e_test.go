// Package claudeaccount — end-to-end test for the briefing's e2e AC.
//
// What the briefing demands:
//
//  1. Real shellouts via os/exec — no mock interface for this test.
//  2. The shellouts must hit fake `claude` and `ccusage` scripts that
//     emit canned output. We achieve this by stubbing PATH to a temp
//     dir holding the scripts.
//  3. Boot httptest.Server with the gqlgen handler, POST a GraphQL
//     query, assert email + quota fields are populated.
//  4. Add a "tool not installed" case (PATH containing nothing) and
//     assert the typed error surfaces as a GraphQL error.
//
// PII: the fake scripts emit `alice@example.com` only — no real
// Anthropic accounts, no /Users/<name> paths.
package claudeaccount_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"

	gql "github.com/drewdrewthis/orchardist/internal/server/graphql"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudeaccount"
	"github.com/drewdrewthis/orchardist/internal/server/resolvers"
)

// gqlEnvelope is the shape we decode GraphQL responses into. We only
// care about the fields the resolver returns + any per-field errors.
type gqlEnvelope struct {
	Data struct {
		ClaudeAccounts []struct {
			ID             string   `json:"id"`
			Email          string   `json:"email"`
			QuotaUsed      *float64 `json:"quotaUsed"`
			QuotaCap       *float64 `json:"quotaCap"`
			QuotaEstimated bool     `json:"quotaEstimated"`
			QuotaResetsAt  *string  `json:"quotaResetsAt"`
			Host           struct {
				ID string `json:"id"`
			} `json:"host"`
			Instances []struct {
				ID string `json:"id"`
			} `json:"instances"`
		} `json:"claudeAccounts"`
	} `json:"data"`
	Errors []struct {
		Message string   `json:"message"`
		Path    []string `json:"path"`
	} `json:"errors"`
}

// TestE2E_HappyPath_FakeShellouts proves the full pipeline works:
// real os/exec → fake `claude`/`ccusage` scripts → adapter → provider
// → resolver → gqlgen handler → HTTP. The query asserts every field
// the briefing names.
func TestE2E_HappyPath_FakeShellouts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("e2e relies on POSIX shell scripts in PATH")
	}

	dir := t.TempDir()
	// Use /bin/echo (resolved at script-write time) so the fakes are
	// self-contained and do not depend on coreutils being on the
	// stubbed PATH.
	writeFakeScript(t, dir, "claude", `#!/bin/sh
printf '%s\n' '{"email": "alice@example.com"}'
`)
	writeFakeScript(t, dir, "ccusage", `#!/bin/sh
printf '%s\n' '{"blocks": [{"active": true, "used": 12.5, "cap": 50, "resetsAt": "2026-05-05T00:00:00Z"}]}'
`)

	t.Setenv("PATH", dir)

	srv := newTestDaemon(t)
	defer srv.Close()

	resp := postQuery(t, srv.URL+"/graphql", canonicalQuery)
	if len(resp.Errors) != 0 {
		t.Fatalf("graphql errors: %+v", resp.Errors)
	}
	if got := len(resp.Data.ClaudeAccounts); got != 1 {
		t.Fatalf("got %d claudeAccounts, want 1", got)
	}
	a := resp.Data.ClaudeAccounts[0]
	if a.Email != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com", a.Email)
	}
	if a.QuotaUsed == nil || *a.QuotaUsed != 12.5 {
		t.Errorf("quotaUsed = %v, want 12.5", a.QuotaUsed)
	}
	if a.QuotaCap == nil || *a.QuotaCap != 50 {
		t.Errorf("quotaCap = %v, want 50", a.QuotaCap)
	}
	if !a.QuotaEstimated {
		t.Error("quotaEstimated = false, want true")
	}
	if a.QuotaResetsAt == nil || *a.QuotaResetsAt == "" {
		t.Errorf("quotaResetsAt = %v, want non-empty timestamp", a.QuotaResetsAt)
	}
	if a.Host.ID == "" {
		t.Error("host.id is empty, want stable Host: prefix")
	}
	if a.Instances == nil {
		t.Error("instances is nil, want non-null empty array")
	}
	if len(a.Instances) != 0 {
		t.Errorf("instances has %d entries, want 0 (v1 placeholder)", len(a.Instances))
	}
	if a.ID == "" {
		t.Error("id is empty")
	}
}

// TestE2E_ToolNotInstalled_SurfacesGraphQLError is the briefing's
// "tool not installed" case: neither `claude` nor `ccusage` resolves.
// The resolver must surface a typed GraphQL error on the
// claudeAccounts field rather than collapsing the daemon.
//
// ORCHARD_BIN_DIRS is pinned empty alongside PATH: since #400 the
// adapter also searches the well-known user bin dirs, where a dev
// machine really does have `claude` installed.
func TestE2E_ToolNotInstalled_SurfacesGraphQLError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("e2e relies on POSIX shell scripts in PATH")
	}

	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)
	t.Setenv("ORCHARD_BIN_DIRS", "")

	srv := newTestDaemon(t)
	defer srv.Close()

	resp := postQuery(t, srv.URL+"/graphql", canonicalQuery)
	if len(resp.Errors) == 0 {
		t.Fatalf("expected per-field GraphQL error; got data=%+v", resp.Data)
	}
	if !containsString(resp.Errors[0].Message, "claudeaccount") || !containsString(resp.Errors[0].Message, "not installed") {
		t.Errorf("error message = %q, want it to mention 'claudeaccount' + 'not installed'",
			resp.Errors[0].Message)
	}
	// The error should be scoped to the claudeAccounts field — that's
	// what makes it a per-field error rather than a transport failure.
	if len(resp.Errors[0].Path) > 0 && resp.Errors[0].Path[0] != "claudeAccounts" {
		t.Errorf("error path = %v, want first segment 'claudeAccounts'", resp.Errors[0].Path)
	}
}

// canonicalQuery is the same shape `orchard query claude-account`
// dispatches; keeps the e2e test in lockstep with the CLI.
const canonicalQuery = `query {
  claudeAccounts {
    id
    email
    quotaUsed
    quotaCap
    quotaEstimated
    quotaResetsAt
    host { id }
    instances { id }
  }
}`

// newTestDaemon boots a fresh provider + resolver + gqlgen handler on
// httptest. The provider takes a synchronous initial fetch in Start
// (because cache TTL is far in the future), so callers can POST
// immediately.
func newTestDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a := claudeaccount.NewShellAdapter("test-host", nil)
	p := claudeaccount.NewWith(a, nil, time.Now, time.Hour, time.Hour)
	t.Cleanup(func() { _ = p.Stop() })

	if err := p.Start(ctx); err != nil {
		t.Fatalf("provider Start: %v", err)
	}

	cfg := gql.Config{Resolvers: resolvers.New(time.Now()).WithClaudeAccount(p)}
	gqlSrv := handler.New(gql.NewExecutableSchema(cfg))
	gqlSrv.AddTransport(transport.POST{})

	mux := http.NewServeMux()
	mux.Handle("/graphql", gqlSrv)
	return httptest.NewServer(mux)
}

// postQuery issues a GraphQL POST and decodes into gqlEnvelope.
// Failures are fatal — a broken transport invalidates the whole test.
func postQuery(t *testing.T, url, query string) gqlEnvelope {
	t.Helper()
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		t.Fatalf("marshal query: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, string(raw))
	}
	var out gqlEnvelope
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out
}

// writeFakeScript drops a chmod +x POSIX shell script into dir under
// the given name. The script emits the canned body via heredoc on
// stdout when invoked — exactly what `claude auth status --json` /
// `ccusage blocks --json` would print.
func writeFakeScript(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// containsString is the test helper sister of strings.Contains; kept
// local so the test file does not need to import strings just for one
// substring check.
func containsString(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
