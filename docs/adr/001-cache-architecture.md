# ADR-001: Per-Source Cache Architecture

## Status
Proposed

## Date
2026-03-20

## Context

Orchard is evolving from a single-repo worktree manager into a terminal-native task and session command center. Tasks (GitHub issues) are the primary unit of work; git worktrees and tmux sessions are resources attached to tasks.

The current `state.json` (at `~/.local/state/git-orchard/state.json`) attempts to serve two conflicting roles simultaneously: an in-process cache for live data and a user-editable state store. This dual role causes several problems:

- **Status drift.** Stored `TaskStatus` (e.g. `InProgress`) diverges from actual live state (the tmux session died, the PR merged) with no automatic reconciliation.
- **Accidental mutations.** Key bindings like `s` (start) and `d` (done) write permanent status changes. A mispress requires manual JSON surgery to correct.
- **Fragile recovery.** If the file is corrupted or deleted, task state is lost; there is nothing to fall back to because the ground truth lives in external services.
- **Single-repo scoping.** `AppState` loads relative to CWD, making it impossible to show tasks from multiple repos in one view.
- **Mixed abstraction.** `TaskStatus`, `issue_sync.rs`, and `merge_worktrees_into_tasks` are all bridging logic that exists solely because status is stored rather than derived.

The existing types in `src/state.rs` make this concrete: `AppState` holds a flat `Vec<Task>`, each `Task` carries a `TaskStatus` enum and directly stored fields (`worktree`, `sessions`, `pr`, `remote_host`) that duplicate live data from git, tmux, and GitHub.

## Decision

Replace `state.json` with a set of per-source cache files. Each external data source owns exactly one cache file. The display model (task rows, status groups) is derived at render time by joining the caches — nothing is stored that can be recomputed from live services.

### Data Sources and Cache Files

| Source | Data provided | Cache file |
|--------|--------------|------------|
| GitHub Issues | issue number, title, open/closed state, labels | `~/.cache/orchard/{owner}_{repo}_issues.json` |
| GitHub PRs | PR number, state, review decision, CI checks, merge conflicts, unresolved threads | `~/.cache/orchard/{owner}_{repo}_prs.json` |
| Git Worktrees | worktree paths, branch names, bare/conflict status | `~/.cache/orchard/{owner}_{repo}_worktrees.json` |
| Tmux Sessions | session names, paths, pane titles, running commands | `~/.cache/orchard/tmux_sessions.json` |
| Remote Worktrees | same as Git Worktrees but polled over SSH | `~/.cache/orchard/{owner}_{repo}_remote_worktrees.json` |

The `{owner}_{repo}` prefix scopes local caches per repository, enabling multi-repo operation from a single TUI instance. The tmux sessions cache is global because sessions are not per-repo.

### Derived View (computed, not stored)

Display groups (`needs_you`, `claude_working`, `claude_done`, `in_review`, `backlog`) are computed at render time by joining the five source caches:

1. Start with issues — each open issue is a candidate task row.
2. Join with worktrees — match by issue number in branch name.
3. Join with PRs — match by branch to PR association.
4. Join with tmux sessions — match by worktree path.
5. Derive display group from PR review state, CI state, and session activity.
6. Derive display text (PR status, Claude indicator, host label) from joined data.

No intermediate computed state is written to disk.

### Startup Flow

```
1. Read all cache files (instant — file reads only, no network)
2. Join & derive → render TUI immediately with last-known data
3. Background: refresh each source in parallel
   - gh issue list        → write issues cache
   - gh pr list           → write PRs cache
   - git worktree list    → write worktrees cache
   - tmux list-sessions   → write sessions cache
   - SSH remote poll      → write remote worktrees cache
4. On each source update: re-join, re-derive, re-render, write updated cache
```

Stale cache renders fine on startup (last-known state). Background refresh converges to current state within seconds.

### Multi-Repo Configuration

A global config at `~/.config/orchard/config.json` declares all managed repos:

```json
{
  "repos": [
    {
      "slug": "owner/repo-a",
      "path": "/Users/USER/workspace/repo-a",
      "remote": { "host": "ubuntu@10.0.0.1", "path": "/home/ubuntu/repo-a-workspace" }
    },
    {
      "slug": "owner/repo-b",
      "path": "/Users/USER/workspace/repo-b"
    }
  ]
}
```

Each repo's sources are refreshed independently. The TUI aggregates tasks from all repos into a single view grouped by display status.

### What Gets Removed

- `state.json` — replaced by per-source cache files.
- `AppState` and `Task` structs in `src/state.rs` — the monolithic task record that tried to own all fields.
- `TaskStatus` enum (`Backlog`, `Ready`, `InProgress`, `InReview`, `Done`) — replaced by a derived `DisplayGroup` computed from live source data.
- `issue_sync.rs` — replaced by a GitHub Issues cache source with its own refresh logic.
- `merge_worktrees_into_tasks` — replaced by the render-time join described above.
- `s` key binding (start task) — no manual status transitions; state derives automatically.
- `d` key binding (mark done) — already removed; issues close on GitHub and the cache reflects that.
- `p` key binding (set priority) — priority comes from GitHub issue order or labels.

### What Stays

- `events.jsonl` — structured event log for observability; not state, not replaced.
- `status.txt` — tmux status bar segment, written from derived render data.
- TUI rendering, key handlers (`enter`, `o`, `c`, `q`), popup model, and wrapper script.
- Remote worktree support and session switching.
- Per-repo `.git/orchard.json` for backwards-compatible remote host config.

## Consequences

### Positive

- **No status drift.** Every render reflects what external services actually report. There is nothing stored that can disagree with reality.
- **Safe by default.** No key press can permanently corrupt task state; the worst a user can do is navigate.
- **Instant startup.** Cache reads are local file I/O, so the TUI renders immediately regardless of network or GitHub availability.
- **Multi-repo native.** Cache files are scoped per repo by design; adding a new repo adds a new set of cache files with no schema migration.
- **Simpler code.** Removing `TaskStatus`, `issue_sync.rs`, and `merge_worktrees_into_tasks` eliminates a significant layer of bridging logic.
- **Independently evolvable sources.** Each cache source can be refreshed, retried, or replaced without touching the others.

### Negative

- **Significant refactor.** `state.rs`, `issue_sync.rs`, and parts of `main.rs` and the TUI must be rewritten. Existing tests for state serialization are invalidated.
- **Cold start shows stale data.** On first run (empty caches) or after clearing caches, the TUI shows nothing until the first background refresh completes. This is a worse first-run experience than the current state where tasks are explicitly persisted.
- **Cache staleness is visible.** If the user runs Orchard while offline or GitHub is down, the TUI shows whatever the last successful refresh captured. Users must understand that the display may lag behind reality.
- **No user-managed ordering.** Removing manual priority (`p` key) means the task order is whatever GitHub's API returns. Users who relied on local reordering lose that capability.

### Risks

- **Cache directory permissions.** If `~/.cache/orchard/` is not writable, all caches fail silently. Mitigation: check and warn on startup; degrade gracefully to in-memory-only refresh results.
- **Concurrent refresh races.** Multiple source refreshes writing cache files simultaneously could produce a briefly inconsistent joined view. Mitigation: each source owns exactly one file; joins read atomically by loading all files before joining; no cross-file transactions needed.
- **Breaking existing state.** Users with tasks persisted in `state.json` lose that data on migration. Mitigation: document the migration in release notes; the data is reconstructible from GitHub.
- **SSH availability for remote caches.** Remote worktree polling depends on SSH connectivity. Mitigation: remote cache is optional; a failed SSH poll leaves the previous cache in place and does not block rendering.
