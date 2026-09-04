// Adapter wraps the tmux CLI. Per ADR-011 §3 it is stateless: cache,
// watcher, and invalidation live in the surrounding Provider.
//
// All field separation in -F format strings uses fieldSep (a TAB) — see
// fieldsep.go, which owns the wire-format contract and the rationale.

package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CommandRunner is the test seam — production wires execRunner.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

// Run executes name with args and returns combined stdout. Errors are wrapped
// with enough context to diagnose failures in production logs.
//
// Issue #464: systemd-oomd kills the daemon with SIGKILL when the tmux exec
// storm causes excessive fork pressure. Previously the error string said only
// "signal: killed", which is indistinguishable from a ctx-cancel race in
// logs. Now signal-terminated exits surface the SIGxxx name so operators can
// confirm the oomd hypothesis without attaching a debugger.
//
// CodeRabbit follow-up (PR #507): exec.CommandContext kills the child with
// SIGKILL when the context is cancelled — which would surface as "signal:
// SIGKILL" and be conflated with an external (oomd / kernel-OOM) kill. Check
// ctx.Err() first so a ctx-cancel produces a "context cancelled" error
// rather than a misleading SIGKILL diagnostic.
func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Force a UTF-8 ctype on the child so tmux's `-F` TAB separator survives
	// (issue #701 / D3). See exec_env.go for the mechanism and rationale.
	cmd.Env = utf8Env(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// If the context was cancelled or expired, exec.CommandContext will
		// have killed the child with SIGKILL. Surface ctx.Err() (DeadlineExceeded
		// / Canceled) so logs distinguish "we killed it" from "something else
		// killed it".
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.Bytes(), fmt.Errorf("%s %v: %w (stderr: %q)", name, args, ctxErr, strings.TrimSpace(stderr.String()))
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Inspect WaitStatus to distinguish signal-terminated from
			// non-zero-exit — the two require different remediation paths.
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				sig := ws.Signal()
				return stdout.Bytes(), fmt.Errorf("%s %v: signal: %s (stderr: %q)", name, args, signalName(sig), strings.TrimSpace(stderr.String()))
			}
			return stdout.Bytes(), fmt.Errorf("%s %v: %w (stderr: %q)", name, args, err, strings.TrimSpace(stderr.String()))
		}
		return stdout.Bytes(), fmt.Errorf("%s %v: %w", name, args, err)
	}
	return stdout.Bytes(), nil
}

// signalName returns the canonical SIGxxx name for common signals.
// For unrecognised signals it falls back to syscall.Signal.String(), which
// returns e.g. "signal 31". The SIGxxx form is preferred because it matches
// what operators grep for in logs and is unambiguous across platforms.
func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGABRT:
		return "SIGABRT"
	default:
		return sig.String()
	}
}

// Adapter is the tmux CLI client. The alive-check result is cached for
// aliveTTL (default = DefaultPollInterval) so consecutive FetchAll cycles
// do not each pay for a separate liveness-probe exec (see IsAlive).
//
// The cache lives behind a pointer so the With*-builder methods can do a
// shallow value-copy of Adapter without copying the embedded mutex —
// `go vet` flags lock-by-value, and copies of an Adapter share the same
// cache state, which is the right semantics for builder chains
// (NewAdapter(...).WithSocket(...).WithAliveTTL(...) returns one logical
// adapter).
type Adapter struct {
	host     HostID
	socket   string // empty = default socket
	runner   CommandRunner
	aliveTTL time.Duration // 0 → use defaultAliveTTL
	alive    *aliveCache
	logger   *slog.Logger // nil → slog.Default() via log(); see droplog.go
}

// aliveCache memoises the result of the liveness probe (see IsAlive) for
// one TTL window. It is heap-allocated and shared across With*-derived
// Adapter copies so the mutex inside is not copied by value.
type aliveCache struct {
	mu          sync.Mutex
	lastChecked time.Time
	lastResult  bool
}

// defaultAliveTTL matches DefaultPollInterval so a single FetchAll within
// one tick never runs the liveness probe twice.
const defaultAliveTTL = DefaultPollInterval

// NewAdapter constructs an Adapter targeting the default tmux socket on
// the given host. Use WithSocket for tests / non-default sockets.
func NewAdapter(host HostID) *Adapter {
	return &Adapter{
		host:   host,
		runner: execRunner{},
		alive:  &aliveCache{},
	}
}

// WithSocket returns a copy of a addressing the given tmux socket via
// `-S <path>`. Lets E2E tests run against a sandbox tmux server.
//
// Note: the returned copy SHARES the alive-cache pointer with the receiver.
// Calling NewAdapter() returns a fresh cache; chained With* builders intentionally
// keep one logical adapter behind one cache.
func (a *Adapter) WithSocket(path string) *Adapter {
	cp := *a
	cp.socket = path
	return &cp
}

// WithRunner returns a copy of a using the given runner — for tests.
//
// Note: shares the alive-cache pointer with the receiver. See WithSocket.
func (a *Adapter) WithRunner(r CommandRunner) *Adapter {
	cp := *a
	cp.runner = r
	return &cp
}

// WithAliveTTL returns a copy of a using d as the IsAlive cache TTL.
// Useful in tests that want fine-grained control over when the cache expires.
//
// Passing 0 reverts to defaultAliveTTL (= DefaultPollInterval) — the cache
// is always on by default. There is no "disable cache" knob; pass a small
// positive duration (e.g. 1*time.Nanosecond) and rely on TTL expiry instead.
//
// Note: shares the alive-cache pointer with the receiver. See WithSocket.
func (a *Adapter) WithAliveTTL(d time.Duration) *Adapter {
	cp := *a
	cp.aliveTTL = d
	return &cp
}

// Host returns the host id baked into every key the adapter emits.
func (a *Adapter) Host() HostID { return a.host }

// Socket exposes the configured socket path (empty = tmux default).
func (a *Adapter) Socket() string { return a.socket }

// SocketBasename returns the basename of the configured tmux socket.
// When no -S flag is set, tmux uses a socket named "default" inside
// the socket directory ($TMPDIR/tmux-<uid>/), so that is returned.
func (a *Adapter) SocketBasename() string {
	if a.socket != "" {
		return filepath.Base(a.socket)
	}
	return "default"
}

// tmuxArgs returns the global flags every tmux invocation gets.
// `-L`/`-S` come first per `man 1 tmux`.
func (a *Adapter) tmuxArgs(rest ...string) []string {
	out := make([]string, 0, len(rest)+2)
	if a.socket != "" {
		out = append(out, "-S", a.socket)
	}
	out = append(out, rest...)
	return out
}

// IsAlive shells out the cheapest possible SERVER-scoped command to detect a
// live tmux server. Results are cached for aliveTTL (default =
// DefaultPollInterval) so a single FetchAll cycle — which calls IsAlive before
// listAll — never pays for two probe execs within the same tick.
//
// Issue: the probe must be SERVER-scoped, not CLIENT-scoped. `tmux info` is a
// client command: it exits non-zero with "no current client" when run from a
// process that is not attached to a tmux client — which is exactly the daemon's
// situation (a detached background process). Under that probe IsAlive returned
// false even though the server was alive and `list-panes -a` returned data,
// short-circuiting FetchAll to an empty snapshot. `tmux list-sessions` is
// server-scoped: it exits 0 (and lists sessions) whenever a server is running,
// regardless of client attachment, and exits non-zero ("no server running" /
// "error connecting") when the server is dead — correct in both directions.
//
// Stale-true window: between server death and the next TTL expiry (≤TTL),
// IsAlive may return true even though the server is dead. The next FetchAll
// then runs listAll/listClients against a dead server and gets an empty
// snapshot; the alive-probe re-fires after TTL and corrects the cache. This
// is acceptable because the worst-case staleness is one poll interval.
func (a *Adapter) IsAlive(ctx context.Context) bool {
	ttl := a.aliveTTL
	if ttl <= 0 {
		ttl = defaultAliveTTL
	}

	a.alive.mu.Lock()
	if !a.alive.lastChecked.IsZero() && time.Since(a.alive.lastChecked) < ttl {
		result := a.alive.lastResult
		a.alive.mu.Unlock()
		return result
	}
	a.alive.mu.Unlock()

	// `-F ''` suppresses the per-session output line — we only care about the
	// exit code, not the session list (FetchAll's listAll does the real read).
	_, err := a.runner.Run(ctx, "tmux", a.tmuxArgs("list-sessions", "-F", "")...)
	result := err == nil

	a.alive.mu.Lock()
	a.alive.lastChecked = time.Now()
	a.alive.lastResult = result
	a.alive.mu.Unlock()

	return result
}

// FetchAll fetches a full Snapshot in at most 3 tmux execs per cycle:
//
//  1. `tmux list-sessions` — via IsAlive (cached for aliveTTL; 0 execs when
//     warm). Server-scoped liveness probe; see IsAlive for why `tmux info`
//     (client-scoped) is wrong for a detached daemon.
//  2. `tmux list-panes -a -F …` — via listAll, which synthesises session,
//     window, and pane maps from a single output stream.
//  3. `tmux list-clients -F …` — via listClients, which populates the Clients
//     map so the client subgraph (tmuxServer.clients, tmuxSession.activeAttached,
//     tmuxPane.attachedClients, tmuxWindow.watchingClients, and the
//     subscribeTmuxClientChanged subscription) returns live data.
//
// Issue #464: the original 6-exec path (info + list-sessions + list-windows +
// list-panes + list-clients + display-message) caused fork-storm pressure on
// Linux/systemd-user, tripping oomd into SIGKILLing list-windows.
//
// Design decisions made to reach ≤3 execs:
//
//   - list-sessions, list-windows, and display-message are eliminated.
//     list-panes -a already implies the full session/window hierarchy, so a
//     single listAll call replaces three separate execs. display-message
//     (serverInfo / Pid) is not called — the Pid field is best-effort per
//     ServerInfo's doc comment; resolvers surface zero as "unknown".
//
//   - list-clients cannot be folded into list-panes because clients are an
//     orthogonal tmux concept (a terminal attachment, not a pane). It is kept
//     as a separate exec (#3) to avoid breaking the client subgraph. Dropping
//     it was tried in an earlier commit but silently broke five GraphQL fields
//     (clients, attachedClients, activeAttached, watchingClients, and the
//     subscribeTmuxClientChanged subscription). 3 execs/tick is a 50 % reduction
//     from the original 6 — still well below the oomd-trip threshold.
//
// When the daemon is dead, FetchAll returns an EmptySnapshot without an error
// — callers see "no sessions" instead of an error.
//
// Stale-alive recovery: within the IsAlive TTL window, tmux can die between
// the cached IsAlive check and the actual list-* calls. listAll/listClients
// surface that as errNoServer (sentinel). FetchAll invalidates the alive cache
// and ships a snapshot with Server.Alive=false rather than the contradictory
// snapshot the previous code produced (Alive=true + empty session/window/pane
// /client maps). See CodeRabbit follow-up on PR #507.
func (a *Adapter) FetchAll(ctx context.Context) (Snapshot, error) {
	if !a.IsAlive(ctx) { // exec #1 (cached after first call within TTL)
		snap := EmptySnapshot()
		snap.Server = ServerInfo{SocketPath: a.socketOrDefault(), Alive: false}
		return snap, nil
	}

	sessions, windows, panes, err := a.listAll(ctx) // exec #2
	if err != nil {
		if errors.Is(err, errNoServer) {
			a.invalidateAlive()
			snap := EmptySnapshot()
			snap.Server = ServerInfo{SocketPath: a.socketOrDefault(), Alive: false}
			return snap, nil
		}
		return EmptySnapshot(), fmt.Errorf("list-all: %w", err)
	}

	clients, err := a.listClients(ctx) // exec #3
	if err != nil {
		if errors.Is(err, errNoServer) {
			a.invalidateAlive()
			snap := EmptySnapshot()
			snap.Server = ServerInfo{SocketPath: a.socketOrDefault(), Alive: false}
			return snap, nil
		}
		return EmptySnapshot(), fmt.Errorf("list-clients: %w", err)
	}

	return Snapshot{
		Server: ServerInfo{
			SocketPath: a.socketOrDefault(),
			Alive:      true,
			// Pid is 0 — resolved lazily on demand (see design decisions above).
		},
		Sessions: sessions,
		Windows:  windows,
		Panes:    panes,
		Clients:  clients,
	}, nil
}

// invalidateAlive resets the IsAlive cache so the next call re-probes the
// server. Used by FetchAll when listAll/listClients observe a dead server
// after IsAlive cached true — bringing the cache back in sync with reality.
func (a *Adapter) invalidateAlive() {
	a.alive.mu.Lock()
	a.alive.lastChecked = time.Time{}
	a.alive.lastResult = false
	a.alive.mu.Unlock()
}

// CapturePane runs `tmux capture-pane -p` against a single pane. start
// and end are line numbers in tmux's history coordinate (negative
// values walk backwards into history, 0 is the top of the screen, etc.
// — see `man 1 tmux` "WINDOWS AND PANES").
//
// Pass startLine=0, endLine=0 with full=true to capture the entire
// visible+history buffer (`-S -`, `-E -`).
func (a *Adapter) CapturePane(ctx context.Context, pane PaneKey, start, end int, full bool, stripAnsi bool) (string, error) {
	args := []string{"capture-pane", "-p", "-t", pane.PaneID}
	if !stripAnsi {
		// `-e` keeps escape sequences. By default capture-pane strips
		// them, which matches `stripAnsi: true` in the schema.
		args = append(args, "-e")
	}
	if full {
		args = append(args, "-S", "-", "-E", "-")
	} else {
		if start != 0 {
			args = append(args, "-S", strconv.Itoa(start))
		}
		if end != 0 {
			args = append(args, "-E", strconv.Itoa(end))
		}
	}

	out, err := a.runner.Run(ctx, "tmux", a.tmuxArgs(args...)...)
	if err != nil {
		return "", fmt.Errorf("capture-pane %s: %w", pane.PaneID, err)
	}
	return string(out), nil
}

// CapturePaneTail captures the last `lines` rows. Implemented as
// `-S -<lines>` per tmux convention (negative starts walk into history).
func (a *Adapter) CapturePaneTail(ctx context.Context, pane PaneKey, lines int, stripAnsi bool) (string, error) {
	if lines <= 0 {
		lines = 50
	}
	args := []string{"capture-pane", "-p", "-t", pane.PaneID, "-S", strconv.Itoa(-lines)}
	if !stripAnsi {
		args = append(args, "-e")
	}
	out, err := a.runner.Run(ctx, "tmux", a.tmuxArgs(args...)...)
	if err != nil {
		return "", fmt.Errorf("capture-pane tail %s: %w", pane.PaneID, err)
	}
	return string(out), nil
}

// ----------------------------------------------------------------------
// listAll — coalesced single-exec fetch (issue #464 AC1).
// ----------------------------------------------------------------------

// listAllFormat carries every field needed for session, window, and pane maps
// in a single `tmux list-panes -a` invocation. Fields are fieldSep-separated.
//
// Field order (0-based):
//
//	 0  session_name
//	 1  session_created
//	 2  session_attached
//	 3  session_activity
//	 4  session_windows
//	 5  session_window_index   (active window index within session)
//	 6  window_index
//	 7  window_name
//	 8  window_active
//	 9  window_panes
//	10  window_active_pane
//	11  pane_id
//	12  pane_title
//	13  pane_current_command
//	14  pane_pid
//	15  pane_width
//	16  pane_height
//	17  pane_dead
const listAllFormat = "" +
	"#{session_name}" + fieldSep +
	"#{session_created}" + fieldSep +
	"#{session_attached}" + fieldSep +
	"#{session_activity}" + fieldSep +
	"#{session_windows}" + fieldSep +
	"#{session_window_index}" + fieldSep +
	"#{window_index}" + fieldSep +
	"#{window_name}" + fieldSep +
	"#{window_active}" + fieldSep +
	"#{window_panes}" + fieldSep +
	"#{window_active_pane}" + fieldSep +
	"#{pane_id}" + fieldSep +
	"#{pane_title}" + fieldSep +
	"#{pane_current_command}" + fieldSep +
	"#{pane_pid}" + fieldSep +
	"#{pane_width}" + fieldSep +
	"#{pane_height}" + fieldSep +
	"#{pane_dead}"

const listAllFieldCount = 18

// errNoServer is the sentinel returned by list-* helpers when tmux reports
// "no server running" between the IsAlive cache check and the actual list call.
// FetchAll uses this to invalidate the alive cache and ship a snapshot with
// Server.Alive=false rather than the contradictory mix of Alive=true + empty
// session/window/pane/client maps that the previous code produced. See the
// CodeRabbit follow-up on PR #507 for the contradictory-snapshot scenario.
var errNoServer = errors.New("tmux: no server running")

// listAll executes a single `tmux list-panes -a -F <combined>` call and
// synthesises session, window, and pane maps from the output. Each output
// row represents one pane; session and window entries are deduplicated by
// their natural keys.
//
// Known limitation: a session or window that exists but has zero panes (a
// rare race during creation or destruction) will not appear in the output.
// tmux always creates a default pane on session creation, so in practice
// this window is at most one poll tick wide. A subsequent FetchAll will
// pick up the pane once it exists. No fallback to the old multi-call path
// is provided — that would defeat the exec-count reduction of AC1.
//
// If tmux has died between the parent's IsAlive check and this call, returns
// errNoServer (sentinel) rather than an empty result set. The caller decides
// how to surface this — FetchAll invalidates the alive cache and ships
// Server.Alive=false instead of a contradictory snapshot.
func (a *Adapter) listAll(ctx context.Context) (
	sessions map[SessionKey]Session,
	windows map[WindowKey]Window,
	panes map[PaneKey]Pane,
	err error,
) {
	out, err := a.runner.Run(ctx, "tmux", a.tmuxArgs("list-panes", "-a", "-F", listAllFormat)...)
	if err != nil {
		if strings.Contains(err.Error(), "no server running") {
			return nil, nil, nil, errNoServer
		}
		return nil, nil, nil, err
	}

	sessions = make(map[SessionKey]Session)
	windows = make(map[WindowKey]Window)
	panes = make(map[PaneKey]Pane)

	out = bytes.TrimRight(out, "\n")
	if len(out) == 0 {
		return sessions, windows, panes, nil
	}

	dropped, firstGot := 0, 0
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		fields := strings.Split(string(line), fieldSep)
		if len(fields) != listAllFieldCount {
			// A row that does not split into the expected field count is
			// almost always the D3 locale defect: a non-UTF-8 tmux client
			// sanitizes the TAB separator to `_`, collapsing the row to one
			// field. Count and warn (see warnDroppedRows) instead of the old
			// silent drop that surfaced as empty tmuxSessions (#701 AC12).
			if dropped == 0 {
				firstGot = len(fields)
			}
			dropped++
			continue
		}

		// --- session ---
		sessKey := SessionKey{Host: a.host, Name: fields[0]}
		if _, ok := sessions[sessKey]; !ok {
			sessions[sessKey] = Session{
				Key:            sessKey,
				CreatedAt:      parseUnix(fields[1]),
				Attached:       fields[2] != "" && fields[2] != "0",
				AttachedCount:  parseInt(fields[2]),
				LastActivityAt: parseUnix(fields[3]),
				WindowCount:    parseInt(fields[4]),
				CurrentWindow:  parseInt(fields[5]),
			}
		}

		// --- window ---
		winKey := WindowKey{Host: a.host, Session: fields[0], Index: parseInt(fields[6])}
		if _, ok := windows[winKey]; !ok {
			windows[winKey] = Window{
				Key:         winKey,
				Name:        fields[7],
				Active:      fields[8] == "1",
				PaneCount:   parseInt(fields[9]),
				CurrentPane: fields[10],
			}
		}

		// --- pane ---
		paneKey := PaneKey{Host: a.host, PaneID: fields[11]}
		panes[paneKey] = Pane{
			Key:     paneKey,
			WindowKey: WindowKey{
				Host:    a.host,
				Session: fields[0],
				Index:   parseInt(fields[6]),
			},
			Title:          fields[12],
			CurrentCommand: fields[13],
			CurrentPid:     parseInt(fields[14]),
			Width:          parseInt(fields[15]),
			Height:         parseInt(fields[16]),
			Dead:           fields[17] == "1",
		}
	}
	a.warnDroppedRows("list-panes -a", dropped, listAllFieldCount, firstGot)
	return sessions, windows, panes, nil
}

// In `list-clients -F` context, the per-client format vars
// `#{client_window_index}` and `#{client_active_pane}` are silently
// empty in current tmux releases. The format engine instead resolves
// the *unprefixed* `#{window_index}` and `#{pane_id}` against the
// client's currently-attached pane — which is exactly what we want.
// Verified empirically against tmux 3.5+ on macOS: a client attached
// to a different window/pane shows that window/pane via the
// unprefixed vars, while the client_* vars stay empty.
const clientFormat = "" +
	"#{client_name}" + fieldSep +
	"#{client_session}" + fieldSep +
	"#{client_tty}" + fieldSep +
	"#{client_termname}" + fieldSep +
	"#{client_created}" + fieldSep +
	"#{client_activity}" + fieldSep +
	"#{client_readonly}" + fieldSep +
	"#{window_index}" + fieldSep +
	"#{pane_id}"

// clientFieldCount is the field count of a clientFormat row.
const clientFieldCount = 9

func (a *Adapter) listClients(ctx context.Context) (map[ClientKey]Client, error) {
	out, err := a.runner.Run(ctx, "tmux", a.tmuxArgs("list-clients", "-F", clientFormat)...)
	if err != nil {
		if strings.Contains(err.Error(), "no server running") {
			return nil, errNoServer
		}
		return nil, err
	}
	out = bytes.TrimRight(out, "\n")
	if len(out) == 0 {
		return map[ClientKey]Client{}, nil
	}
	clients := make(map[ClientKey]Client)
	dropped, firstGot := 0, 0
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		fields := strings.Split(string(line), fieldSep)
		if len(fields) != clientFieldCount {
			// Same D3 field-separator hazard as listAll — count and warn
			// rather than dropping silently (#701 AC12).
			if dropped == 0 {
				firstGot = len(fields)
			}
			dropped++
			continue
		}
		// tmux historically used the tty path as `client_name`; newer
		// tmux releases honour the client name passed to `attach -t`.
		// Either way the value is unique within a server.
		name := fields[0]
		if name == "" {
			name = fields[2]
		}
		key := ClientKey{Host: a.host, ClientName: name}
		clients[key] = Client{
			Key:            key,
			Session:        fields[1],
			TTY:            fields[2],
			TermName:       fields[3],
			AttachedAt:     parseUnix(fields[4]),
			LastActivityAt: parseUnix(fields[5]),
			Readonly:       fields[6] == "1",
			CurrentWindow:  parseIntOrNeg1(fields[7]),
			CurrentPane:    fields[8],
		}
	}
	a.warnDroppedRows("list-clients", dropped, clientFieldCount, firstGot)
	return clients, nil
}

// socketOrDefault returns the configured socket path, falling back to a
// stable string for the daemon's default. tmux's actual default lives
// in $TMPDIR (e.g. /private/tmp/tmux-501/default on macOS); for callers
// that need a stable identifier the symbolic name is fine.
func (a *Adapter) socketOrDefault() string {
	if a.socket != "" {
		return a.socket
	}
	return "default"
}

// ----------------------------------------------------------------------
// Helpers. Tiny, intentionally — keep parsing locality high.
// ----------------------------------------------------------------------

func parseInt(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func parseIntOrNeg1(s string) int {
	if s == "" {
		return -1
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

// parseUnix accepts tmux's epoch-second timestamps (e.g. session_created).
// Empty / unparseable values yield the zero time, which downstream maps
// to a null GraphQL value.
func parseUnix(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}
