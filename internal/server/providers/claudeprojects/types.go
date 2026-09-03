// Package claudeprojects implements the Conversation provider — read
// access to the JSONL transcripts that the Claude Code CLI writes
// under `~/.claude/projects/<project-slug>/<session-uuid>.jsonl`.
//
// Each conversation has exactly one JSONL on disk. The provider derives
// every Conversation field (firstSeenAt, lastSeenAt, messageCount,
// open, recap) from the file itself — no sidecar metadata, no embedded
// process state. `recap` is the text of the most recent `/recap` slash
// command invocation, extracted by the daemon directly. No plugin
// required.
//
// Per ADR-011 §5.1 the v1 contract is metadata + `open` + `recap`;
// `status`, `focus`, `awaitingDrew`, `artifacts` are all out of scope.
//
// Why this provider is non-trivial:
//
//   - **JSONL size.** A long-running Claude session can write tens of
//     megabytes. The adapter MUST avoid loading whole files into RAM.
//     We tail the last record, head the first, and stream a counted
//     scan for messageCount. Each pass touches only a small window.
//   - **fsnotify recursion.** The projects root contains one
//     subdirectory per project, and each subdirectory contains the
//     JSONLs we care about. fsnotify on macOS does not recurse, so we
//     watch the root for new project subdirs and add subdir watchers
//     on the fly. See watcher.go.
package claudeprojects

import (
	"path/filepath"
	"strings"
	"time"
)

// ConversationID uniquely identifies a Conversation across hosts. v1
// only enumerates the local host, but the host-qualified shape lets
// federated daemons (WS-F) merge tables without rekeying.
//
// HostID is a free-form string in v1 — typically the local machine's
// id (e.g. /etc/machine-id on Linux, IOPlatformUUID on macOS). Tests
// use a stable fixture (e.g. "test-host").
type ConversationID struct {
	HostID      string
	SessionUUID string
}

// Conversation is the in-memory representation of a Claude Code
// transcript. It is what the provider's cache holds and what the
// resolver maps onto graphql.Conversation.
//
// FirstSeenAt and LastSeenAt are pointers because an empty JSONL (the
// file was just created with no records yet) has neither timestamp.
// Cwd is a pointer because older Claude Code transcripts omit `cwd` on
// every record, in which case we have nothing to surface.
//
// MessageCount is the number of newline-terminated JSON records — i.e.
// every `type: user`, `type: assistant`, `type: summary`, etc. We do
// not filter to a specific role; the count is a coarse "how many
// turns" signal.
type Conversation struct {
	ID           ConversationID
	Path         string
	Cwd          *string
	FirstSeenAt  *time.Time
	LastSeenAt   *time.Time
	MessageCount int64
	// CustomTitle is the user-set title from the JSONL `type: "custom-title"` record. Nil when not yet recorded.
	CustomTitle *string
	// AgentName is the sub-agent name from the JSONL `type: "agent-name"` record. Nil when not yet recorded.
	AgentName *string
	// Recap is the latest recap in this session — either a `/recap`
	// slash-command output or an autonomous `away_summary` system record,
	// whichever is later in the JSONL. Derived directly from the JSONL —
	// no plugin involvement. Nil when the session has produced neither.
	Recap *string
	// RecapSource names which record kind Recap came from. Nil exactly
	// when Recap is nil.
	RecapSource *RecapSource
}

// GraphQLID returns the stable orchard id for a Conversation node. The
// shape mirrors other Node implementers (e.g. "Host:<machineId>"),
// keeping the Query.node(id) lookup uniform across types.
func (id ConversationID) GraphQLID() string {
	return "Conversation:" + id.SessionUUID
}

// sessionUUIDFromPath returns the JSONL filename minus its `.jsonl`
// suffix. Claude Code names files by their sessionId, so the trimmed
// filename is the canonical session UUID.
func sessionUUIDFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, ".jsonl")
}
