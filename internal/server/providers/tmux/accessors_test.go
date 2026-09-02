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

func TestWindowsOf_ScopedToSession(t *testing.T) {
	p := graphFixture()

	got := p.WindowsOf("local", "alpha")
	if len(got) != 2 {
		t.Fatalf("WindowsOf(alpha) = %d windows, want 2", len(got))
	}
	for _, w := range got {
		if w.Key.Session != "alpha" {
			t.Errorf("WindowsOf(alpha) leaked window from session %q", w.Key.Session)
		}
	}
	if got := p.WindowsOf("local", "nope"); len(got) != 0 {
		t.Errorf("WindowsOf(nope) = %d windows, want 0", len(got))
	}
}

func TestPanesOf_ScopedToWindow(t *testing.T) {
	p := graphFixture()

	got := p.PanesOf("local", "alpha", 0)
	if want := []string{"%1", "%2"}; !equalStrings(paneIDs(got), want) {
		t.Errorf("PanesOf(alpha,0) = %v, want %v", paneIDs(got), want)
	}
	if got := p.PanesOf("local", "alpha", 1); !equalStrings(paneIDs(got), []string{"%3"}) {
		t.Errorf("PanesOf(alpha,1) = %v, want [%%3]", paneIDs(got))
	}
	if got := p.PanesOf("local", "beta", 9); len(got) != 0 {
		t.Errorf("PanesOf(beta,9) = %d panes, want 0", len(got))
	}
}

func TestPanesOnHost_ScopedToHost(t *testing.T) {
	p := graphFixture()

	if got := p.PanesOnHost("local"); len(got) != 4 {
		t.Errorf("PanesOnHost(local) = %d panes, want 4", len(got))
	}
	if got := p.PanesOnHost("elsewhere"); len(got) != 0 {
		t.Errorf("PanesOnHost(elsewhere) = %d panes, want 0", len(got))
	}
}

func TestClientsOfSession_ScopedToSession(t *testing.T) {
	p := graphFixture()

	got := p.ClientsOfSession("local", "alpha")
	if len(got) != 1 || got[0].Key.ClientName != "c-alpha" {
		t.Fatalf("ClientsOfSession(alpha) = %+v, want [c-alpha]", got)
	}
	if got := p.ClientsOfSession("local", "nope"); len(got) != 0 {
		t.Errorf("ClientsOfSession(nope) = %d clients, want 0", len(got))
	}
}

func TestClientsWatchingPane_MatchesCurrentPane(t *testing.T) {
	p := graphFixture()

	got := p.ClientsWatchingPane("local", "%4")
	if len(got) != 1 || got[0].Key.ClientName != "c-beta" {
		t.Fatalf("ClientsWatchingPane(%%4) = %+v, want [c-beta]", got)
	}
	if got := p.ClientsWatchingPane("local", "%2"); len(got) != 0 {
		t.Errorf("ClientsWatchingPane(%%2) = %d clients, want 0 — no client is on it", len(got))
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
		"PanesOf":        func() []Pane { return p.PanesOf("local", "alpha", 0) },
		"PanesOnHost":    func() []Pane { return p.PanesOnHost("local") },
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
	p.WindowsOf("local", "alpha")
	p.PanesOf("local", "alpha", 0)
	p.PanesOnHost("local")
	p.PanesBySession("local", "alpha")
	p.ClientsOfSession("local", "alpha")
	p.ClientsWatchingPane("local", "%1")

	if got := p.SnapshotCalls() - before; got != 0 {
		t.Errorf("narrow accessors made %d Snapshot() calls, want 0 (RULES.md O9)", got)
	}
}
