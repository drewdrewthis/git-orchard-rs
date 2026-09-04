// session_loaders_test.go — DataLoader batching tests for the ADR-022
// SessionByPid axis: per-key resolution, missing key → nil (not error), and
// batch coalescing.
package loaders_test

import (
	"context"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/server/loaders"
	claudesessions "github.com/drewdrewthis/orchardist/internal/server/providers/claudesessions"
)

// fakeSessionRegistry is an in-memory ClaudeSessionByPid (a fake, not a mock —
// we own the boundary) keyed by pid.
type fakeSessionRegistry map[int]claudesessions.Session

func (f fakeSessionRegistry) SessionByPid(pid int) (claudesessions.Session, bool) {
	s, ok := f[pid]
	return s, ok
}

// TestSessionByPid_NilProvider asserts the loader returns nil gracefully when
// no registry provider is wired.
func TestSessionByPid_NilProvider(t *testing.T) {
	l := loaders.NewLoaders(&loaders.ProvidersBundle{}) // ClaudeSessions nil
	got, err := l.SessionByPid.Load(context.Background(), loaders.SessionPidKey{Host: "local", Pid: 8164})()
	if err != nil {
		t.Fatalf("SessionByPid.Load (nil provider): unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("SessionByPid.Load (nil provider): want nil, got %v", got)
	}
}

// TestSessionByPid_PerKeyResults asserts each key resolves to its own registry
// entry, and a missing pid yields nil with no error.
func TestSessionByPid_PerKeyResults(t *testing.T) {
	reg := fakeSessionRegistry{
		8164: {Pid: 8164, SessionUUID: "uuid-a", Cwd: "/w/a"},
		8165: {Pid: 8165, SessionUUID: "uuid-b", Cwd: "/w/b"},
	}
	l := loaders.NewLoaders(&loaders.ProvidersBundle{ClaudeSessions: reg})
	ctx := context.Background()

	a, errA := l.SessionByPid.Load(ctx, loaders.SessionPidKey{Host: "local", Pid: 8164})()
	b, errB := l.SessionByPid.Load(ctx, loaders.SessionPidKey{Host: "local", Pid: 8165})()
	miss, errM := l.SessionByPid.Load(ctx, loaders.SessionPidKey{Host: "local", Pid: 9999})()

	if errA != nil || errB != nil || errM != nil {
		t.Fatalf("unexpected errors: %v %v %v", errA, errB, errM)
	}
	if a == nil || a.SessionUUID != "uuid-a" {
		t.Errorf("pid 8164: got %v, want uuid-a", a)
	}
	if b == nil || b.SessionUUID != "uuid-b" {
		t.Errorf("pid 8165: got %v, want uuid-b", b)
	}
	if miss != nil {
		t.Errorf("missing pid: want nil, got %v", miss)
	}
}

// TestSessionByPid_BatchCount asserts N concurrent same-key loads collapse into
// exactly one batch invocation.
func TestSessionByPid_BatchCount(t *testing.T) {
	l := loaders.NewLoaders(&loaders.ProvidersBundle{})
	ctx := context.Background()
	const N = 7
	key := loaders.SessionPidKey{Host: "local", Pid: 8164}

	thunks := make([]func() (interface{}, error), 0, N)
	for i := 0; i < N; i++ {
		thunk := l.SessionByPid.Load(ctx, key)
		thunks = append(thunks, func() (interface{}, error) { return thunk() })
	}
	for i, thunk := range thunks {
		if _, err := thunk(); err != nil {
			t.Fatalf("thunk %d: %v", i, err)
		}
	}

	if got := l.SessionByPidBatchCount(); got != 1 {
		t.Errorf("SessionByPidBatchCount = %d, want 1 (%d loads should batch)", got, N)
	}
}
