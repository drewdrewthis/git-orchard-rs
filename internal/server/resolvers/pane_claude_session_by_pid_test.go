package resolvers

// Tests for issue #743 — ClaudeInstance.sessionUuid must be resolved by the
// pane's own live pid via the ~/.claude/sessions/<pid>.json registry, not by
// cwd. The pre-fix cwdToSession map keyed sessionUuid by cwd (last-wins), so
// every REPL sharing a worktree cwd collapsed onto one shared sessionUuid.
//
// The logic under test is the pure helper resolveSessionUUID (functional core);
// AC-10 and AC-11 additionally drive the real providers / single-pane resolver.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudeprojects"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudesessions"
	psprovider "github.com/drewdrewthis/orchardist/internal/server/providers/ps"
)

// fakeSessionRegistry is an in-memory paneSessionRegistry (a fake, not a mock —
// we own the boundary) keyed by pid.
type fakeSessionRegistry map[int]claudesessions.Session

func (f fakeSessionRegistry) SessionByPid(pid int) (claudesessions.Session, bool) {
	s, ok := f[pid]
	return s, ok
}

const paneCwd = "/Users/dev/worktrees/issue743"

// AC-1 — Registry-first resolution: a registry entry for the pane's own pid
// (cwd cross-checked) wins over the cwdToSession value.
func TestResolveSessionUUID_RegistryWinsOverCwd(t *testing.T) {
	reg := fakeSessionRegistry{8164: {Pid: 8164, SessionUUID: "reg-uuid", Cwd: paneCwd}}
	idx := map[string]cwdMatch{paneCwd: {sessionUUID: "cwd-uuid", count: 1}}

	got := resolveSessionUUID("local", 8164, paneCwd, reg, idx)
	if got != "reg-uuid" {
		t.Errorf("sessionUuid = %q, want %q (registry must win over cwd map)", got, "reg-uuid")
	}
	if got == "cwd-uuid" {
		t.Errorf("sessionUuid resolved to the cwd-map value %q — collision not fixed", got)
	}
}

// AC-2 — Collision eliminated: two panes, one cwd, two live pids, two registry
// entries → two distinct non-empty sessionUuids.
func TestResolveSessionUUID_TwoPanesOneCwd_DistinctSessions(t *testing.T) {
	reg := fakeSessionRegistry{
		8164: {Pid: 8164, SessionUUID: "uuid-a", Cwd: paneCwd},
		8165: {Pid: 8165, SessionUUID: "uuid-b", Cwd: paneCwd},
	}
	// cwd map is ambiguous (both convs share the cwd); registry must override.
	idx := map[string]cwdMatch{paneCwd: {sessionUUID: "uuid-b", count: 2}}

	a := resolveSessionUUID("local", 8164, paneCwd, reg, idx)
	b := resolveSessionUUID("local", 8165, paneCwd, reg, idx)
	if a != "uuid-a" {
		t.Errorf("instance A sessionUuid = %q, want %q", a, "uuid-a")
	}
	if b != "uuid-b" {
		t.Errorf("instance B sessionUuid = %q, want %q", b, "uuid-b")
	}
	if a == b {
		t.Errorf("both instances share sessionUuid %q — collision NOT eliminated", a)
	}
}

// AC-4 — Ambiguous cwd → nil: no registry entry and ≥2 conversations share the
// cwd → "" (nil sessionUuid), no guess.
func TestResolveSessionUUID_AmbiguousCwd_YieldsEmpty(t *testing.T) {
	reg := fakeSessionRegistry{} // no entry for the pane's pid
	idx := map[string]cwdMatch{paneCwd: {sessionUUID: "some-uuid", count: 2}}

	got := resolveSessionUUID("local", 8164, paneCwd, reg, idx)
	if got != "" {
		t.Errorf("sessionUuid = %q, want \"\" (ambiguous cwd must not guess)", got)
	}
}

// AC-5 — Stale pid excluded: a registry entry for a dead pid 9999 is never
// attributed to a live instance on pid 8164 (lookup keys on the pane's own pid).
func TestResolveSessionUUID_StalePidNotAttributed(t *testing.T) {
	reg := fakeSessionRegistry{9999: {Pid: 9999, SessionUUID: "stale-uuid", Cwd: paneCwd}}
	idx := map[string]cwdMatch{paneCwd: {sessionUUID: "cwd-uuid", count: 1}}

	got := resolveSessionUUID("local", 8164, paneCwd, reg, idx)
	if got == "stale-uuid" {
		t.Errorf("sessionUuid = %q — a dead pid's registry file must never be attributed", got)
	}
	if got != "cwd-uuid" {
		t.Errorf("sessionUuid = %q, want the cwd fallback %q", got, "cwd-uuid")
	}
}

// AC-6 — Malformed registry degrades to fallback: an absent/garbage entry
// (SessionByPid returns false) falls back to the cwd value, never an error.
func TestResolveSessionUUID_MissingRegistryEntry_FallsBackToCwd(t *testing.T) {
	reg := fakeSessionRegistry{} // SessionByPid always false — mirrors garbage/missing file
	idx := map[string]cwdMatch{paneCwd: {sessionUUID: "cwd-uuid", count: 1}}

	got := resolveSessionUUID("local", 8164, paneCwd, reg, idx)
	if got != "cwd-uuid" {
		t.Errorf("sessionUuid = %q, want cwd-fallback %q", got, "cwd-uuid")
	}
}

// AC-12 — Pid reuse rejected: registry entry alive but its recorded cwd differs
// from the pane's resolved cwd → not attributed; falls back to cwd/nil.
func TestResolveSessionUUID_PidReuseCwdMismatch_Rejected(t *testing.T) {
	reg := fakeSessionRegistry{8164: {Pid: 8164, SessionUUID: "reused-uuid", Cwd: "/some/other/cwd"}}
	idx := map[string]cwdMatch{paneCwd: {sessionUUID: "cwd-uuid", count: 1}}

	got := resolveSessionUUID("local", 8164, paneCwd, reg, idx)
	if got == "reused-uuid" {
		t.Errorf("sessionUuid = %q — a cwd-mismatched (pid-reuse) entry must not be attributed", got)
	}
	if got != "cwd-uuid" {
		t.Errorf("sessionUuid = %q, want cwd fallback %q", got, "cwd-uuid")
	}
}

// AC-13 — Remote host uses fallback: a non-local host never has a local
// registry file attributed even when the pid matches.
func TestResolveSessionUUID_RemoteHost_IgnoresLocalRegistry(t *testing.T) {
	reg := fakeSessionRegistry{8164: {Pid: 8164, SessionUUID: "local-reg-uuid", Cwd: paneCwd}}
	idx := map[string]cwdMatch{paneCwd: {sessionUUID: "cwd-uuid", count: 1}}

	got := resolveSessionUUID("remote-box", 8164, paneCwd, reg, idx)
	if got == "local-reg-uuid" {
		t.Errorf("sessionUuid = %q — a remote pane must never attribute a local registry file", got)
	}
	if got != "cwd-uuid" {
		t.Errorf("sessionUuid = %q, want cwd fallback %q", got, "cwd-uuid")
	}
}

// AC-10 — Value-equivalence: the registry's sessionId is byte-equal to the
// jsonl-derived ID.SessionUUID for the same live session.
func TestSessionByPid_ByteEqualsJsonlSessionUUID(t *testing.T) {
	const sessionUUID = "abc-123-def-456"
	const pid = 8164

	// Fake claudeprojects jsonl for one session.
	projRoot := t.TempDir()
	slug := filepath.Join(projRoot, "-Users-dev-worktrees-issue743")
	if err := os.MkdirAll(slug, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := `{"type":"user","cwd":"` + paneCwd + `","sessionId":"` + sessionUUID + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(slug, sessionUUID+".jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}
	cp := claudeprojects.New(projRoot, "local", nil)
	if err := cp.Start(context.Background()); err != nil {
		t.Fatalf("claudeprojects Start: %v", err)
	}
	convs, err := cp.List(context.Background())
	if err != nil {
		t.Fatalf("claudeprojects List: %v", err)
	}
	var jsonlUUID string
	for _, c := range convs {
		if c.ID.SessionUUID == sessionUUID {
			jsonlUUID = c.ID.SessionUUID
		}
	}
	if jsonlUUID == "" {
		t.Fatalf("claudeprojects did not surface the fake conversation %q", sessionUUID)
	}

	// Fake registry entry for the same session.
	regRoot := t.TempDir()
	writeRegistryFile(t, regRoot, pid, sessionUUID, paneCwd)
	reg := claudesessions.New(regRoot, nil)
	s, ok := reg.SessionByPid(pid)
	if !ok {
		t.Fatalf("SessionByPid(%d) not found", pid)
	}
	if s.SessionUUID != jsonlUUID {
		t.Errorf("registry sessionId %q != jsonl ID.SessionUUID %q — values must be byte-equal", s.SessionUUID, jsonlUUID)
	}
}

// AC-11 — Single-pane path: the tmuxPane.claudeInstance resolver (cwdToSession
// == nil branch) resolves sessionUuid from the registry by pid. Uses the test
// process's own pid with a REAL ps provider so cwd resolution (lsof on darwin,
// /proc on linux) is deterministic and portable; the registry file's cwd is set
// to exactly what the resolver will resolve, so the pid-reuse guard passes.
//
// Retagged @integration (not @unit): Resolver.PS is a concrete *ps.Provider,
// not an interface, so there is no seam to fake LoadCwd without a
// larger provider-interface refactor across the resolvers package (out of
// scope for #743). On Linux this shells out to nothing — LoadCwd reads
// /proc/<pid>/cwd via readlink — but on darwin it invokes lsof, hence real I/O.
func TestTmuxPaneClaudeInstance_ResolvesSessionUUIDFromRegistry(t *testing.T) {
	pid := os.Getpid()
	paneID := "%7"
	ctx := context.Background()

	tmuxProv := buildTmuxProvider(t, []string{
		paneRowWithCommand("solo", paneID, pid, "claude"),
	})
	// Real ps provider: the fake runner used by buildPsProvider cannot answer
	// the lsof cwd shellout, so cwd would be "" and the registry path unreachable.
	psProv := psprovider.New(psprovider.NewAdapter(testHost), nil)
	if err := psProv.Start(ctx); err != nil {
		t.Fatalf("ps provider Start: %v", err)
	}

	// Discover the exact cwd the resolver will see, so the pid-reuse guard
	// (registry.cwd == pane cwd) passes regardless of OS / symlink expansion.
	cwd, err := psProv.LoadCwd(ctx, pid)
	if err != nil || cwd == "" {
		t.Fatalf("could not resolve own cwd for pid %d (err=%v); test seam broken", pid, err)
	}

	regRoot := t.TempDir()
	writeRegistryFile(t, regRoot, pid, "solo-uuid", cwd)
	reg := claudesessions.New(regRoot, nil)

	r := &tmuxPaneResolver{&Resolver{Tmux: tmuxProv, PS: psProv, ClaudeSessions: reg}}
	obj := &graphql1.TmuxPane{ID: paneGQLID(testHost, paneID)}

	got, err := r.ClaudeInstance(ctx, obj)
	if err != nil {
		t.Fatalf("ClaudeInstance() error: %v", err)
	}
	if got == nil {
		t.Fatal("ClaudeInstance() = nil, want an instance for a live claude pane")
	}
	if got.SessionUUID == nil {
		t.Fatal("sessionUuid = nil, want \"solo-uuid\" from the registry (single-pane branch not wired)")
	}
	if *got.SessionUUID != "solo-uuid" {
		t.Errorf("sessionUuid = %q, want %q", *got.SessionUUID, "solo-uuid")
	}
}

// writeRegistryFile writes a Claude Code <pid>.json registry file under root.
func writeRegistryFile(t *testing.T, root string, pid int, sessionID, cwd string) {
	t.Helper()
	rec := map[string]any{
		"pid":       pid,
		"sessionId": sessionID,
		"cwd":       cwd,
		"startedAt": 1788556382449,
		"version":   "2.1.261",
		"kind":      "interactive",
		"agent":     "lead",
		"status":    "busy",
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid)+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}
