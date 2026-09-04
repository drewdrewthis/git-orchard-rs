# ADR-022: Data + Graph Modeling

## Status
Accepted.

## Context

The daemon's job is to expose a graph of nodes (`Worktree`, `Pane`, `Conversation`, `ClaudeInstance`, `PullRequest`, ...) and serve them through typed lookups. The recurring failure mode is the opposite: a new caller needs data the daemon doesn't yet expose, and instead of modeling the underlying node and its lookup axes, we wrap, synthesize, or adapt the existing shape to fit the caller.

Canonical example: `claudeinstance.TmuxSeeder` synthesizes `Heartbeat` records from tmux pane state so that claude REPLs surface in `claudeInstances` even when the SessionStart hook didn't write a heartbeat file. The actual model gap is that `Pane` is a first-class node and `claudeInstances` is a *view* over panes filtered by foreground command — `PanesByCommand("claude")`. The seeder exists because the file-reader's shape was treated as the contract instead of the underlying graph.

Symptoms of the same shape across the codebase:

- Wrappers/seeders/synthesizers/adapters that emit type X from type Y to fit an existing reader.
- Resolvers that walk a provider's full snapshot in a `for` loop instead of asking it a typed question.
- Provider methods named `For<SpecificCaller>` instead of `By<Axis>`.
- The same underlying data fetched in two different shapes through two different paths.
- Nullable schema fields that the daemon could always populate if it asked the right node.

This produces drift, duplicate code, debug-time hunts ("which path served this stale value?"), and schema noise.

## Decision

Every new feature that touches data goes through a **3-step gate** before any code lands:

### 1. Name the node

What graph entity does this serve? Always one of: `Pane`, `Worktree`, `Conversation`, `ClaudeInstance`, `PullRequest`, `Host`, `Repo`, `Process`, `TmuxSession`. If no node fits, the first deliverable is naming a new one — not a wrapper around an old one.

### 2. Name the lookup axes

What filters does the consumer actually need? `ByID`, `ByCwd`, `ByCommand`, `BySession`, `ByOwner`, `ByCommit`. Naming arity matches return shape:

- `PaneByID(id)` → one
- `PanesByCwd(cwd)` → many
- `PanesByCommand(cmd)` → many

If a new axis is missing, add it to the provider. Don't loop in the resolver.

### 3. Wire through provider → dataloader → resolver

- **Provider** holds the snapshot and exposes typed methods that build per-axis indices once per refresh tick.
- **Dataloader** lives in `internal/server/loaders/`. One loader per `(provider, axis)`. Per-request, never shared, never long-lived.
- **Resolver** is a thin `Load(key)` + projection. No for-loops over provider snapshots. No bespoke joins.

## Smells — stop and redesign when you see any of these

- About to write `*Seeder`, `*Synthesizer`, `*Adapter`, `*Composer` that produces type X from type Y to fit an existing reader.
- Resolver body has a `for` loop walking a provider snapshot.
- New provider method named `For<SpecificCaller>` instead of `By<Axis>`.
- Same underlying data fetched in two different shapes through two different paths.
- Schema has nullable fields where the underlying resource is mandatory because the daemon can't always populate it.
- The phrase "we'll just match these up after the fact" appears in the design.

## Examples

### Bad: claudeinstance seeded from tmux

`internal/server/providers/claudeinstance/` has a `Provider`, `Composer`, `Reader`, `Watcher`, `Heartbeat`, and `TmuxSeeder`. The seeder enumerates local tmux panes whose foreground process is `claude` and synthesizes `Heartbeat` records to feed the composer. This exists because `claudeInstances` was modeled as "things heartbeat files describe" — a reader-shape — rather than "panes running claude, enriched with transcript state".

### Good (target shape): `claudeInstances` is a view over `Pane`

```graphql
# Pane is the node. claudeInstances is a typed view.
type Pane {
  id: ID!
  command: String!
  cwd: String!
  tmuxSession: TmuxSession!
  claudeInstance: ClaudeInstance   # nullable — null when command != "claude"
}

type Query {
  pane(id: ID!): Pane
  panes(byCwd: String, byCommand: String): [Pane!]!
  claudeInstances: [ClaudeInstance!]!  # resolved as panes(byCommand: "claude").claudeInstance
}
```

The tmux provider already exposes `Sessions()`, `Windows()`, `Panes()`, `Clients()` as typed facets — adding `PanesByCommand` is one method, not a new subsystem. The `claudeinstance` package collapses to: jsonl decoder + resolver that loads panes-by-command and joins transcript state per pane cwd.

### Good (existing): `WorktreeForCwd`

`internal/server/loaders/loaders.go` exposes `WorktreeForCwd: *dataloader.Loader[string, *Worktree]` — one node (`Worktree`), one axis (`Cwd`), one dataloader. Resolvers call `Load(cwd)`. This is the shape every new lookup should match.

### Good (existing): `PullRequestEnrichment`

`PullRequestEnrichment: *dataloader.Loader[PullRequestKey, PullRequest]` batches per-PR enrichment from `gh` inside one GraphQL request window. Provider exposes `BatchEnrichPullRequests(keys)`; loader fans in; resolver projects. No per-row gh calls in resolvers.

## Consequences

- **No rewrite mandate.** Existing subsystems stay until touched. New work follows the gate.
- **Schema-first** stays the rule (ADR-016): the gate runs *before* schema edits, not after.
- **Provider surface grows** typed by-axis methods rather than per-caller helpers. Tests narrow accordingly.
- **Loaders accumulate** one entry per `(node, axis)` pair. `Loaders` in `loaders.go` is the visible inventory of what the daemon serves cheaply.
- **`claudeinstance` is the canonical follow-up refactor** — collapse `Provider`, `Composer`, `Reader`, `Watcher`, `Heartbeat`, `TmuxSeeder` into a resolver over `Pane` + jsonl-keyed transcript state. Out of scope for this ADR; tracked separately.

See ADR-016 (GraphQL as protocol), ADR-017 (layer responsibilities), ADR-019 (no client-side cache layers — the GUI counterpart of this rule).

## Amendments

- 2026-09-04: applied the gate to `ClaudeSessionRegistry` (node), `SessionByPid`
  (axis), provider → resolver directly, no dataloader — each pane's pid is
  distinct per request, so there's nothing to batch (#743).
