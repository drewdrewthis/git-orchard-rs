// Tests for the daemon-UX bundle (#469): SchemaSDL, WorkView, DaemonState.
//
// Subscription resolvers depend on provider invalidation streams that
// are exercised end-to-end by the existing nodechanged_e2e_test.go and
// pr_enrich_e2e_test.go suites; these tests focus on the cheap, pure
// resolvers that don't need a full daemon stack.

package resolvers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
)

func TestSchemaSDL_NonEmpty(t *testing.T) {
	sdl := SchemaSDL()
	if sdl == "" {
		t.Fatal("expected embedded schema SDL to be non-empty")
	}
	if !strings.Contains(sdl, "type Query") {
		t.Errorf("expected schema SDL to contain 'type Query'; got first 200 chars: %q", sdl[:min(200, len(sdl))])
	}
	if !strings.Contains(sdl, "schemaSDL: String!") {
		t.Errorf("expected SDL to contain its own schemaSDL field declaration (sanity check that the embedded copy is current)")
	}
}

func TestSchemaSDL_MatchesRootSource(t *testing.T) {
	// The embedded copy must match the canonical schema.graphql at the
	// repo root. `make generate` mirrors the file; this test catches
	// stale embeds before they hit production.
	root, err := os.ReadFile("../../../schema.graphql")
	if err != nil {
		t.Skipf("repo-root schema not readable (%v) — skipping drift check", err)
	}
	if string(root) != SchemaSDL() {
		t.Errorf(
			"embedded schema drift: internal/server/resolvers/schema.graphql is out of sync with repo-root schema.graphql. Run `make generate` to mirror.",
		)
	}
}

func TestSchemaSDLResolver_ReturnsEmbedded(t *testing.T) {
	r := &queryResolver{New(time.Now())}
	got, err := r.SchemaSdl(context.Background())
	if err != nil {
		t.Fatalf("SchemaSdl: unexpected error: %v", err)
	}
	if got != SchemaSDL() {
		t.Errorf("SchemaSdl returned %d bytes; expected %d (embedded)", len(got), len(SchemaSDL()))
	}
}

func TestDaemonState_ReportsProvidersAndUptime(t *testing.T) {
	startedAt := time.Now().Add(-10 * time.Second)
	r := &queryResolver{New(startedAt)}
	state, err := r.DaemonState(context.Background())
	if err != nil {
		t.Fatalf("DaemonState: %v", err)
	}
	if state.UptimeS < 1 {
		t.Errorf("uptime %ds < 1s — clock skew?", state.UptimeS)
	}
	if state.StartedAt == "" {
		t.Errorf("startedAt empty")
	}
	// #666 collapsed contracts onto jsonl sentinels folded by
	// scripts/fold-contracts.sh and deleted the daemon domain, so DaemonState
	// no longer reports a "contracts" provider.
	wantProviders := map[string]bool{
		"host": true, "git": true, "ps": true, "tmux": true,
		"claudeProjects": true, "claudeAccount": true, "claudeInstance": true,
		"hostService": true, "gh": true, "peerProxy": true,
	}
	for _, p := range state.Providers {
		if _, ok := wantProviders[p.Name]; !ok {
			t.Errorf("unexpected provider %q in DaemonState", p.Name)
		}
		if p.Configured {
			t.Errorf("provider %q reported configured on a bare resolver", p.Name)
		}
		delete(wantProviders, p.Name)
	}
	if len(wantProviders) > 0 {
		t.Errorf("missing providers from DaemonState: %v", wantProviders)
	}
}

func TestWorkView_EmptyResolverReturnsEmptyLists(t *testing.T) {
	// A resolver with no providers wired should return empty slices and
	// a Meta envelope flagging the failure (#469 F1) — not nil panics.
	r := &queryResolver{New(time.Now())}
	view, err := r.WorkView(context.Background())
	if err != nil {
		t.Fatalf("WorkView: unexpected error: %v", err)
	}
	if view == nil {
		t.Fatal("WorkView returned nil")
	}
	if view.Repos == nil || view.TmuxSessions == nil || view.ClaudeInstances == nil {
		t.Errorf("WorkView returned nil slices; expected empty: %+v", view)
	}
	if view.Meta == nil {
		t.Fatal("WorkView.Meta nil")
	}
	if view.Meta.Provider != metaProviderWorkView {
		t.Errorf("Meta.Provider = %q; want %q", view.Meta.Provider, metaProviderWorkView)
	}
	// On a bare resolver, projects+claudeInstances both error out
	// (providers nil), so failureReason should be populated.
	if view.Meta.FailureReason == nil {
		t.Errorf("expected Meta.FailureReason on bare resolver; got nil")
	}
	if view.Meta.LastSuccessfulFetchAt != nil {
		t.Errorf("LastSuccessfulFetchAt should be nil when failure is reported; got %v", *view.Meta.LastSuccessfulFetchAt)
	}
}

func TestSplitRepo(t *testing.T) {
	cases := []struct {
		in          string
		owner, name string
		ok          bool
	}{
		{"owner/name", "owner", "name", true},
		{"o/n", "o", "n", true},
		{"", "", "", false},
		{"owner", "", "", false},
		{"owner/", "", "", false},
		{"/name", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			o, n, ok := splitRepo(c.in)
			if ok != c.ok || o != c.owner || n != c.name {
				t.Errorf("splitRepo(%q) = (%q, %q, %v); want (%q, %q, %v)", c.in, o, n, ok, c.owner, c.name, c.ok)
			}
		})
	}
}

// Compile-time assertion that the resolver root satisfies the gqlgen
// interfaces for both Query and Subscription — catches missing methods
// before runtime.
var (
	_ graphql1.QueryResolver        = (*queryResolver)(nil)
	_ graphql1.SubscriptionResolver = (*subscriptionResolver)(nil)
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
