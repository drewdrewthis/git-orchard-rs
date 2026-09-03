// Package tmux exposes the tmux-server / tmux-session / tmux-window /
// tmux-pane / tmux-client hierarchy as an orchard provider per
// ADR-011 §5.1.
//
// Layering (worker-standards §5):
//
//	resolvers/  ─►  tmux.Provider  ─►  tmux.Adapter  ─►  `tmux ...`
//
// The provider owns the cache + watcher; the adapter is stateless I/O.
// All interaction with the tmux daemon goes through the adapter — the
// provider never shells out directly.
package tmux

import "time"

// HostID identifies the host the tmux daemon runs on. v1 only ever holds
// the local machine's id; the prefix is preserved in every node id so
// federation in Workstream F can disambiguate without a schema bump.
type HostID string

// SessionKey is the cache key for a TmuxSession. Sessions are uniquely
// identified by name within a server.
type SessionKey struct {
	Host HostID
	Name string
}

// WindowKey identifies a window within a session.
type WindowKey struct {
	Host    HostID
	Session string
	Index   int
}

// PaneKey identifies a pane globally within a host. tmux assigns each
// pane a stable `%N` id that survives window/session moves.
type PaneKey struct {
	Host   HostID
	PaneID string // includes leading '%'
}

// ClientKey identifies an attached client by tty.
type ClientKey struct {
	Host       HostID
	ClientName string
}

// ServerInfo is what the adapter reports about the tmux daemon itself.
// Pid is best-effort — older tmux releases don't expose it through the
// CLI; resolvers surface zero as "unknown".
type ServerInfo struct {
	SocketPath string
	Pid        int
	Alive      bool
}

// Session mirrors the parts of `tmux list-sessions -F` we care about.
// Field meanings track tmux's documented format-variables so a reader
// can map straight back to `man 1 tmux`.
type Session struct {
	Key            SessionKey
	CreatedAt      time.Time
	Attached       bool
	AttachedCount  int
	LastActivityAt time.Time // zero when tmux returned no value
	WindowCount    int
	CurrentWindow  int // index of the focused window
}

// Window mirrors `tmux list-windows -F`.
type Window struct {
	Key         WindowKey
	Name        string
	Active      bool
	PaneCount   int
	CurrentPane string // pane id of the active pane in this window
}

// Pane mirrors `tmux list-panes -F`. The tracking fields here are
// intentionally narrow — content captures travel through a separate
// adapter call so the poll cycle stays cheap.
type Pane struct {
	Key            PaneKey
	WindowKey      WindowKey
	Title          string
	CurrentCommand string
	CurrentPid     int
	Width          int
	Height         int
	Dead           bool
	WatchingTTYs   []string // tty paths from `client_active_pane`
}

// clone returns a Pane whose reference-typed fields are independent of the
// receiver's. Cached panes are handed out by value, and a plain struct copy
// shares WatchingTTYs' backing array with the store's own entry — a caller
// could otherwise rewrite the cache by writing through it.
func (p Pane) clone() Pane {
	if p.WatchingTTYs != nil {
		p.WatchingTTYs = append([]string(nil), p.WatchingTTYs...)
	}
	return p
}

// Client mirrors `tmux list-clients -F`.
type Client struct {
	Key            ClientKey
	Session        string
	TTY            string
	Hostname       string
	TermName       string
	AttachedAt     time.Time
	LastActivityAt time.Time // zero when tmux returned no value
	Readonly       bool
	CurrentWindow  int    // index, -1 when unknown
	CurrentPane    string // pane id, "" when unknown
}

// Snapshot is the adapter's full per-tick view of a tmux server. The
// provider does a single bulk fetch each poll tick because the four
// `list-*` commands share the same socket connection cost.
type Snapshot struct {
	Server   ServerInfo
	Sessions map[SessionKey]Session
	Windows  map[WindowKey]Window
	Panes    map[PaneKey]Pane
	Clients  map[ClientKey]Client
}

// EmptySnapshot is the value the adapter returns when no tmux daemon is
// reachable. Callers can pass it through the provider's Store unchanged
// — every map is non-nil so resolvers don't crash on a cold-boot lookup.
func EmptySnapshot() Snapshot {
	return Snapshot{
		Sessions: map[SessionKey]Session{},
		Windows:  map[WindowKey]Window{},
		Panes:    map[PaneKey]Pane{},
		Clients:  map[ClientKey]Client{},
	}
}
