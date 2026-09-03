package ps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner is a test double for CommandRunner. It records every Run
// invocation and returns canned (output, err) pairs indexed by call order.
// If responses is exhausted it returns ("", nil) for any subsequent call.
type fakeRunner struct {
	mu        sync.Mutex
	calls     []fakeCall
	responses []fakeResponse
}

// fakeCall records a single Run invocation.
type fakeCall struct {
	Name string
	Args []string
}

// fakeResponse is the canned reply for one Run call.
type fakeResponse struct {
	Out []byte
	Err error
}

// Run records the call and returns the next canned response.
func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	argsCopy := make([]string, len(args))
	copy(argsCopy, args)
	f.calls = append(f.calls, fakeCall{Name: name, Args: argsCopy})
	if len(f.responses) == 0 {
		return nil, nil
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return resp.Out, resp.Err
}

// Calls returns a snapshot of recorded invocations.
func (f *fakeRunner) Calls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// buildLsofBlock builds the -F output block for one pid and path:
//
//	p<pid>
//	fcwd
//	n<path>
func buildLsofBlock(pid int, path string) string {
	return fmt.Sprintf("p%d\nfcwd\nn%s\n", pid, path)
}

// TestAdapter_FetchAll_RealPs invokes the actual `ps` binary on the host
// and asserts the test process itself (os.Getpid) is in the result.
// Worker-standards §3: provider tests run against real backends.
func TestAdapter_FetchAll_RealPs(t *testing.T) {
	a := NewAdapter("local")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	all, err := a.FetchAll(ctx)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	self := ProcessID{Host: "local", PID: os.Getpid()}
	got, ok := all[self]
	if !ok {
		t.Fatalf("test process pid %d not in ps result (size=%d)", os.Getpid(), len(all))
	}
	if got.Command == "" {
		t.Errorf("test process command basename should be non-empty, got %q", got.Command)
	}
	if got.PPID == 0 {
		t.Errorf("test process should have a non-zero parent pid, got 0")
	}
}

// TestAdapter_Watch_EmitsOnSpawn proves the watcher detects a real
// subprocess appearing in the process table and emits its key.
func TestAdapter_Watch_EmitsOnSpawn(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("ps watcher test is darwin/linux only")
	}
	a := NewAdapter("local").WithPollInterval(150 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	ch, err := a.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Drain the initial-snapshot burst before spawning so the assertion
	// is unambiguous: we want to see the spawn-pid AFTER the burst ends.
	drainBurst(t, ch, 500*time.Millisecond)

	cmd := exec.CommandContext(ctx, "sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep subprocess: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	want := ProcessID{Host: "local", PID: cmd.Process.Pid}
	deadline := time.After(4 * time.Second)
	for {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatalf("watch channel closed before pid %d appeared", want.PID)
			}
			if got == want {
				return // success
			}
		case <-deadline:
			t.Fatalf("did not observe spawn of pid %d within deadline", want.PID)
		}
	}
}

// TestAdapter_FetchCwds_RealLsof asks for the test process's cwd and
// asserts it matches os.Getwd. macOS only — Linux returns empty until
// the /proc fallback lands.
func TestAdapter_FetchCwds_RealLsof(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("lsof cwd test is darwin only (Linux uses /proc which is not yet wired)")
	}
	a := NewAdapter("local")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cwds, err := a.FetchCwds(ctx, []int{os.Getpid()})
	if err != nil {
		t.Fatalf("FetchCwds: %v", err)
	}
	got, ok := cwds[os.Getpid()]
	if !ok {
		t.Fatalf("cwd missing for self pid %d", os.Getpid())
	}
	want, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	if !sameDir(t, got, want) {
		t.Errorf("cwd = %q, want %q", got, want)
	}
}

// sameDir reports whether two paths name the same directory. The kernel
// hands back fully-resolved paths (lsof on macOS, /proc/<pid>/cwd on Linux)
// while os.Getwd may return the logical path, so the two disagree whenever
// any component of the checkout path is a symlink — e.g. a worktree under
// /tmp on macOS, where /tmp -> /private/tmp turns a correct answer into a
// string mismatch. Compare resolved forms so the assertion tests the
// adapter rather than where the test happens to be checked out.
func sameDir(t *testing.T, got, want string) bool {
	t.Helper()
	if got == want {
		return true
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		return false
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		return false
	}
	return gotResolved == wantResolved
}

// TestAdapter_FetchArgs_RealPs verifies that argv resolution finds the
// `go test` binary invocation (which always has a -test.* flag).
func TestAdapter_FetchArgs_RealPs(t *testing.T) {
	a := NewAdapter("local")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	args, err := a.FetchArgs(ctx, []int{os.Getpid()})
	if err != nil {
		t.Fatalf("FetchArgs: %v", err)
	}
	got, ok := args[os.Getpid()]
	if !ok || len(got) == 0 {
		t.Fatalf("argv missing for self pid %d", os.Getpid())
	}
}

// drainBurst consumes events that arrive within window. Used to swallow
// the initial watcher snapshot so subsequent assertions can target the
// post-snapshot deltas only.
func drainBurst(t *testing.T, ch <-chan ProcessID, window time.Duration) {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case <-ch:
			// keep draining
		case <-deadline:
			return
		}
	}
}

// TestFetchCwdsDarwin_EmptyInput verifies that fetchCwdsDarwin with an
// empty pid list returns an empty map without invoking lsof.
func TestFetchCwdsDarwin_EmptyInput(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("fetchCwdsDarwin is darwin-only")
	}
	fr := &fakeRunner{}
	a := NewAdapter("local").WithRunner(fr)
	ctx := context.Background()

	got, err := a.fetchCwdsDarwin(ctx, []int{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
	if calls := fr.Calls(); len(calls) != 0 {
		t.Errorf("expected 0 runner calls, got %d", len(calls))
	}
}

// TestFetchCwdsDarwin_SinglePid verifies that a single-pid lsof output
// block is parsed into a map with the correct cwd.
func TestFetchCwdsDarwin_SinglePid(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("fetchCwdsDarwin is darwin-only")
	}
	raw := []byte("p4242\nfcwd\nn/tmp/foo\n")
	fr := &fakeRunner{responses: []fakeResponse{{Out: raw, Err: nil}}}
	a := NewAdapter("local").WithRunner(fr)
	ctx := context.Background()

	got, err := a.fetchCwdsDarwin(ctx, []int{4242})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[4242] != "/tmp/foo" {
		t.Errorf("cwd for pid 4242 = %q, want %q", got[4242], "/tmp/foo")
	}
	if len(got) != 1 {
		t.Errorf("expected 1 entry, got %d", len(got))
	}
}

// TestFetchCwdsDarwin_MultiPid verifies that a 50-pid output block is
// fully parsed into a 50-entry map.
func TestFetchCwdsDarwin_MultiPid(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("fetchCwdsDarwin is darwin-only")
	}
	const n = 50
	pids := make([]int, n)
	want := make(map[int]string, n)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		pid := 1000 + i
		path := fmt.Sprintf("/workspace/repo%d", i)
		pids[i] = pid
		want[pid] = path
		sb.WriteString(buildLsofBlock(pid, path))
	}

	fr := &fakeRunner{responses: []fakeResponse{{Out: []byte(sb.String()), Err: nil}}}
	a := NewAdapter("local").WithRunner(fr)
	ctx := context.Background()

	got, err := a.fetchCwdsDarwin(ctx, pids)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != n {
		t.Fatalf("expected %d entries, got %d", n, len(got))
	}
	for pid, wantPath := range want {
		if got[pid] != wantPath {
			t.Errorf("cwd[%d] = %q, want %q", pid, got[pid], wantPath)
		}
	}
}

// TestFetchCwdsDarwin_SingleShellout proves that N pids produce exactly
// one lsof invocation with a comma-joined -p argument.
func TestFetchCwdsDarwin_SingleShellout(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("fetchCwdsDarwin is darwin-only")
	}
	const n = 50
	pids := make([]int, n)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		pids[i] = 2000 + i
		sb.WriteString(buildLsofBlock(2000+i, fmt.Sprintf("/dir/%d", i)))
	}

	fr := &fakeRunner{responses: []fakeResponse{{Out: []byte(sb.String()), Err: nil}}}
	a := NewAdapter("local").WithRunner(fr)
	ctx := context.Background()

	if _, err := a.fetchCwdsDarwin(ctx, pids); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := fr.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 lsof invocation, got %d", len(calls))
	}

	// The -p flag must be present and its value must be a comma-separated
	// list of all requested pids — not 50 separate invocations.
	call := calls[0]
	if call.Name != "lsof" {
		t.Errorf("expected command 'lsof', got %q", call.Name)
	}
	pFlagIdx := -1
	for i, arg := range call.Args {
		if arg == "-p" && i+1 < len(call.Args) {
			pFlagIdx = i + 1
			break
		}
	}
	if pFlagIdx == -1 {
		t.Fatalf("no -p flag found in lsof args: %v", call.Args)
	}
	pidArg := call.Args[pFlagIdx]
	parts := strings.Split(pidArg, ",")
	if len(parts) != n {
		t.Errorf("-p arg has %d parts (comma-separated), want %d; arg=%q", len(parts), n, pidArg)
	}
}

// TestFetchCwdsDarwin_PartialOutput verifies that when lsof returns data
// for only 2 of 3 requested pids, the map contains exactly those 2 entries.
func TestFetchCwdsDarwin_PartialOutput(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("fetchCwdsDarwin is darwin-only")
	}
	// lsof returns blocks for pids 100 and 200 but not 300.
	raw := []byte(buildLsofBlock(100, "/a") + buildLsofBlock(200, "/b"))
	fr := &fakeRunner{responses: []fakeResponse{{Out: raw, Err: nil}}}
	a := NewAdapter("local").WithRunner(fr)
	ctx := context.Background()

	got, err := a.fetchCwdsDarwin(ctx, []int{100, 200, 300})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(got), got)
	}
	if got[100] != "/a" {
		t.Errorf("cwd[100] = %q, want %q", got[100], "/a")
	}
	if got[200] != "/b" {
		t.Errorf("cwd[200] = %q, want %q", got[200], "/b")
	}
	if _, present := got[300]; present {
		t.Errorf("cwd[300] should be absent, got %q", got[300])
	}
}

// TestFetchCwdsDarwin_NonZeroExitWithPartialOutput verifies that when lsof
// exits non-zero but still emits partial stdout, the parser returns the
// pids that were present and does not propagate the exit error.
func TestFetchCwdsDarwin_NonZeroExitWithPartialOutput(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("fetchCwdsDarwin is darwin-only")
	}
	partial := []byte(buildLsofBlock(10, "/x") + buildLsofBlock(20, "/y"))
	stubErr := errors.New("lsof: exit status 1")
	fr := &fakeRunner{responses: []fakeResponse{{Out: partial, Err: stubErr}}}
	a := NewAdapter("local").WithRunner(fr)
	ctx := context.Background()

	got, err := a.fetchCwdsDarwin(ctx, []int{10, 20, 30})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries from partial output, got %d: %v", len(got), got)
	}
	if got[10] != "/x" {
		t.Errorf("cwd[10] = %q, want %q", got[10], "/x")
	}
	if got[20] != "/y" {
		t.Errorf("cwd[20] = %q, want %q", got[20], "/y")
	}
}
