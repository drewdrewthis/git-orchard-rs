# Git Orchard — Vision & Direction

## What is this?

A terminal-native task and session management system for developers who work across multiple repos, worktrees, and AI coding agents.

## The Problem

The day-to-day developer workflow involves juggling:

- **GitHub issues** sorted by priority — the "what to work on"
- **Git worktrees** — the "where the code lives"
- **Tmux sessions** — the "where the work happens"
- **AI agent sessions** (Claude, etc.) — the "who's helping"

These are all loosely coupled but managed separately. Sessions get lost. Worktrees orphan. Issues lose context. There's no single place to see: *what's in flight, what's ready, what's done*.

## Core Frictions (as of 2026-03-20)

1. **Losing Claude sessions.** It's easy to lose track of which AI session is doing what, or to accidentally abandon one. Sessions should be first-class, persistent, recoverable.
2. **Worktrees ≠ tasks.** Sometimes a session has no worktree. Sometimes a worktree has no session. The real unit of work is a *task* (often a GitHub issue), and worktrees/sessions are just resources attached to it.
3. **Multi-repo management.** Work spans multiple repos. Orchard started as a single-repo worktree manager, but the real need is broader.
4. **Context switching tax.** Jumping between GitHub web UI, terminal, and editor to figure out what to do next. Too many surfaces.

## The Vision

A terminal-first command center where:

- **Tasks are the primary unit.** Each task maps to an issue (GitHub or otherwise). Worktrees and sessions are resources *attached* to tasks.
- **The main view is a sortable task list**, inspired by Kanban but adapted for terminal:
  - **Top:** Ready / needs attention (bubbles up)
  - **Middle:** In progress (active sessions, open PRs)
  - **Bottom:** Backlog (paginated)
  - **Done items get cleaned out** (archived, worktrees pruned)
- **Sessions are managed, not lost.** AI agent sessions are tracked, recoverable, and visible. You can see what Claude is working on, resume it, or reassign it.
- **Multi-repo aware.** Configuration declares which repos Orchard manages. Tasks can span repos.
- **Everything in the terminal.** Minimize trips to the browser. GitHub issues, PR status, CI status — all surfaced in the TUI.

## Personas

### Solo Developer (primary, current)
Works across 1–5 repos. Uses GitHub issues for planning. Delegates implementation to AI agents. Needs to keep track of what's running, what's blocked, what's next.

### Small Team (future)
2–5 developers sharing a GitHub project board. Each person sees their own task queue. Orchard reflects the shared Kanban state.

## Key Concepts

| Concept | Description |
|---------|-------------|
| **Task** | The primary unit of work. Backed by a GitHub issue. Status: backlog → ready → in_progress → in_review → done. |
| **Session** | A tmux session attached to a task. Multiple sessions per task (e.g., your shell + Claude). |
| **Pane** | A pane within a tmux session. Agent detection happens at pane level — any pane can be running Claude. |
| **Worktree** | A git worktree attached to a task. Optional — created on demand when you start working. |
| **Repo** | A git repository that Orchard manages. Declared in global config. |
| **Event** | A structured JSON log entry. Everything that happens is logged for observability and debugging. |

## Architecture Decisions (resolved)

### Multi-repo config → global
Config lives at `~/.config/git-orchard/config.json` with a `repos` array. Per-repo `.git/orchard.json` is still read for remote config (backwards compat), but global config wins on conflict. Running orchard in a new repo auto-adds it.

### State → JSON file, global
State lives at `~/.local/state/git-orchard/state.json`. Contains tasks with their status, priority, worktree path, session names, and source (GitHub issue). Written atomically after every mutation. Read on startup for instant TUI render, then enriched by live data in background.

### Sessions → multiple per task, pane-level tracking
A task has a `sessions` array, not a single session. Each session's panes are scanned for agent detection (Claude in pane title or command). This matches real usage: you open a second pane, start Claude there, and Orchard sees it.

### Observability → structured event log (JSONL)
`~/.local/state/git-orchard/events.jsonl` — every meaningful action is a JSON line with timestamp and event type. Queryable with `jq`. Not OTEL, but OTEL-shaped — could be piped to a collector later. Coexists with the existing debug.log for low-level output.

### GitHub integration → issues + PRs, no project boards
Pull issues and PR status. Don't mirror GitHub Projects board state — Orchard's task list *is* the board. Priority and ordering are local.

### Issue sources → GitHub only (for now)
The `source` field on tasks (`{ "type": "github_issue", ... }`) leaves the door open for Linear/Jira/etc. without building the abstraction today.

### TUI layout → vertical status bands, not horizontal Kanban
Ready → In Progress → In Review → Backlog, top to bottom. Compact lines for ready/backlog, expanded detail for in-progress. Done tasks hidden (cleaned up separately).

### TUI hosting → tmux popup, not dedicated session
The TUI runs as a tmux popup (`display-popup -E`), not in its own dedicated tmux session. This eliminates the alternate screen vs switch-client conflict, the restart loop, the shell function's session management, and the runtime keybinding manipulation. The user binds one key in `~/.tmux.conf` and Orchard never touches tmux keybindings. Session switching happens after the popup exits.

## Open Questions

- **Naming:** Is "Orchard" still the right name? TBD — not blocking.
- **Agent launching:** Should Orchard run `claude` for you, or just track sessions you create? Leaning toward "just track" with the option to create sessions that are pre-configured for agent use.
- **Remote task support:** Tasks that span local and remote worktrees. The transfer system exists but needs to be task-aware.
- **Conflict resolution:** When cached state disagrees with live data (e.g., session died, issue closed externally), what's the UX for reconciliation?

## Inspirations

- **Kanban Code** — Task-driven development, everything flows from issues
- **GitHub Projects** — Board view, status fields, automation
- **Lazygit** — Proof that complex git workflows work great in TUI
- **Claude Code's `/recover`** — Session recovery as a first-class concept

## What This Doc Is For

This is the living document for product direction. Not implementation specs (those go in `specs/features/`), not architecture decisions (those go in ADRs). This is for:

- Articulating *why* we're building what we're building
- Capturing frictions and pain points as they come up
- Exploring ideas before they become specs
- Keeping the big picture visible as the project evolves
