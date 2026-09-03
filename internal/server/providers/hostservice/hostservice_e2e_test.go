package hostservice_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"

	gql "github.com/drewdrewthis/orchardist/internal/server/graphql"
	"github.com/drewdrewthis/orchardist/internal/server/providers/hostservice"
	"github.com/drewdrewthis/orchardist/internal/server/resolvers"
)

// TestHostService_E2E_FullStack drives the brief's primary AC: stub PATH
// with fake `launchctl`/`systemctl` scripts emitting canned outputs that
// cover all five states (active, inactive, failed, not_installed,
// unknown) and assert each maps correctly through the full GraphQL
// pipeline.
//
// Real shellouts via the production NewAdapter() — no provider/adapter
// mocks. The test owns the OS service-manager binary path via PATH.
//
// PII guard: every fixture uses generic `com.example.test.*` (macOS) /
// `example-test-*` (Linux) names. No real services.
func TestHostService_E2E_FullStack(t *testing.T) {
	stubDir := setupStubServiceManager(t)
	t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))

	services := []string{
		serviceForState("active"),
		serviceForState("inactive"),
		serviceForState("failed"),
		serviceForState("not_installed"),
		serviceForState("unknown"),
	}
	provider := hostservice.NewWith(hostservice.NewAdapter(), "test-host-id", services, time.Now)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := provider.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	srv := newTestDaemon(t, provider)
	defer srv.Close()

	resp := postQuery(t, srv.URL, `query {
		host {
			id
			machineId
			hostServices {
				id
				name
				state
				since
				exitCode
				logTail
			}
		}
	}`)
	if len(resp.Errors) > 0 {
		t.Fatalf("graphql errors: %+v", resp.Errors)
	}
	if resp.Data.Host == nil {
		t.Fatal("data.host is nil")
	}
	if got, want := resp.Data.Host.MachineID, "test-host-id"; got != want {
		t.Errorf("machineId = %q, want %q", got, want)
	}
	if got, want := len(resp.Data.Host.HostServices), 5; got != want {
		t.Fatalf("hostServices length = %d, want %d", got, want)
	}

	byName := map[string]hostServicePayload{}
	for _, hs := range resp.Data.Host.HostServices {
		byName[hs.Name] = hs
	}
	for _, want := range []struct {
		name  string
		state string
	}{
		{serviceForState("active"), "active"},
		{serviceForState("inactive"), "inactive"},
		{serviceForState("failed"), "failed"},
		{serviceForState("not_installed"), "not_installed"},
		{serviceForState("unknown"), "unknown"},
	} {
		got, ok := byName[want.name]
		if !ok {
			t.Errorf("expected service %q in response, missing", want.name)
			continue
		}
		if got.State != want.state {
			t.Errorf("%s: state = %q, want %q", want.name, got.State, want.state)
		}
		expectedID := "HostService:test-host-id:" + want.name
		if got.ID != expectedID {
			t.Errorf("%s: id = %q, want %q", want.name, got.ID, expectedID)
		}
		if want.state == "active" && got.Since == nil {
			t.Errorf("%s: since is nil; want non-null timestamp for active service", want.name)
		}
		if want.state == "not_installed" || want.state == "unknown" {
			if got.Since != nil || got.ExitCode != nil || got.LogTail != nil {
				t.Errorf("%s service has non-nil optional fields: %+v", want.state, got)
			}
		}
	}
}

// TestHostService_E2E_ServiceManagerMissing asserts that when the OS
// service manager binary is unreachable on PATH, every watched service
// degrades to state=unknown rather than collapsing the resolver.
//
// We point PATH at an empty directory so launchctl / systemctl can't be
// found. The resolver path catches ErrServiceManagerMissing and surfaces
// state=unknown for each service.
func TestHostService_E2E_ServiceManagerMissing(t *testing.T) {
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)

	services := []string{"missing-svc-1", "missing-svc-2"}
	provider := hostservice.NewWith(hostservice.NewAdapter(), "test-host-id", services, time.Now)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := provider.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	srv := newTestDaemon(t, provider)
	defer srv.Close()

	resp := postQuery(t, srv.URL, `query { host { hostServices { name state } } }`)
	if len(resp.Errors) > 0 {
		t.Fatalf("graphql errors: %+v", resp.Errors)
	}
	if resp.Data.Host == nil {
		t.Fatal("host is nil")
	}
	if got := len(resp.Data.Host.HostServices); got != 2 {
		t.Fatalf("expected 2 services, got %d", got)
	}
	for _, hs := range resp.Data.Host.HostServices {
		if hs.State != "unknown" {
			t.Errorf("%s: state = %q, want unknown when service-manager binary is missing", hs.Name, hs.State)
		}
	}
}

// setupStubServiceManager writes a fake `launchctl` (macOS) or
// `systemctl` + `journalctl` (Linux) into a temp dir. Each script
// dispatches on argv[2] (the service name) so the four states each
// emit their own canned response.
//
// Returns the temp dir path so the caller can prepend it to PATH.
func setupStubServiceManager(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	switch runtime.GOOS {
	case "darwin":
		writeStubLaunchctl(t, dir)
	case "linux":
		writeStubSystemctl(t, dir)
		writeStubJournalctl(t, dir)
	default:
		t.Skipf("unsupported OS %q (only darwin/linux ship a watchlist)", runtime.GOOS)
	}
	return dir
}

func writeStubLaunchctl(t *testing.T, dir string) {
	t.Helper()
	script := `#!/bin/sh
set -e
case "$2" in
  com.example.test.active)
    cat <<'EOF'
{
	"Label" = "com.example.test.active";
	"LastExitStatus" = 0;
	"PID" = 12345;
};
EOF
    exit 0
    ;;
  com.example.test.inactive)
    cat <<'EOF'
{
	"Label" = "com.example.test.inactive";
	"LastExitStatus" = 0;
};
EOF
    exit 0
    ;;
  com.example.test.failed)
    cat <<'EOF'
{
	"Label" = "com.example.test.failed";
	"LastExitStatus" = 78;
};
EOF
    exit 0
    ;;
  com.example.test.not_installed)
    echo "Could not find service \"com.example.test.not_installed\" in domain for system" 1>&2
    exit 113
    ;;
  com.example.test.unknown)
    echo "launchctl: an unrecognised internal error occurred (-42)" 1>&2
    exit 1
    ;;
  *)
    echo "stub launchctl: unhandled label $2" 1>&2
    exit 1
    ;;
esac
`
	path := filepath.Join(dir, "launchctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub launchctl: %v", err)
	}
}

func writeStubSystemctl(t *testing.T, dir string) {
	t.Helper()
	script := `#!/bin/sh
set -e
# The adapter calls ` + "`systemctl --user <verb> [flags] <name>`" + `, so the verb is
# $2 and the unit name is the last argument. Take the last argument with a
# POSIX for-loop, NOT bash's ${@: -1}: /bin/sh is dash on Debian/Ubuntu, which
# rejects that slice with "Bad substitution" and — under set -e — exits
# non-zero for every call, so the adapter reported every service as "unknown".
# macOS never caught it because /bin/sh there is bash, and macOS runs the
# launchctl stub anyway.
verb="$2"
for name; do :; done
case "$verb" in
  is-active)
    case "$name" in
      example-test-active) echo "active"; exit 0 ;;
      example-test-inactive) echo "inactive"; exit 3 ;;
      example-test-failed) echo "failed"; exit 3 ;;
      example-test-not_installed)
        echo "Unit example-test-not_installed.service not loaded." 1>&2
        echo ""
        exit 4
        ;;
      example-test-unknown)
        echo "systemctl: an unrecognised internal error occurred (-42)" 1>&2
        echo ""
        exit 1
        ;;
      *)
        echo "stub systemctl: unhandled name $name" 1>&2
        exit 1 ;;
    esac
    ;;
  show)
    case "$name" in
      example-test-active)
        echo "ActiveEnterTimestamp=Mon 2026-05-04 12:34:56 UTC"
        echo "ExecMainStatus=0" ;;
      example-test-inactive)
        echo "ActiveEnterTimestamp="
        echo "ExecMainStatus=0" ;;
      example-test-failed)
        echo "ActiveEnterTimestamp=Mon 2026-05-04 12:00:00 UTC"
        echo "ExecMainStatus=42" ;;
      *)
        echo "ActiveEnterTimestamp="
        echo "ExecMainStatus=" ;;
    esac
    ;;
  *)
    echo "stub systemctl: unhandled verb $verb" 1>&2
    exit 1 ;;
esac
`
	path := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub systemctl: %v", err)
	}
}

func writeStubJournalctl(t *testing.T, dir string) {
	t.Helper()
	script := `#!/bin/sh
# Stub journalctl: emits a canned tail per unit name. The test does not
# assert on tail content; this stub exists so the adapter's logTail
# read does not 127.
name=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-u" ]; then
    name="$arg"
  fi
  prev="$arg"
done
case "$name" in
  example-test-active) echo "Jan 01 00:00:00 host example: started"; exit 0 ;;
  example-test-failed) echo "Jan 01 00:00:00 host example: exited with code 42"; exit 0 ;;
  *) exit 0 ;;
esac
`
	path := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub journalctl: %v", err)
	}
}

// serviceForState returns the canonical fixture service name for a
// given state on the current OS. All names are intentionally generic
// (com.example.* / example-test-*) — no real service / bundle ids.
func serviceForState(state string) string {
	switch runtime.GOOS {
	case "darwin":
		return "com.example.test." + state
	case "linux":
		return "example-test-" + state
	default:
		return state
	}
}

// newTestDaemon spins up an httptest GraphQL server wired to the
// production resolver pipeline, so the E2E asserts the same code path
// the real daemon serves. Mirrors ws-b-host's E2E harness.
func newTestDaemon(t *testing.T, p *hostservice.Provider) *httptest.Server {
	t.Helper()
	cfg := gql.Config{Resolvers: resolvers.New(time.Now()).WithHostService(p)}
	gh := handler.New(gql.NewExecutableSchema(cfg))
	gh.AddTransport(transport.POST{})
	gh.AddTransport(transport.GET{})
	mux := http.NewServeMux()
	mux.Handle("/graphql", gh)
	return httptest.NewServer(mux)
}

// hostServicePayload mirrors the GraphQL HostService shape so we can
// JSON-decode the response without bringing in gqlgen's MarshalGQL.
type hostServicePayload struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Since    *string `json:"since"`
	ExitCode *int    `json:"exitCode"`
	LogTail  *string `json:"logTail"`
}

type hostPayload struct {
	ID           string               `json:"id"`
	MachineID    string               `json:"machineId"`
	HostServices []hostServicePayload `json:"hostServices"`
}

type queryPayload struct {
	Data struct {
		Host *hostPayload `json:"host"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
		Path    []any  `json:"path"`
	} `json:"errors"`
}

// postQuery POSTs the GraphQL query to the test server and decodes the
// response into queryPayload. Test failures are reported via t.Fatal.
func postQuery(t *testing.T, base, query string) queryPayload {
	t.Helper()
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		t.Fatalf("marshal query: %v", err)
	}
	resp, err := http.Post(base+"/graphql", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("daemon returned %d: %s", resp.StatusCode, string(raw))
	}
	var out queryPayload
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}
