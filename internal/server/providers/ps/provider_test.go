package ps

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestProvider_StartHydratesCache spins up the provider against the real
// `ps` binary, asserts Start returns nil, and confirms the cache is
// non-empty (every machine has at least the test process and init).
func TestProvider_StartHydratesCache(t *testing.T) {
	p := New(NewAdapter("local").WithPollInterval(200*time.Millisecond), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	keys, err := p.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) == 0 {
		t.Fatal("cache empty after Start; expected at least the test process")
	}

	self := ProcessID{Host: "local", PID: os.Getpid()}
	v, _, err := p.Get(ctx, self)
	if err != nil {
		t.Fatalf("Get(self): %v", err)
	}
	if v.ID != self {
		t.Errorf("Get(self).ID = %v, want %v", v.ID, self)
	}
}

// TestProvider_ListSnapshotIncludesSelf is a smoke test for the List
// helper used by the resolver layer.
func TestProvider_ListSnapshotIncludesSelf(t *testing.T) {
	p := New(NewAdapter("local").WithPollInterval(time.Hour), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	found := false
	for _, proc := range p.List() {
		if proc.ID.PID == os.Getpid() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("List did not include self pid %d", os.Getpid())
	}
}

// TestProvider_LoadCwdResolvesSelf exercises the slow-path cwd loader
// against the test process, proving the batch loader plumbs through to
// the adapter and back.
func TestProvider_LoadCwdResolvesSelf(t *testing.T) {
	p := New(NewAdapter("local").WithPollInterval(time.Hour), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cwd, err := p.LoadCwd(ctx, os.Getpid())
	if err != nil {
		t.Fatalf("LoadCwd: %v", err)
	}
	want, _ := os.Getwd()
	if !sameDir(t, cwd, want) {
		t.Errorf("LoadCwd(self) = %q, want %q", cwd, want)
	}
}

// TestProvider_LoadArgsResolvesSelf exercises the slow-path args loader.
func TestProvider_LoadArgsResolvesSelf(t *testing.T) {
	p := New(NewAdapter("local").WithPollInterval(time.Hour), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got, err := p.LoadArgs(ctx, []int{os.Getpid()})
	if err != nil {
		t.Fatalf("LoadArgs: %v", err)
	}
	argv, ok := got[os.Getpid()]
	if !ok || len(argv) == 0 {
		t.Fatalf("argv missing for self pid %d", os.Getpid())
	}
	// `go test` invocations always include the test binary's argv0;
	// it should at least mention "test" or end with .test
	if !strings.Contains(strings.Join(argv, " "), "test") {
		t.Errorf("argv unexpectedly missing 'test' marker: %v", argv)
	}
}

// TestProvider_SubscribeReceivesInvalidations forces a Refresh after
// adding the test process and confirms the subscriber sees the keys.
func TestProvider_SubscribeReceivesInvalidations(t *testing.T) {
	p := New(NewAdapter("local").WithPollInterval(time.Hour), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	ch := p.Subscribe(subCtx)

	// Forcing a Refresh should emit zero invalidations (cache matches),
	// so we mutate the store via a manual delete to trigger an "add"
	// event on the next Refresh. Provider.store is unexported; reach
	// for the public seam by killing the prior cache via ReplaceAll.
	p.store.ReplaceAll(map[ProcessID]Process{}, "test", processEqualsHotPath)

	if err := p.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	deadline := time.After(2 * time.Second)
	gotAny := false
	for {
		select {
		case <-ch:
			gotAny = true
			if gotAny {
				return
			}
		case <-deadline:
			if !gotAny {
				t.Fatal("subscriber received no invalidation events after Refresh")
			}
			return
		}
	}
}
