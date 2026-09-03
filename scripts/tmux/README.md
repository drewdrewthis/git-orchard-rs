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
| `@orchard_daemon_url` | `http://127.0.0.1:7777/graphql` | The daemon's GraphQL endpoint. A base URL (`http://host:7777`) gets `/graphql` appended; a full endpoint is used as given. Point it at another host to label panes from a remote daemon. |
| `@orchard_key` | `s` | Prefix key the picker binds to. Moving it unbinds the key the plugin previously took — that key is left *unbound*, not restored to its stock binding. |
| `@orchard_heartbeat_dir` | _(unset)_ | Where Claude hook state files live. Unset means the helper resolves it itself: `$ORCHARD_HEARTBEAT_DIR`, then `$TMPDIR`, then `/tmp`. |

```tmux
set -g @orchard_key 'w'
set -g @orchard_daemon_url 'http://127.0.0.1:8080/graphql'
run-shell '/path/to/git-orchard-rs/scripts/tmux/orchard.tmux'
```

### Environment variables

`pane-labels.sh` also reads two variables. `orchard.tmux` sets neither, so
under the documented install path they only take effect if you export them
into tmux's own environment.

| Variable | Effect |
|----------|--------|
| `ORCHARD_LABEL_ENRICH` | `1` turns on the heavy query. **Required for the PR-status, id, title and label cells** — see the table below. |
| `ORCHARD_DAEMON_URL` | Fallback endpoint when `--daemon-url` is not passed. Accepts a base URL or a full endpoint, same as `@orchard_daemon_url`. `orchard.tmux` always passes `--daemon-url`, so this is ignored under the plugin path. |

## What a label contains

Cells are joined by two spaces; **every cell is dropped when its data is
absent**, so a bare shell gets a short label and a busy worktree gets a long one.

Four cells need `ORCHARD_LABEL_ENRICH=1`. Enriching each worktree with
PR/issue data hits the gh provider per worktree and can take 30s+ on a cold
cache, so the default query is deliberately lean — `prefix + <key>` stays
under about a second even with 30+ worktrees. **The default install shows the
five unmarked cells only.**

| Cell | Source | Needs enrich | Example |
|------|--------|:------------:|---------|
| PR status glyph | daemon `pr` | ✅ | `🚫` `🔴` `⚠` `📝` `⬆` `🟢` |
| Issue / PR ids | daemon `issue`, `pr` | ✅ | `#742 / PR#751` |
| Issue title | daemon `issue.title` | ✅ | truncated to 55 chars |
| PR labels | daemon `pr.labels` | ✅ | `[bug] [ready]` (first 3) |
| Branch | daemon `worktree.branch` | | `issue-742/pane-width` |
| Repo slug | daemon `repo.slug` | | `orchardist` |
| Claude state | hook state file | | `⏺ working` `⏸ idle` `⌨ input` |
| Process | `pane_current_command` | | `⏵ claude` |
| Path | pane cwd | | shown only when no worktree matches |

To turn the enriched cells on, put the variable in tmux's environment before
the `run-shell` line:

```tmux
set-environment -g ORCHARD_LABEL_ENRICH 1
run-shell '/path/to/git-orchard-rs/scripts/tmux/orchard.tmux'
```

### Untrusted text in a label

Branch names, issue titles, PR labels, hook-state fields and pane paths are
all attacker-influenceable, and `choose-tree` renders the label through
`#{E:@orchard_pane_label}` — a second format expansion, in which `#(cmd)`
executes `cmd`. Every cell is therefore built by one `cell()` constructor that
doubles `#` in the text and leaves only its own style markers live. A payload
shows up in the picker as inert literal text.

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
alone is not enough to claim a pane. State attaches to the pane whose path is
at or beneath the recorded `cwd`, or failing that to the pane actually running
`claude`. Other panes in the same session stay unlabelled.

`state` is rendered only when it is one of `working`, `idle` or `input` —
the values `orchard-state.sh` writes. Anything else is dropped with a warning
on stderr rather than passed through as free text.

**Provenance.** The heartbeat dir defaults to `/tmp`, which is world-writable,
so any local process can drop an `orchard-claude-<session>.json` naming a
session it does not own. A sidecar is read only when the invoking user owns
it, it is a regular file (not a symlink), and it is not world-writable; the
directory itself is refused if it is world-writable without the sticky bit.
Rejected files are skipped, never fatal.

## Degradation

The binding runs the labeler with `run-shell -b`, so `choose-tree` opens
immediately on the previous labels rather than waiting on the daemon.

**The picker always opens.** Every failure below costs labels, never the
binding:

| Condition | Result |
|-----------|--------|
| Daemon unreachable | Empty result set, exit 0, every pane still labelled from local data (path, Claude state, process). |
| Daemon answers 2xx with a body that is not JSON, or is JSON of the wrong shape | Same fallback as unreachable. |
| `HOME` unset or empty | Path cell is left unabbreviated. |
| Helper missing or not executable | Picker binds alone; labels fall back to command + path. |
| An apostrophe in the labeler path, daemon URL or heartbeat dir | tmux cannot quote it inside a binding, so labels are disabled and the reason is printed. Picker binds alone. |
| Heartbeat dir or a sidecar fails the provenance check | That sidecar is skipped; every other cell still renders. |

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
