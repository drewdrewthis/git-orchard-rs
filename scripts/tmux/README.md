# orchard.tmux — the pane picker

`prefix + s` opens an expanded `choose-tree` where every **pane** row is
labelled with what is actually happening in it: the worktree's branch, its
PR/issue chrome, the orchard repo it belongs to, the Claude session state,
and the running process.

Two files:

| File | Role |
|------|------|
| `orchard.tmux` | Plugin entry script. tmux runs it once at config load; it installs the key binding. |
| `pane-labels.sh` | The labeler. Queries the daemon, folds in Claude hook state, writes `@orchard_pane_label` per pane. |

`orchard.tmux` owns its own wiring — you do not paste a `bind-key` into
`tmux.conf`, and no copy of the helper needs to live on `PATH`.

## Install

**In-repo** — one line in `tmux.conf`:

```tmux
run-shell '/path/to/git-orchard-rs/scripts/tmux/orchard.tmux'
```

**TPM** — if this directory is ever split into its own repo:

```tmux
set -g @plugin 'drewdrewthis/orchard.tmux'
```

Then `prefix + I` to install, `prefix + r` to reload.

Either way `orchard.tmux` resolves `pane-labels.sh` relative to its own
location, so the two files just have to stay siblings.

### Superseded: `~/.local/bin/orchard-tmux-labels`

The old install was a hand-copied duplicate of `pane-labels.sh` in
`~/.local/bin`, plus a hand-written `bind-key` in `tmux.conf` pointing at it.
Both are obsolete. That copy drifts from the repo silently and is invisible to
CI — delete it, drop the hand-written bind, and use the `run-shell` line above.

## Configuration

Set these **before** the `run-shell` line — they are read at load time.

| Option | Default | Purpose |
|--------|---------|---------|
| `@orchard_daemon_url` | `http://127.0.0.1:7777/graphql` | Full GraphQL endpoint. Point it at another host to label panes from a remote daemon. |
| `@orchard_key` | `s` | Prefix key the picker binds to. |
| `@orchard_heartbeat_dir` | _(unset)_ | Where Claude hook state files live. Unset means the helper resolves it itself: `$ORCHARD_HEARTBEAT_DIR`, then `$TMPDIR`, then `/tmp`. |

```tmux
set -g @orchard_key 'w'
set -g @orchard_daemon_url 'http://127.0.0.1:8080/graphql'
run-shell '/path/to/git-orchard-rs/scripts/tmux/orchard.tmux'
```

## What a label contains

Cells are joined by two spaces; **every cell is dropped when its data is
absent**, so a bare shell gets a short label and a busy worktree gets a long one.

| Cell | Source | Example |
|------|--------|---------|
| PR status glyph | daemon `pr` | `🚫` `🔴` `⚠` `📝` `⬆` `🟢` |
| Issue / PR ids | daemon `issue`, `pr` | `#742 / PR#751` |
| Issue title | daemon `issue.title` | truncated to 55 chars |
| Branch | daemon `worktree.branch` | `issue-742/pane-width` |
| PR labels | daemon `pr.labels` | `[bug] [ready]` (first 3) |
| Repo slug | daemon `repo.slug` | `orchardist` |
| Claude state | hook state file | `⏺ working` `⏸ idle` `⌨ input` |
| Process | `pane_current_command` | `⏵ claude` |
| Path | pane cwd | shown only when no worktree matches |

### Claude state

Per [ADR-007](../../docs/adr/007-session-data-model.md), Claude enrichment comes
from hook state files, not from tmux. `~/.claude/hooks/orchard-state.sh` writes
one sidecar per tmux session, `orchard-claude-<tmux_session>.json`, containing
exactly:

```json
{
  "state": "working",
  "session_id": "a19ad031-…",
  "tmux_session": "daemon-wire",
  "cwd": "/Users/…/git-orchard-rs",
  "event": "PostToolUse",
  "timestamp": "2026-08-28T12:30:42Z"
}
```

`state` is the only rendered field. **Model and context-window percentage are
not in this payload** — they are status-line telemetry, and
`ClaudeSessionInfo::from_state_file` leaves both `None`. The labeler renders
them only if a writer ever supplies them, and shows nothing at all otherwise;
it never emits a placeholder.

A sidecar is per-*session* but a label is per-*pane*, so a session-name hit
alone is not enough to claim a pane. State attaches to the pane whose path
matches the recorded `cwd`, or failing that to the pane actually running
`claude`. Other panes in the same session stay unlabelled.

## Degradation

The binding runs the labeler with `run-shell -b`, so `choose-tree` opens
immediately on the previous labels rather than waiting on the daemon.

If the daemon is unreachable the labeler substitutes an empty result set,
exits 0, and still labels every pane from local data (path, Claude state,
process). The picker always opens.

## Tests

```sh
bats scripts/tmux/pane-labels.bats scripts/tmux/orchard-tmux.bats
```

Run under CI by the `Bats (scripts)` job (`bats -r scripts plugin-sources`).

`pane-labels.bats` drives the script through `--print`, which composes labels
to stdout instead of writing tmux options, so no tmux server is needed. The
daemon is faked at the wire by `testdata/fake-daemon.py` — a canned HTTP
response, so the real `curl` call and the real JSON parse both execute.
`orchard-tmux.bats` runs the plugin against a throwaway tmux server on its own
socket, started with `-f /dev/null`, so your real bindings are never touched.

## Known deviation: RULES.md L11

L11 — *"Scripts do not call the daemon. … A script MUST NOT invoke `orchard
<subcommand>` or hit `127.0.0.1:7777`."*

`pane-labels.sh` does exactly that, and has since before this plugin existed.
The refactor preserves the behaviour rather than fixing it, so the deviation
carries over unchanged — it is not resolved here and should not be read as
sanctioned. L11's stated rationale is a `script → daemon → script` cycle that
deadlocks under L5 mutations; this helper is read-only and is triggered by a
tmux keybinding rather than by a mutation, which is why it has not bitten yet.
Resolving it properly means moving the query behind an `orchard` subcommand
that the script consumes, or having the daemon push labels itself.
