// Package query — CLI E2E for `orchard query claude-account`.
//
// Compiles the binary from cmd/orchard-daemon, spins the daemon's GraphQL
// handler in an httptest.Server (so the test does not compete for the
// hard-coded localhost port), points the binary at that URL via the
// --addr flag, and asserts stdout JSON. No mocks beyond the PATH
// stub for the underlying `claude` / `ccusage` shellouts.
package query_test

import (
	"bytes"
	"context"
	"encoding/json"
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

// const cliE2EDeadline removed (defined in query_e2e_test.go)

// repoRoot returns the absolute path to the git repo root (the working
// directory of `make daemon`). The test file is at
// internal/cli/query/, so two parent traversals get there.
// `orchard query claude-account --addr <ephemeral>` against a test
// daemon backed by fake `claude` / `ccusage` scripts, and asserts the
// CLI prints valid JSON containing the expected email + quota.
func TestCLI_QueryClaudeAccount_E2E_HappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("CLI e2e relies on POSIX shell scripts in PATH")
	}

	binary := buildOrchard(t)

	// Real provider + httptest.Server, served by the gqlgen handler.
	pathDir := stubPathWithFakeBinaries(t)
	t.Setenv("PATH", pathDir)

	provider := claudeaccount.New("test-host", nil)
	t.Cleanup(func() { _ = provider.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), cliE2EDeadline)
	defer cancel()
	if err := provider.Start(ctx); err != nil {
		t.Fatalf("provider start: %v", err)
	}

	cfg := gql.Config{Resolvers: resolvers.New(time.Now()).WithClaudeAccount(provider)}
	gqlSrv := handler.New(gql.NewExecutableSchema(cfg))
	gqlSrv.AddTransport(transport.POST{})

	mux := http.NewServeMux()
	mux.Handle("/graphql", gqlSrv)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)


	stdout, stderr, err := runOrchard(t, binary, ts.URL, "query", "claude-account")
	if err != nil {
		t.Fatalf("orchard query claude-account failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// The CLI pretty-prints the GraphQL envelope. Decode and assert
	// the well-known shape rather than string-matching.
	var env struct {
		Data struct {
			ClaudeAccounts []struct {
				Email          string   `json:"email"`
				QuotaUsed      *float64 `json:"quotaUsed"`
				QuotaCap       *float64 `json:"quotaCap"`
				QuotaEstimated bool     `json:"quotaEstimated"`
			} `json:"claudeAccounts"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(stdout, &env); err != nil {
		t.Fatalf("decode stdout %q: %v", stdout, err)
	}
	if len(env.Errors) != 0 {
		t.Fatalf("unexpected GraphQL errors: %s", env.Errors)
	}
	if got := len(env.Data.ClaudeAccounts); got != 1 {
		t.Fatalf("got %d accounts, want 1", got)
	}
	a := env.Data.ClaudeAccounts[0]
	if a.Email != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com", a.Email)
	}
	if a.QuotaUsed == nil || *a.QuotaUsed != 12.5 {
		t.Errorf("quotaUsed = %v, want 12.5", a.QuotaUsed)
	}
	if !a.QuotaEstimated {
		t.Error("quotaEstimated = false, want true")
	}
}

// TestCLI_QueryClaudeAccount_E2E_ToolNotInstalled compiles the
// binary, points it at a daemon that can resolve neither `claude` nor
// `ccusage`, and asserts the CLI surfaces the typed GraphQL error
// rather than silently returning [].
//
// ORCHARD_BIN_DIRS is pinned empty alongside PATH: since #400 the
// adapter also searches the well-known user bin dirs, where a dev
// machine really does have `claude` installed.
func TestCLI_QueryClaudeAccount_E2E_ToolNotInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("CLI e2e relies on POSIX shell scripts in PATH")
	}

	binary := buildOrchard(t)
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)
	t.Setenv("ORCHARD_BIN_DIRS", "")

	provider := claudeaccount.New("test-host", nil)
	t.Cleanup(func() { _ = provider.Stop() })
	ctx, cancel := context.WithTimeout(context.Background(), cliE2EDeadline)
	defer cancel()
	if err := provider.Start(ctx); err != nil {
		t.Fatalf("provider start: %v", err)
	}

	cfg := gql.Config{Resolvers: resolvers.New(time.Now()).WithClaudeAccount(provider)}
	gqlSrv := handler.New(gql.NewExecutableSchema(cfg))
	gqlSrv.AddTransport(transport.POST{})
	mux := http.NewServeMux()
	mux.Handle("/graphql", gqlSrv)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	stdout, stderr, err := runOrchard(t, binary, ts.URL, "query", "claude-account")
	if err != nil {
		t.Fatalf("CLI failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !bytes.Contains(stdout, []byte("not installed")) {
		t.Errorf("stdout %q missing 'not installed' substring", stdout)
	}
}


// stubPathWithFakeBinaries drops POSIX shell scripts named `claude`
// and `ccusage` into a fresh temp dir and returns the dir. The
// scripts emit canned JSON output matching the documented shape.
func stubPathWithFakeBinaries(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("claude", `#!/bin/sh
printf '%s\n' '{"email": "alice@example.com"}'
`)
	write("ccusage", `#!/bin/sh
printf '%s\n' '{"blocks": [{"active": true, "used": 12.5, "cap": 50, "resetsAt": "2026-05-05T00:00:00Z"}]}'
`)
	return dir
}

