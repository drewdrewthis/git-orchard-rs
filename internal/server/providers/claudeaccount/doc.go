// Package claudeaccount implements the ClaudeAccount provider — orchard's
// reflection of the local `claude` CLI's auth subject and the quota
// numbers `ccusage` reports for that subject.
//
// Per ADR-011 §5.1, ClaudeAccount carries:
//
//   - Identity: (host_id, email). The email is what `claude auth status`
//     reports for the active session.
//   - Quota: used / cap / resets-at. Read from `ccusage blocks --json`,
//     so `quotaEstimated` is always true in v1 (we do not call any
//     first-party Anthropic billing API).
//   - Edges: host (back-edge), instances (resolved by the
//     B-claudeinstance workstream; v1 returns []).
//
// The provider holds:
//
//   - one ShellAdapter (raw shellouts to `claude` and `ccusage`).
//   - an in-memory cache of (AccountID -> Account) with a 60s TTL.
//   - a poll-based watcher (60s); no fsnotify because there is no
//     observable file under the user's control — the source of truth
//     is the CLI exit code + stdout.
//   - a fan-out of invalidation events for subscribers.
//
// Per ADR-011 §6 ("per-field errors"), if either CLI is missing the
// adapter returns a typed `ErrToolNotInstalled`. The provider
// propagates the error verbatim; the resolver maps it to a per-field
// GraphQL error so the daemon does not collapse just because one field
// could not be resolved.
//
// # Locating the CLIs
//
// The daemon usually runs under launchd or systemd, which do not
// source the operator's shell profile: PATH is the system default and
// holds none of the prefixes `bun install -g` / `npm install -g` /
// Homebrew write to. Bare-name lookup therefore misses `ccusage` on
// hosts where an interactive shell finds it (#400), and every quota
// field resolves null. Resolution (toolpath.go) searches, in order:
//
//   - ORCHARD_CLAUDE_BIN / ORCHARD_CCUSAGE_BIN — an explicit
//     executable. Set but not executable is an error, never a silent
//     fall-through.
//   - PATH.
//   - ~/.bun/bin, ~/.local/bin, /opt/homebrew/bin, /usr/local/bin —
//     replaced wholesale by ORCHARD_BIN_DIRS (a PATH-style list) when
//     that is set; set it empty to search nothing beyond PATH.
//
// A tool that resolves nowhere is logged once per not-found
// transition, at Warn.
//
// PII: the adapter MUST NOT log raw stdout, because `claude auth
// status` includes the user's email. The provider's logger only sees
// structured fields the implementation has explicitly redacted.
package claudeaccount
