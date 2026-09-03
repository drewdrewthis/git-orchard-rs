package claudeprojects

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// FSAdapter implements adapter.Adapter[ConversationID, Conversation]
// by walking the projects root and reading metadata from every JSONL
// it finds.
//
// Layout assumed:
//
//	<root>/
//	  <project-slug-1>/
//	    <session-uuid>.jsonl
//	    <session-uuid>.jsonl
//	  <project-slug-2>/
//	    <session-uuid>.jsonl
//
// Subdirectories two-deep are not enumerated (Claude Code does not
// nest transcripts). The watcher (watcher.go) handles new project
// subdirs appearing at runtime.
//
// hostID is constant for the lifetime of one daemon process; tests
// inject a fixture id so the GraphQL responses are deterministic.
type FSAdapter struct {
	root   string
	hostID string
	logger *slog.Logger

	// Watcher state — owned by Watch / Close in watcher.go. Held on
	// the struct so Close (which the Provider calls during teardown)
	// can shut the goroutine down.
	watcherMu   sync.Mutex
	watcher     *fsnotify.Watcher
	watcherDone chan struct{}
}

// NewFSAdapter constructs an adapter rooted at root. The root is not
// required to exist at construction time — Fetch / FetchAll degrade to
// empty results when the directory is missing, so a daemon that boots
// before the user has installed Claude Code does not fail.
func NewFSAdapter(root, hostID string, logger *slog.Logger) *FSAdapter {
	if logger == nil {
		logger = slog.Default()
	}
	return &FSAdapter{
		root:   root,
		hostID: hostID,
		logger: logger,
	}
}

// Root returns the configured projects root. Useful for diagnostics
// and tests; the watcher and provider both read it.
func (a *FSAdapter) Root() string { return a.root }

// HostID returns the host id stamped onto every ConversationID this
// adapter produces.
func (a *FSAdapter) HostID() string { return a.hostID }

// Fetch returns one Conversation by id. The session UUID lookup is
// O(N) over all .jsonl filenames since we don't keep a path index —
// FetchAll is the dominant code path and Fetch is only the cache-miss
// fallback. If the conversation cannot be located, returns an error
// with %w so callers can detect not-found via errors.Is(..., fs.ErrNotExist).
func (a *FSAdapter) Fetch(ctx context.Context, id ConversationID) (Conversation, error) {
	if err := ctx.Err(); err != nil {
		return Conversation{}, err
	}
	if id.HostID != a.hostID {
		return Conversation{}, fmt.Errorf("unknown host id %q (this adapter serves %q)", id.HostID, a.hostID)
	}
	all, err := a.FetchAll(ctx)
	if err != nil {
		return Conversation{}, err
	}
	c, ok := all[id]
	if !ok {
		return Conversation{}, fmt.Errorf("conversation %q: %w", id.SessionUUID, fs.ErrNotExist)
	}
	return c, nil
}

// FetchAll enumerates every JSONL under the projects root and returns
// the cheap-to-compute Conversation summary for each. A missing root
// returns an empty map (not an error) so the daemon can boot before
// Claude Code has been used on this host.
//
// Errors reading individual files are logged but do not fail the
// whole walk — one corrupt transcript should not blank the GraphQL
// response. The caller (the provider) is responsible for any
// error-aware reporting.
func (a *FSAdapter) FetchAll(ctx context.Context) (map[ConversationID]Conversation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[ConversationID]Conversation)

	root, err := os.Stat(a.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("stat projects root %s: %w", a.root, err)
	}
	if !root.IsDir() {
		return nil, fmt.Errorf("projects root is not a directory: %s", a.root)
	}

	projectDirs, err := os.ReadDir(a.root)
	if err != nil {
		return nil, fmt.Errorf("read projects root %s: %w", a.root, err)
	}

	for _, dir := range projectDirs {
		if !dir.IsDir() {
			continue
		}
		projectPath := filepath.Join(a.root, dir.Name())
		entries, err := os.ReadDir(projectPath)
		if err != nil {
			a.logger.Warn("claudeprojects: skipping unreadable project dir",
				"dir", projectPath, "err", err)
			continue
		}
		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			if !strings.HasSuffix(ent.Name(), ".jsonl") {
				continue
			}
			full := filepath.Join(projectPath, ent.Name())
			c, err := a.fromFile(full)
			if err != nil {
				a.logger.Warn("claudeprojects: skipping unparseable transcript",
					"file", full, "err", err)
				continue
			}
			out[c.ID] = c
		}
	}
	return out, nil
}

// FetchOne reads a single transcript file and returns its summary.
// Surfaced for the watcher hot-path: a fsnotify Write event for
// /root/proj/abc.jsonl re-reads only that file rather than walking
// the whole tree again.
func (a *FSAdapter) FetchOne(ctx context.Context, path string) (Conversation, error) {
	if err := ctx.Err(); err != nil {
		return Conversation{}, err
	}
	return a.fromFile(path)
}

// fromFile builds a Conversation from one transcript path. Cheap by
// design — the heavy lifting lives in readJSONLMeta, which never
// loads the whole file into memory.
func (a *FSAdapter) fromFile(path string) (Conversation, error) {
	meta, err := readJSONLMeta(path)
	if err != nil {
		return Conversation{}, err
	}
	id := ConversationID{
		HostID:      a.hostID,
		SessionUUID: sessionUUIDFromPath(path),
	}
	return Conversation{
		ID:           id,
		Path:         path,
		Cwd:          meta.Cwd,
		FirstSeenAt:  meta.FirstSeenAt,
		LastSeenAt:   meta.LastSeenAt,
		MessageCount: meta.MessageCount,
		CustomTitle:  meta.CustomTitle,
		AgentName:    meta.AgentName,
		Recap:        meta.Recap,
		RecapSource:  meta.RecapSource,
	}, nil
}
