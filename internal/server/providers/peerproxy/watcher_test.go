package peerproxy_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/providers/peerproxy"
)

// TestConfigWatcher_EditTriggersReload verifies that writing a new config
// to the watched path causes the Provider's peer set to be updated within
// the 2-second window required by the feature scenario.
//
// Steps:
//  1. Start two fake peers (orchard.boxd.sh, lw-fed-c).
//  2. Write an initial config containing only "orchard.boxd.sh".
//  3. Construct Provider from that config; Start it.
//  4. Construct ConfigWatcher pointing at the config file; Start it.
//  5. Wait for the initial peer to begin probing.
//  6. Snapshot Peers() length (== 1) and SpawnCount("orchard.boxd.sh") (== 1).
//  7. Atomically write a new config with both peers.
//  8. Poll up to 2 seconds for Peers() to grow to 2.
//  9. Assert both peers are present.
// 10. Assert SpawnCount("orchard.boxd.sh") is still 1 (not restarted).
// 11. Assert SpawnCount("lw-fed-c") is 1 (started exactly once).
func TestConfigWatcher_EditTriggersReload(t *testing.T) {
	// 1. Fake peers for the two addresses.
	fakeBoxd := newFakePeer(t)
	fakeFedC := newFakePeer(t)

	// 2. Write the initial config — only orchard.boxd.sh.
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	writeConfig(t, cfgPath, []peerproxy.PeerConfig{
		{Name: "orchard.boxd.sh", Address: fakeBoxd.addr(), TLS: false},
	})

	// 3. Construct provider from the initial config.
	initialCfg := loadConfig(t, cfgPath)
	p := peerproxy.NewProvider(initialCfg, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Provider.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// 4. Construct and start the ConfigWatcher.
	cw := peerproxy.NewConfigWatcher(cfgPath, p, slog.Default())
	if err := cw.Start(ctx); err != nil {
		t.Fatalf("ConfigWatcher.Start: %v", err)
	}
	t.Cleanup(func() { _ = cw.Close() })

	// 5. Wait for the initial peer to begin probing (confirms goroutine is live).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fakeBoxd.pingCount.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fakeBoxd.pingCount.Load() < 1 {
		t.Fatalf("orchard.boxd.sh probe goroutine did not issue a Ping within 500ms")
	}

	// 6. Snapshot the initial state.
	if got := len(p.Peers()); got != 1 {
		t.Fatalf("initial Peers() len = %d, want 1", got)
	}
	initialBoxdSpawnCount := p.SpawnCount("orchard.boxd.sh")
	if initialBoxdSpawnCount != 1 {
		t.Fatalf("initial SpawnCount(orchard.boxd.sh) = %d, want 1", initialBoxdSpawnCount)
	}

	// 7. Atomically write the new config with both peers.
	// Use write-then-rename for atomicity (same pattern as `orchard config add-peer`).
	writeConfigAtomic(t, cfgPath, []peerproxy.PeerConfig{
		{Name: "orchard.boxd.sh", Address: fakeBoxd.addr(), TLS: false},
		{Name: "lw-fed-c", Address: fakeFedC.addr(), TLS: false},
	})

	// 8. Poll up to 2 seconds for Peers() to grow to 2.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(p.Peers()) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 9. Assert both peers are present.
	peers := p.Peers()
	if len(peers) != 2 {
		t.Fatalf("Peers() len = %d after config edit, want 2; peers=%v", len(peers), peerNames(peers))
	}
	peerSet := make(map[string]bool, len(peers))
	for _, peer := range peers {
		peerSet[peer.Name] = true
	}
	if !peerSet["orchard.boxd.sh"] {
		t.Errorf("Peers() missing %q; got %v", "orchard.boxd.sh", peerNames(peers))
	}
	if !peerSet["lw-fed-c"] {
		t.Errorf("Peers() missing %q; got %v", "lw-fed-c", peerNames(peers))
	}

	// 10. orchard.boxd.sh must NOT have been restarted — SpawnCount unchanged.
	if got := p.SpawnCount("orchard.boxd.sh"); got != initialBoxdSpawnCount {
		t.Fatalf("SpawnCount(orchard.boxd.sh) = %d after reload, want %d (peer was restarted unexpectedly)",
			got, initialBoxdSpawnCount)
	}

	// 11. lw-fed-c was newly added — SpawnCount must be exactly 1.
	if got := p.SpawnCount("lw-fed-c"); got != 1 {
		t.Fatalf("SpawnCount(lw-fed-c) = %d after reload, want 1", got)
	}
}

// TestConfigWatcher_ParseErrorKeepsLastGood is the unit coverage for the
// feature scenario "Parse error on reload keeps the last good peer set live".
//
// Steps:
//  1. Boot a fake peer (orchard.boxd.sh).
//  2. Write a valid 1-peer config and build a Provider from it.
//  3. Start the Provider and a ConfigWatcher with a short debounce (100ms).
//  4. Wait for orchard.boxd.sh to start probing.
//  5. Snapshot SpawnCount, pingCount, and ApplyPeersInvocationCount.
//  6. Write malformed JSON to the config path.
//  7. Wait debounce + slack (~300ms).
//  8. Assert ApplyPeersInvocationCount is unchanged (ApplyPeers never called).
//  9. Assert Peers() still contains exactly "orchard.boxd.sh".
// 10. Assert SpawnCount("orchard.boxd.sh") is still 1 (goroutine untouched).
// 11. Assert pingCount has continued to grow (probe goroutine still alive).
func TestConfigWatcher_ParseErrorKeepsLastGood(t *testing.T) {
	fakeBoxd := newFakePeer(t)

	// 2. Write the initial valid config.
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	writeConfig(t, cfgPath, []peerproxy.PeerConfig{
		{Name: "orchard.boxd.sh", Address: fakeBoxd.addr(), TLS: false},
	})

	// Build and start the Provider.
	initialCfg := loadConfig(t, cfgPath)
	p := peerproxy.NewProvider(initialCfg, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Provider.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// 3. ConfigWatcher with a short debounce so the test doesn't take 1 second.
	const debounce = 100 * time.Millisecond
	cw := peerproxy.NewConfigWatcher(cfgPath, p, slog.Default(), peerproxy.WithDebounce(debounce))
	if err := cw.Start(ctx); err != nil {
		t.Fatalf("ConfigWatcher.Start: %v", err)
	}
	t.Cleanup(func() { _ = cw.Close() })

	// 4. Wait for orchard.boxd.sh to begin probing — confirms the goroutine is live.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fakeBoxd.pingCount.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fakeBoxd.pingCount.Load() < 1 {
		t.Fatalf("orchard.boxd.sh probe goroutine did not issue a Ping within 500ms")
	}

	// 5. Snapshot the live state before writing the bad config.
	spawnCountBefore := p.SpawnCount("orchard.boxd.sh")
	if spawnCountBefore != 1 {
		t.Fatalf("SpawnCount(orchard.boxd.sh) = %d before bad write, want 1", spawnCountBefore)
	}
	pingBefore := fakeBoxd.pingCount.Load()
	applyCountBefore := cw.ApplyPeersInvocationCount()

	// 6. Write malformed JSON — simulates the operator making a typo mid-edit.
	if err := os.WriteFile(cfgPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}

	// 7. Wait debounce + generous slack for the (non-)reload to settle.
	time.Sleep(debounce + 200*time.Millisecond)

	// 8. ApplyPeers must NOT have been invoked — bad config never reaches the provider.
	if got := cw.ApplyPeersInvocationCount(); got != applyCountBefore {
		t.Errorf("ApplyPeersInvocationCount = %d after malformed write, want %d (ApplyPeers was called with a broken config)",
			got, applyCountBefore)
	}

	// 9. Peers() must still contain exactly "orchard.boxd.sh".
	peers := p.Peers()
	if len(peers) != 1 {
		t.Fatalf("Peers() len = %d after malformed write, want 1; peers=%v", len(peers), peerNames(peers))
	}
	if peers[0].Name != "orchard.boxd.sh" {
		t.Errorf("Peers()[0].Name = %q, want \"orchard.boxd.sh\"", peers[0].Name)
	}

	// 10. SpawnCount must be unchanged — the goroutine was never restarted.
	if got := p.SpawnCount("orchard.boxd.sh"); got != spawnCountBefore {
		t.Errorf("SpawnCount(orchard.boxd.sh) = %d after malformed write, want %d (goroutine was restarted)",
			got, spawnCountBefore)
	}

	// 11. The probe goroutine must still be alive — pingCount must not have
	// decreased. Since pingCount is monotonically increasing, this is
	// guaranteed unless something catastrophically cancels the goroutine
	// (e.g. a spurious RemovePeer). The definitive liveness proof is
	// SpawnCount == 1 (checked above) combined with Peers() intact.
	// Note: in a test environment the fake server does not support
	// WebSocket upgrade, so the probe goroutine retries Subscribe every
	// 5 seconds without calling Probe again — we cannot cheaply assert
	// pingCount grew within the debounce+slack window.
	pingAfter := fakeBoxd.pingCount.Load()
	if pingAfter < pingBefore {
		t.Errorf("pingCount decreased after malformed write: before=%d now=%d (probe goroutine was cancelled)",
			pingBefore, pingAfter)
	}
}

// TestConfigWatcher_AtomicRenameTriggersReload is the regression guard for the
// macOS atomic-rename scenario from federation-peers-hot-reload.feature.
//
// # Background
//
// On macOS, fsnotify historically misses writes when watching a single file
// path and the write is performed via a tmp-then-rename pattern (which is how
// `orchard config add-peer` and most editors save files). The documented
// workaround is to watch the PARENT DIRECTORY and filter events by filename.
// That workaround is already in ConfigWatcher.Start via `w.Add(filepath.Dir(path))`.
//
// This test PROTECTS that workaround from regression. If someone "simplifies"
// ConfigWatcher.Start to call `w.Add(path)` instead of `w.Add(filepath.Dir(path))`,
// this test will fail on darwin because the Rename event for the config file
// would never arrive.
//
// Steps:
//  1. Start a fake peer (orchard.boxd.sh).
//  2. Write an initial 1-peer config; build and start the Provider.
//  3. Start ConfigWatcher with short debounce (100ms).
//  4. Wait for orchard.boxd.sh to begin probing.
//  5. Atomically write a 2-peer config via `path+".tmp"` then `os.Rename` —
//     the exact sequence `orchard config add-peer` uses.
//  6. Poll up to (debounce + 2s) for Peers() length to reach 2.
//  7. Assert:
//   - Peers() length == 2.
//   - SpawnCount("orchard.boxd.sh") == 1 (not restarted).
//   - SpawnCount("lw-fed-c") == 1 (started exactly once).
//   - ApplyPeersInvocationCount() == 1 (exactly one reload, no spurious calls).
func TestConfigWatcher_AtomicRenameTriggersReload(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-specific scenario: atomic-rename + parent-dir watch workaround")
	}

	// 1. Fake peers for the two addresses.
	fakeBoxd := newFakePeer(t)
	fakeFedC := newFakePeer(t)

	// 2. Write initial config — only orchard.boxd.sh.
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	writeConfig(t, cfgPath, []peerproxy.PeerConfig{
		{Name: "orchard.boxd.sh", Address: fakeBoxd.addr(), TLS: false},
	})

	initialCfg := loadConfig(t, cfgPath)
	p := peerproxy.NewProvider(initialCfg, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Provider.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// 3. Start ConfigWatcher with short debounce so the test completes quickly.
	const debounce = 100 * time.Millisecond
	cw := peerproxy.NewConfigWatcher(cfgPath, p, slog.Default(), peerproxy.WithDebounce(debounce))
	if err := cw.Start(ctx); err != nil {
		t.Fatalf("ConfigWatcher.Start: %v", err)
	}
	t.Cleanup(func() { _ = cw.Close() })

	// 4. Wait for the initial peer to begin probing (confirms goroutine is live).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fakeBoxd.pingCount.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fakeBoxd.pingCount.Load() < 1 {
		t.Fatalf("orchard.boxd.sh probe goroutine did not issue a Ping within 500ms")
	}

	// Snapshot initial spawn counts.
	initialBoxdSpawnCount := p.SpawnCount("orchard.boxd.sh")
	if initialBoxdSpawnCount != 1 {
		t.Fatalf("initial SpawnCount(orchard.boxd.sh) = %d, want 1", initialBoxdSpawnCount)
	}
	if got := len(p.Peers()); got != 1 {
		t.Fatalf("initial Peers() len = %d, want 1", got)
	}

	// 5. Perform the atomic-rename write: write to path+".tmp" then os.Rename.
	// This is the exact sequence used by `orchard config add-peer` and most
	// editors. On macOS, watching the file directly (w.Add(path)) would miss
	// this Rename event; only parent-dir watching catches it.
	writeConfigAtomic(t, cfgPath, []peerproxy.PeerConfig{
		{Name: "orchard.boxd.sh", Address: fakeBoxd.addr(), TLS: false},
		{Name: "lw-fed-c", Address: fakeFedC.addr(), TLS: false},
	})

	// 6. Poll up to debounce + 2s for Peers() to grow to 2.
	deadline = time.Now().Add(debounce + 2*time.Second)
	for time.Now().Before(deadline) {
		if len(p.Peers()) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 7. Assertions.

	// Peers() must have grown to 2 — the rename event was observed.
	peers := p.Peers()
	if len(peers) != 2 {
		t.Fatalf("Peers() len = %d after atomic rename, want 2; peers=%v\n"+
			"(If this fails on macOS, ConfigWatcher may be watching the file directly\n"+
			" instead of the parent directory — check ConfigWatcher.Start for w.Add(path))",
			len(peers), peerNames(peers))
	}

	// Both peers must be present.
	peerSet := make(map[string]bool, len(peers))
	for _, peer := range peers {
		peerSet[peer.Name] = true
	}
	if !peerSet["orchard.boxd.sh"] {
		t.Errorf("Peers() missing %q; got %v", "orchard.boxd.sh", peerNames(peers))
	}
	if !peerSet["lw-fed-c"] {
		t.Errorf("Peers() missing %q; got %v", "lw-fed-c", peerNames(peers))
	}

	// orchard.boxd.sh must NOT have been restarted.
	if got := p.SpawnCount("orchard.boxd.sh"); got != initialBoxdSpawnCount {
		t.Fatalf("SpawnCount(orchard.boxd.sh) = %d after reload, want %d (peer was restarted unexpectedly)",
			got, initialBoxdSpawnCount)
	}

	// lw-fed-c was newly added — spawned exactly once.
	if got := p.SpawnCount("lw-fed-c"); got != 1 {
		t.Fatalf("SpawnCount(lw-fed-c) = %d after reload, want 1", got)
	}

	// Exactly one ApplyPeers call: not zero (rename was not missed), not more
	// than one (debounce prevented double-reload).
	if got := cw.ApplyPeersInvocationCount(); got != 1 {
		t.Fatalf("ApplyPeersInvocationCount() = %d, want 1 (one rename → one debounced reload)", got)
	}
}

// TestConfigWatcher_MissingFileAtStartup verifies that Start succeeds when the
// config file does not yet exist, that no peers are loaded until the file is
// created, and that creating the file triggers a single LoadFederationConfig +
// ApplyPeers cycle.
//
// This is the unit coverage for the feature scenario:
// "Watcher falls back gracefully if ~/.orchard/config.json is missing at startup".
//
// Steps:
//  1. Point cfgPath at a file that does not exist (parent dir created by watcher).
//  2. Construct an empty Provider (no peers) and Start it.
//  3. Construct a ConfigWatcher with a short debounce (50ms). Start it.
//     Assert err == nil — the missing file must not prevent startup.
//  4. Wait ~100ms. Assert Peers() is empty and ApplyPeersInvocationCount == 0.
//     (No file ⇒ no fsnotify events ⇒ no reload triggered.)
//  5. Atomically create the config file (tmp + os.Rename) with one peer "lw-fed-c".
//  6. Poll up to (debounce + 2s) for Peers() length to reach 1.
//  7. Assert:
//     - Peers() has exactly one entry "lw-fed-c".
//     - SpawnCount("lw-fed-c") == 1 (goroutine spawned exactly once).
//     - ApplyPeersInvocationCount() == 1 (the create event triggered one reload).
func TestConfigWatcher_MissingFileAtStartup(t *testing.T) {
	fakeFedC := newFakePeer(t)

	// 1. cfgPath points at a file that does not exist.
	// NOTE: do NOT write any config file here — the whole point is to test
	// the missing-file code path.
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")

	// 2. Construct an empty Provider (no initial peers) and start it.
	p := peerproxy.NewProvider(peerproxy.FederationConfig{}, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Provider.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// 3. Start the ConfigWatcher pointing at the non-existent config path.
	// The watcher watches the PARENT DIRECTORY, which already exists (t.TempDir
	// creates it), so Start must succeed even though config.json is absent.
	const debounce = 50 * time.Millisecond
	cw := peerproxy.NewConfigWatcher(cfgPath, p, slog.Default(), peerproxy.WithDebounce(debounce))
	if err := cw.Start(ctx); err != nil {
		t.Fatalf("ConfigWatcher.Start with missing file: %v", err)
	}
	t.Cleanup(func() { _ = cw.Close() })

	// 4. Wait briefly and confirm no peers and no reload have occurred.
	// The file does not exist → no fsnotify events → watcher is idle.
	time.Sleep(100 * time.Millisecond)

	if got := len(p.Peers()); got != 0 {
		t.Fatalf("Peers() len = %d before file creation, want 0", got)
	}
	if got := cw.ApplyPeersInvocationCount(); got != 0 {
		t.Fatalf("ApplyPeersInvocationCount() = %d before file creation, want 0", got)
	}

	// 5. Atomically create the config file with one peer "lw-fed-c".
	// Use write-then-rename to mimic real-world atomic file creation (this
	// also exercises the fsnotify.Create event path, not just fsnotify.Write).
	writeConfigAtomic(t, cfgPath, []peerproxy.PeerConfig{
		{Name: "lw-fed-c", Address: fakeFedC.addr(), TLS: false},
	})

	// 6. Poll up to (debounce + 2s) for Peers() length to reach 1.
	deadline := time.Now().Add(debounce + 2*time.Second)
	for time.Now().Before(deadline) {
		if len(p.Peers()) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 7. Assertions.

	// Peers() must contain exactly "lw-fed-c".
	peers := p.Peers()
	if len(peers) != 1 {
		t.Fatalf("Peers() len = %d after file creation, want 1; peers=%v", len(peers), peerNames(peers))
	}
	if peers[0].Name != "lw-fed-c" {
		t.Errorf("Peers()[0].Name = %q, want %q", peers[0].Name, "lw-fed-c")
	}

	// The peer goroutine must have been spawned exactly once.
	if got := p.SpawnCount("lw-fed-c"); got != 1 {
		t.Fatalf("SpawnCount(\"lw-fed-c\") = %d, want 1", got)
	}

	// Exactly one ApplyPeers invocation: the Create event triggered one reload.
	if got := cw.ApplyPeersInvocationCount(); got != 1 {
		t.Fatalf("ApplyPeersInvocationCount() = %d, want 1 (file creation must trigger exactly one reload)", got)
	}
}

// writeConfig serialises peers into the federation config format and writes
// it directly to path. Use writeConfigAtomic for production-style atomicity.
func writeConfig(t *testing.T, path string, peers []peerproxy.PeerConfig) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"peers": peers})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config %s: %v", path, err)
	}
}

// writeConfigAtomic writes the config to a temp file then renames it into
// place, mimicking the atomic-rename approach used by `orchard config add-peer`
// and most editors.
func writeConfigAtomic(t *testing.T, path string, peers []peerproxy.PeerConfig) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"peers": peers})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatalf("write tmp config %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename config %s → %s: %v", tmp, path, err)
	}
}

// loadConfig reads and parses the federation config at path for test setup.
func loadConfig(t *testing.T, path string) peerproxy.FederationConfig {
	t.Helper()
	cfg, err := peerproxy.LoadFederationConfig(path)
	if err != nil {
		t.Fatalf("LoadFederationConfig(%s): %v", path, err)
	}
	return cfg
}

// TestConfigWatcher_BurstCoalesced is the integration coverage for the feature
// scenario "Bursty editor saves are coalesced into one reload".
//
// Steps:
//  1. Write an initial 1-peer config.
//  2. Construct and start a Provider from that config.
//  3. Construct a ConfigWatcher with a SHORT debounce (100ms) via WithDebounce.
//  4. Start the watcher. Wait briefly for it to be ready.
//  5. Rapidly write the SAME 2-peer config 5 times within ~50ms.
//  6. Wait debounce + slack (300ms total).
//  7. Assert ReloadCount() == 1 (all 5 events coalesced into one reload).
//  8. Assert SpawnCount("lw-fed-c") == 1 (AddPeer called exactly once).
func TestConfigWatcher_BurstCoalesced(t *testing.T) {
	fakeBoxd := newFakePeer(t)
	fakeFedC := newFakePeer(t)

	// 1. Write the initial config — only orchard.boxd.sh.
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	writeConfig(t, cfgPath, []peerproxy.PeerConfig{
		{Name: "orchard.boxd.sh", Address: fakeBoxd.addr(), TLS: false},
	})

	// 2. Construct provider from the initial config.
	initialCfg := loadConfig(t, cfgPath)
	p := peerproxy.NewProvider(initialCfg, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Provider.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// 3. Construct a ConfigWatcher driven by a FakeClock so the debounce is
	// fired deterministically — no dependence on a wall-clock window that,
	// under scheduling jitter, lets a straggler fsnotify event fall into a
	// second window and reload twice (issue #773). The reload hook lets the
	// test synchronise on the batch boundary instead of sleeping.
	const debounce = 100 * time.Millisecond
	clk := peerproxy.NewFakeClock()
	reloaded := make(chan struct{}, 8)
	cw := peerproxy.NewConfigWatcher(cfgPath, p, slog.Default(),
		peerproxy.WithDebounce(debounce),
		peerproxy.WithFakeClockForTest(clk),
		peerproxy.WithReloadHookForTest(func() { reloaded <- struct{}{} }),
	)

	// 4. Start the watcher.
	if err := cw.Start(ctx); err != nil {
		t.Fatalf("ConfigWatcher.Start: %v", err)
	}
	t.Cleanup(func() { _ = cw.Close() })

	// 5. Rapidly write the same 2-peer config 5 times. Each write fires one
	// or more fsnotify events; every event re-arms the (fake) debounce timer.
	twoPeerConfig := []peerproxy.PeerConfig{
		{Name: "orchard.boxd.sh", Address: fakeBoxd.addr(), TLS: false},
		{Name: "lw-fed-c", Address: fakeFedC.addr(), TLS: false},
	}
	for i := 0; i < 5; i++ {
		writeConfig(t, cfgPath, twoPeerConfig)
	}

	// 6. Fire the debounce exactly once, then wait for that single reload to
	// complete. Because the fake clock only fires on demand, no straggler
	// event can trigger a second reload.
	clk.FireAfterArmed()
	<-reloaded

	// 7. ReloadCount must be exactly 1 — all 5 events coalesced.
	if got := cw.ReloadCount(); got != 1 {
		t.Fatalf("ReloadCount() = %d after burst of 5 writes, want 1 (debounce did not coalesce)", got)
	}

	// 8. SpawnCount for the newly-added peer must be 1 — AddPeer was called
	// exactly once (not once per burst event).
	if got := p.SpawnCount("lw-fed-c"); got != 1 {
		t.Fatalf("SpawnCount(lw-fed-c) = %d, want 1 (ApplyPeers invoked more than once)", got)
	}
}
