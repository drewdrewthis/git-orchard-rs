# Cache Architecture

## Core Insight

There is no user-managed state. Everything is derived from external services (GitHub, git, tmux). The app is a **read-only dashboard** that caches service data for instant rendering.

## Current Problem

`state.json` tries to be both a cache and a user-editable state store. This causes:
- Status drift (stored status disagrees with live data)
- Accidental mutations (pressing `d` marks things done permanently)
- Fragile recovery (resetting status to wrong values)
- Single-repo scoping (CWD determines which repo's data loads)

## Correct Architecture

Each data source owns its own cache. Orchard reads all caches on startup, renders instantly, then refreshes each source in the background.

### Data Sources

| Source | What it provides | Cache file |
|--------|-----------------|------------|
| GitHub Issues | issue number, title, state (open/closed), labels | `~/.cache/orchard/{owner}_{repo}_issues.json` |
| GitHub PRs | PR number, state, review decision, checks, conflicts, unresolved threads | `~/.cache/orchard/{owner}_{repo}_prs.json` |
| Git Worktrees | worktree paths, branches, bare/conflict status | `~/.cache/orchard/{owner}_{repo}_worktrees.json` |
| Tmux Sessions | session names, paths, pane titles, commands | `~/.cache/orchard/tmux_sessions.json` |
| Remote Worktrees | same as worktrees but via SSH | `~/.cache/orchard/{owner}_{repo}_remote_worktrees.json` |

### Derived View (not stored)

The display groups (needs_you, claude_working, claude_done, in_review, backlog) are **computed at render time** by joining the cached data:

1. Start with issues (each issue = a potential task row)
2. Join with worktrees by issue number in branch name
3. Join with PRs by branch → PR association
4. Join with tmux sessions by worktree path
5. Derive display group from PR status + session status
6. Derive PR status text, Claude status, host from joined data

### Startup Flow

```
1. Read all cache files (instant — just file reads)
2. Join & derive → render TUI immediately
3. Background: refresh each source in parallel
   - gh issue list → update issues cache
   - gh pr list → update PR cache
   - git worktree list → update worktrees cache
   - tmux list-sessions/panes → update sessions cache
   - SSH remote worktrees → update remote cache
4. On each source update: re-join, re-derive, re-render, write cache
```

### Multi-Repo

The config declares repos to manage:
```json
// ~/.config/orchard/config.json
{
  "repos": [
    { "slug": "langwatch/langwatch", "path": "/Users/USER/workspace/langwatch", "remote": { "host": "ubuntu@10.0.0.1", "path": "/home/ubuntu/langwatch-workspace" } },
    { "slug": "drewdrewthis/git-orchard-rs", "path": "/Users/USER/workspace/git-orchard-rs" }
  ]
}
```

Each repo's sources are refreshed independently. The TUI shows tasks from ALL repos, grouped by display status.

### What Goes Away

- `state.json` — replaced by per-source caches
- `TaskStatus` enum — replaced by derived `DisplayGroup`
- `s` key (start task) — no manual status
- `d` key (mark done) — already removed, issues close on GitHub
- `p` key (priority) — priority comes from GitHub labels or issue order
- `issue_sync.rs` — replaced by a GitHub issues cache source
- `merge_worktrees_into_tasks` — replaced by the join logic

### What Stays

- `events.jsonl` — structured event log (observability, not state)
- `status.txt` — tmux status bar segment (written from derived data)
- The TUI rendering, key handlers (enter, o, c, q)
- The popup model, wrapper script, init wizard
- Remote worktree support, session switching

### Priority for Implementation

This is a significant refactor. Suggested order:
1. Create the cache module with per-source read/write
2. Move existing collector stages to write caches
3. Replace state.json task list with cache-derived join
4. Add multi-repo config and per-repo refresh
5. Remove state.rs, issue_sync.rs, TaskStatus
