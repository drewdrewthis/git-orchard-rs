package resolvers

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/loaders"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
	hostprovider "github.com/drewdrewthis/orchardist/internal/server/providers/host"
	psprovider "github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	tmuxprovider "github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// loadHostByID resolves a Host through the request-scoped DataLoader
// when one is wired, falling back to a direct provider call otherwise.
// Returns a stub Host{ID:hostID} for foreign host ids so the schema's
// non-null Node contract still holds.
func loadHostByID(ctx context.Context, r *Resolver, hostID string) (*graphql1.Host, error) {
	if l := loaders.FromContext(ctx); l != nil {
		thunk := l.Host.Load(ctx, hostID)
		return thunk()
	}
	if r.HostProvider == nil {
		return &graphql1.Host{ID: hostID}, nil
	}
	h, _, err := r.HostProvider.Get(ctx, hostprovider.HostID(hostID))
	if err != nil {
		return &graphql1.Host{ID: hostID}, nil
	}
	return h, nil
}

// loadProcessByPid resolves a Process by (host, pid), batched.
func loadProcessByPid(ctx context.Context, r *Resolver, hostID string, pid int) (*graphql1.Process, error) {
	if l := loaders.FromContext(ctx); l != nil {
		thunk := l.Process.Load(ctx, loaders.ProcessKey{HostID: hostID, Pid: pid})
		return thunk()
	}
	if r.PS == nil {
		return nil, nil
	}
	results := loaders.NewLoaders(r.LoaderBundle())
	thunk := results.Process.Load(ctx, loaders.ProcessKey{HostID: hostID, Pid: pid})
	return thunk()
}

// projectProcessFromCache mirrors the projection in schema.resolvers.go
// for cases where the resolver loads a Process from the store directly.
// Lives here so node.resolvers.go and subscription.resolvers.go can
// share one implementation without depending on schema.resolvers.go's
// helpers (which gqlgen routinely shuffles).
func projectProcessFromCache(p *psprovider.Process, hostID string) *graphql1.Process {
	out := &graphql1.Process{
		ID:         p.ID.String(),
		Host:       &graphql1.Host{ID: hostID},
		Pid:        int64(p.ID.PID),
		Ppid:       int64(p.PPID),
		Command:    p.Command,
		StartedAt:  p.StartedRaw,
		CPUPercent: p.CPUPercent,
		MemBytes:   p.MemBytes,
	}
	if !p.StartedAt.IsZero() {
		out.StartedAt = p.StartedAt.Format(time.RFC3339)
	}
	if p.TTY != "" {
		tty := p.TTY
		out.Tty = &tty
	}
	return out
}

// projectTmuxSession mirrors `projectSession` in schema.resolvers.go so
// node.resolvers.go can build a TmuxSession value without importing the
// schema-level helper.
func projectTmuxSession(s tmuxprovider.Session) *graphql1.TmuxSession {
	return &graphql1.TmuxSession{
		ID:   "TmuxSession:" + string(s.Key.Host) + ":" + s.Key.Name,
		Name: s.Key.Name,
	}
}

// projectTmuxPane mirrors `projectPane` in schema.resolvers.go.
func projectTmuxPane(p tmuxprovider.Pane) *graphql1.TmuxPane {
	return &graphql1.TmuxPane{
		ID:     "TmuxPane:" + string(p.Key.Host) + ":" + p.Key.PaneID,
		PaneID: p.Key.PaneID,
	}
}

// projectWindowAtomic mirrors `projectWindow` in schema.resolvers.go so
// node.resolvers.go can build a TmuxWindow value without depending on
// the schema-level helper (gqlgen routinely shuffles those).
func projectWindowAtomic(w tmuxprovider.Window) *graphql1.TmuxWindow {
	return &graphql1.TmuxWindow{
		ID:    "TmuxWindow:" + string(w.Key.Host) + ":" + w.Key.Session + ":" + strconv.Itoa(w.Key.Index),
		Index: int64(w.Key.Index),
	}
}

// projectClientAtomic mirrors `projectClient` in schema.resolvers.go.
func projectClientAtomic(c tmuxprovider.Client) *graphql1.TmuxClient {
	return &graphql1.TmuxClient{
		ID: "TmuxClient:" + string(c.Key.Host) + ":" + c.Key.ClientName,
	}
}

// findTmuxWindow returns the window matching (host, session, index), or
// ok=false when no match.
func findTmuxWindow(p *tmuxprovider.Provider, host, session string, index int) (tmuxprovider.Window, bool) {
	if p == nil {
		return tmuxprovider.Window{}, false
	}
	return p.WindowByKey(host, session, index)
}

// findTmuxClient returns the client matching (host, name), or ok=false
// when no match.
func findTmuxClient(p *tmuxprovider.Provider, host, name string) (tmuxprovider.Client, bool) {
	if p == nil {
		return tmuxprovider.Client{}, false
	}
	return p.ClientByName(host, name)
}

// findTmuxSession returns the session matching (host, name), or ok=false
// when no match. (Host, name) is the session store's own key, so this is a
// single map lookup — it used to clone the session map and scan it.
func findTmuxSession(p *tmuxprovider.Provider, host, name string) (tmuxprovider.Session, bool) {
	if p == nil {
		return tmuxprovider.Session{}, false
	}
	return p.SessionByName(host, name)
}

// findTmuxPane returns the pane matching (host, paneID), or ok=false when
// no match. (Host, paneID) is the pane store's own key, so this is a single
// map lookup — it used to clone the pane map and scan it.
func findTmuxPane(p *tmuxprovider.Provider, host, paneID string) (tmuxprovider.Pane, bool) {
	if p == nil {
		return tmuxprovider.Pane{}, false
	}
	return p.PaneByID(host, paneID)
}

// _ keeps `fmt` referenced even when no error formatting is currently
// performed in this file; subscribe paths in sibling files use it.
var _ = fmt.Errorf
