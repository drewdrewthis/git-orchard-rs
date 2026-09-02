// accessors_test.go — the narrow read surface added for #612.
//
// Each accessor must answer its keyed or filtered question without
// cloning the whole cached graph, and must hand back values the caller
// cannot write through into the store.

package tmux

import (
	"sort"
	"testing"

	provider "github.com/drewdrewthis/orchardist/internal/server/adapter"
)

// buildGraphProvider seeds all four stores so the cross-type accessors
// have something to miss on as well as hit.
func buildGraphProvider(sessions []Session, windows []Window, panes []Pane, clients []Client) *Provider {
	p := New(NewAdapter("local"), nil)

	sk := make(map[SessionKey]Session, len(sessions))
	for _, s := range sessions {
		sk[s.Key] = s
	}
	p.sessions.ReplaceAll(sk, provider.SourcePoll, sessionsEqual)

	wk := make(map[WindowKey]Window, len(windows))
	for _, w := range windows {
		wk[w.Key] = w
	}
	p.windows.ReplaceAll(wk, provider.SourcePoll, windowsEqual)

	pk := make(map[PaneKey]Pane, len(panes))
	for _, pn := range panes {
		pk[pn.Key] = pn
	}
	p.panes.ReplaceAll(pk, provider.SourcePoll, panesEqual)

	ck := make(map[ClientKey]Client, len(clients))
	for _, c := range clients {
		ck[c.Key] = c
	}
	p.clients.ReplaceAll(ck, provider.SourcePoll, clientsEqual)

	return p
}

// graphFixture is the shared two-session / three-window / four-pane graph.
func graphFixture() *Provider {
	return buildGraphProvider(
		[]Session{
			{Key: SessionKey{Host: "local", Name: "alpha"}, CurrentWindow: 1},
			{Key: SessionKey{Host: "local", Name: "beta"}, CurrentWindow: 0},
		},
		[]Window{
			{Key: WindowKey{Host: "local", Session: "alpha", Index: 0}, Name: "a0", CurrentPane: "%1"},
			{Key: WindowKey{Host: "local", Session: "alpha", Index: 1}, Name: "a1", CurrentPane: "%3"},
			{Key: WindowKey{Host: "local", Session: "beta", Index: 0}, Name: "b0", CurrentPane: "%4"},
		},
		[]Pane{
			{Key: PaneKey{Host: "local", PaneID: "%1"}, WindowKey: WindowKey{Host: "local", Session: "alpha", Index: 0}},
			{Key: PaneKey{Host: "local", PaneID: "%2"}, WindowKey: WindowKey{Host: "local", Session: "alpha", Index: 0}},
			{Key: PaneKey{Host: "local", PaneID: "%3"}, WindowKey: WindowKey{Host: "local", Session: "alpha", Index: 1}},
			{Key: PaneKey{Host: "local", PaneID: "%4"}, WindowKey: WindowKey{Host: "local", Session: "beta", Index: 0}},
		},
		[]Client{
			{Key: ClientKey{Host: "local", ClientName: "c-alpha"}, Session: "alpha", CurrentPane: "%1"},
			{Key: ClientKey{Host: "local", ClientName: "c-beta"}, Session: "beta", CurrentPane: "%4"},
		},
	)
}

func paneIDs(panes []Pane) []string {
	out := make([]string, len(panes))
	for i, p := range panes {
		out[i] = p.Key.PaneID
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSessionByName_HitAndMiss(t *testing.T) {
	p := graphFixture()

	got, ok := p.SessionByName("local", "beta")
	if !ok {
		t.Fatal("SessionByName(beta): expected hit")
	}
	if got.Key.Name != "beta" {
		t.Errorf("SessionByName(beta) = %q", got.Key.Name)
	}
	if _, ok := p.SessionByName("local", "nope"); ok {
		t.Error("SessionByName(nope): expected miss")
	}
	if _, ok := p.SessionByName("other-host", "beta"); ok {
		t.Error("SessionByName on a foreign host: expected miss")
	}
}

func TestWindowByKey_HitAndMiss(t *testing.T) {
	p := graphFixture()

	got, ok := p.WindowByKey("local", "alpha", 1)
	if !ok {
		t.Fatal("WindowByKey(alpha,1): expected hit")
	}
	if got.Name != "a1" {
		t.Errorf("WindowByKey(alpha,1).Name = %q, want a1", got.Name)
	}
	if _, ok := p.WindowByKey("local", "alpha", 9); ok {
		t.Error("WindowByKey(alpha,9): expected miss")
	}
	if _, ok := p.WindowByKey("local", "beta", 1); ok {
		t.Error("WindowByKey(beta,1): expected miss — index 1 belongs to alpha")
	}
}

func TestClientByName_HitAndMiss(t *testing.T) {
	p := graphFixture()

	got, ok := p.ClientByName("local", "c-beta")
	if !ok {
		t.Fatal("ClientByName(c-beta): expected hit")
	}
	if got.Session != "beta" {
		t.Errorf("ClientByName(c-beta).Session = %q, want beta", got.Session)
	}
	if _, ok := p.ClientByName("local", "c-nope"); ok {
		t.Error("ClientByName(c-nope): expected miss")
	}
}

func TestWindowsBySession_ScopedToSession(t *testing.T) {
	p := graphFixture()

	got := p.WindowsBySession("local", "alpha")
	if len(got) != 2 {
		t.Fatalf("WindowsBySession(alpha) = %d windows, want 2", len(got))
	}
	for _, w := range got {
		if w.Key.Session != "alpha" {
			t.Errorf("WindowsBySession(alpha) leaked window from session %q", w.Key.Session)
		}
	}
	if got := p.WindowsBySession("local", "nope"); len(got) != 0 {
		t.Errorf("WindowsBySession(nope) = %d windows, want 0", len(got))
	}
}

func TestPanesByWindow_ScopedToWindow(t *testing.T) {
	p := graphFixture()

	got := p.PanesByWindow("local", "alpha", 0)
	if want := []string{"%1", "%2"}; !equalStrings(paneIDs(got), want) {
		t.Errorf("PanesByWindow(alpha,0) = %v, want %v", paneIDs(got), want)
	}
	if got := p.PanesByWindow("local", "alpha", 1); !equalStrings(paneIDs(got), []string{"%3"}) {
		t.Errorf("PanesByWindow(alpha,1) = %v, want [%%3]", paneIDs(got))
	}
	if got := p.PanesByWindow("local", "beta", 9); len(got) != 0 {
		t.Errorf("PanesByWindow(beta,9) = %d panes, want 0", len(got))
	}
}

func TestPanesByHost_ScopedToHost(t *testing.T) {
	p := graphFixture()

	if got := p.PanesByHost("local"); len(got) != 4 {
		t.Errorf("PanesByHost(local) = %d panes, want 4", len(got))
	}
	if got := p.PanesByHost("elsewhere"); len(got) != 0 {
		t.Errorf("PanesByHost(elsewhere) = %d panes, want 0", len(got))
	}
}

func TestSessionsByHost_ScopedToHost(t *testing.T) {
	p := graphFixture()

	if got := p.SessionsByHost("local"); len(got) != 2 {
		t.Errorf("SessionsByHost(local) = %d sessions, want 2", len(got))
	}
	if got := p.SessionsByHost("elsewhere"); len(got) != 0 {
		t.Errorf("SessionsByHost(elsewhere) = %d sessions, want 0", len(got))
	}
}

func TestClientsByHost_ScopedToHost(t *testing.T) {
	p := graphFixture()

	if got := p.ClientsByHost("local"); len(got) != 2 {
		t.Errorf("ClientsByHost(local) = %d clients, want 2", len(got))
	}
	if got := p.ClientsByHost("elsewhere"); len(got) != 0 {
		t.Errorf("ClientsByHost(elsewhere) = %d clients, want 0", len(got))
	}
}

func TestClientsBySession_ScopedToSession(t *testing.T) {
	p := graphFixture()

	got := p.ClientsBySession("local", "alpha")
	if len(got) != 1 || got[0].Key.ClientName != "c-alpha" {
		t.Fatalf("ClientsBySession(alpha) = %+v, want [c-alpha]", got)
	}
	if got := p.ClientsBySession("local", "nope"); len(got) != 0 {
		t.Errorf("ClientsBySession(nope) = %d clients, want 0", len(got))
	}
}

func TestClientsByCurrentPane_MatchesCurrentPane(t *testing.T) {
	p := graphFixture()

	got := p.ClientsByCurrentPane("local", "%4")
	if len(got) != 1 || got[0].Key.ClientName != "c-beta" {
		t.Fatalf("ClientsByCurrentPane(%%4) = %+v, want [c-beta]", got)
	}
	if got := p.ClientsByCurrentPane("local", "%2"); len(got) != 0 {
		t.Errorf("ClientsByCurrentPane(%%2) = %d clients, want 0 — no client is on it", len(got))
	}
}

// TestPaneAccessors_DoNotShareSliceWithStore is the immutability guarantee.
// Pane.WatchingTTYs is the one reference-typed field on a cached value: a
// plain struct copy hands the caller the store's own backing array, so a
// resolver could rewrite the cache by writing through it.
func TestPaneAccessors_DoNotShareSliceWithStore(t *testing.T) {
	seed := Pane{
		Key:          PaneKey{Host: "local", PaneID: "%7"},
		WindowKey:    WindowKey{Host: "local", Session: "alpha", Index: 0},
		WatchingTTYs: []string{"/dev/ttys001"},
	}
	p := buildGraphProvider(nil, nil, []Pane{seed}, nil)

	cases := map[string]func() []Pane{
		"PaneByID": func() []Pane {
			pn, ok := p.PaneByID("local", "%7")
			if !ok {
				t.Fatal("PaneByID(%7): expected hit")
			}
			return []Pane{pn}
		},
		"PanesByWindow":  func() []Pane { return p.PanesByWindow("local", "alpha", 0) },
		"PanesByHost":    func() []Pane { return p.PanesByHost("local") },
		"PanesBySession": func() []Pane { return p.PanesBySession("local", "alpha") },
	}

	for name, get := range cases {
		t.Run(name, func(t *testing.T) {
			got := get()
			if len(got) != 1 {
				t.Fatalf("%s returned %d panes, want 1", name, len(got))
			}
			got[0].WatchingTTYs[0] = "/dev/MUTATED"

			fresh, ok := p.PaneByID("local", "%7")
			if !ok {
				t.Fatal("PaneByID(%7) after mutation: entry vanished")
			}
			if fresh.WatchingTTYs[0] != "/dev/ttys001" {
				t.Errorf("%s let a caller write through into the store: WatchingTTYs[0] = %q, want /dev/ttys001",
					name, fresh.WatchingTTYs[0])
			}
		})
	}
}

// TestNarrowAccessors_DoNotCloneTheGraph asserts the accessors read the
// store directly rather than routing through Snapshot() — the #612 fix is
// worthless if the "narrow" accessor clones the graph on the way in.
func TestNarrowAccessors_DoNotCloneTheGraph(t *testing.T) {
	p := graphFixture()
	before := p.SnapshotCalls()

	p.PaneByID("local", "%1")
	p.SessionByName("local", "alpha")
	p.WindowByKey("local", "alpha", 0)
	p.ClientByName("local", "c-alpha")
	p.WindowsBySession("local", "alpha")
	p.PanesByWindow("local", "alpha", 0)
	p.PanesByHost("local")
	p.PanesBySession("local", "alpha")
	p.ClientsBySession("local", "alpha")
	p.ClientsByCurrentPane("local", "%1")
	p.SessionsByHost("local")
	p.ClientsByHost("local")

	if got := p.SnapshotCalls() - before; got != 0 {
		t.Errorf("narrow accessors made %d Snapshot() calls, want 0 (RULES.md O9)", got)
	}
}
