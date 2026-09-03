# orchardist 🌲🌳🌴

A command center for git worktrees, PRs, tmux sessions, and Claude Code sessions — across repos and machines.

Orchardist is an ecosystem of cooperating binaries and agent-layer tools. One `orchard` command dispatches to them; the languages (Rust, Go, Tauri) matter less than what the components do together.

![License](https://img.shields.io/badge/license-MIT-blue)

📰 **Blog:** [Grow the Orchard](https://growtheorchard.substack.com/)

## What is orchardist

Orchardist gives you a single dashboard showing everything happening across your repos: which worktrees have PRs, what state they're in, which Claude sessions are working/idle/waiting for input, and what needs your attention.

![Orchard TUI](assets/screenshot.png)

The ecosystem spans a terminal dashboard, a desktop GUI, a local GraphQL daemon, worktree and chat CLIs, a tmux popup integration, and Claude Code agent skills — all wired together through one dispatcher.

## The ecosystem

One binary — `orchard` — is the user-facing entry point. It dispatches verbs to the right helper, so you never need to remember which binary owns which command.

### Binaries

| Binary | Role | Stack | Location | Build |
|--------|------|-------|----------|-------|
| **`orchard`** | Dispatcher — the only binary users invoke directly; routes verbs to helpers (`orchard tui` → `orchard-tui`, etc.) | Rust | `crates/orchard-dispatcher` | `make dispatcher` |
| **`orchard-tui`** | Terminal dashboard — worktrees, PRs, sessions, CI in one view; `--json`/`--schema` wire format | Rust + Ratatui 0.30 | `crates/orchard` | `make rust` → `target/release/orchard-tui` |
| **`orchard-daemon`** | Read/join GraphQL daemon on `localhost:7777`; schema-first (`schema.graphql` at repo root, `make generate` to regenerate Go types); read-only join layer over git, tmux, gh, claude, processes, and federation peers | Go (`github.com/drewdrewthis/orchardist`) | `cmd/orchard-daemon` | `make daemon` → `bin/orchard-daemon` |
| **`orchard-gui`** | Desktop app for deep Claude session view | Tauri 2 + SvelteKit / Houdini / Tailwind / xterm | `crates/orchard-gui` | `make gui` |
| **`orchard-worktree`** | Worktree mutation CLI; dispatched as `orchard new/rm/mv/prune/ls/path` | Rust | `crates/orchard-worktree` | `make worktree-cli` |
| **`orchard-chat`** | Cross-machine agent-to-agent chat (JSONL rooms, tmux fanout); dispatched as `orchard send/chat` | Rust | `crates/orchard-chat` + `crates/chat-core` | — |

> The binaries are named distinctly from the repo slug on purpose so they can coexist on the same `$PATH`.

### Non-binary surfaces

**tmux popup** — `orchard-tui init` installs an `orchard()` shell function, a `Ctrl-o` tmux popup keybinding, and an optional status bar. The TUI then lives one keystroke away from any terminal.

**Claude skills / agent layer** — the `/install-orchard` skill (`.claude/skills/install-orchard/`) automates orchardist setup from inside a Claude Code session. The bundled `orchardist` plugin in `.claude-plugin/marketplace.json` provides `/orchardist:recall` — session-search and synthesis over past Claude Code conversations.

**Federation / remote** — manage worktrees on remote SSH hosts; transfer (push/pull) worktrees between machines; reachability indicators in the dashboard.

### History

Renamed from `git-orchard` to `orchardist` on 2026-06-09; the old URL redirects.

## Features

- **Unified dashboard** — worktrees, PRs, issues, tmux sessions, and Claude state in one view
- **Multi-repo** — switch between projects with left/right arrows
- **Claude Code integration** — see which sessions are working, idle, or waiting for input via hooks
- **Smart grouping** — shepherd (main sessions), needs attention, claude working, ready to merge, other
- **Priority toggle** — flag worktrees as priority to keep them at the top
- **PR status** — review decisions, CI checks, merge conflicts, unresolved threads
- **Issue state** — closed issues flagged for cleanup
- **Cleanup** — select and delete worktrees with merged PRs or closed issues
- **Click-to-switch notifications** — desktop notifications when Claude finishes; click to jump to the session
- **Remote worktrees** — manage worktrees on remote machines via SSH, with reachability indicators
- **JSON output** — `orchard-tui --json` returns complete, fresh data for scripting
- **Auto-refresh** — background refresh with two-phase loading (fast locals, then slow remotes)
- **Mouse support** — click to select worktrees, scroll to navigate
- **Transfer** — push/pull worktrees between local and remote machines
- **Self-healing** — `orchard-tui heal` audits and repairs drifted state
- **Theme system** — centralized semantic color definitions for consistent styling

## Install

### From source

```bash
cargo install --git https://github.com/drewdrewthis/orchardist orchard --bin orchard-tui

# Or from a local checkout:
cargo install --path crates/orchard --bin orchard-tui
```

The binaries are named distinctly from the repo slug (`orchardist`) on purpose so they can coexist on the same `$PATH`.

### Prebuilt (curl installer)

```bash
curl -fsSL https://raw.githubusercontent.com/drewdrewthis/orchardist/main/scripts/install.sh | bash -s -- --prefix ~/.local/bin
```

Other flags: `--system` (install to `/usr/local/bin`, elevating via `sudo -n` if needed), `--no-service` (skip installing/restarting the `orchard.service` systemd user unit), `--version vX.Y.Z` (pin a release), `--from-source`, `--json` (emit an `{ok,data,error}` envelope instead of text). The installer downloads one version-matched suite tarball and installs all six binaries: `orchard`, `orchard-tui`, `orchard-daemon`, `orchard-sidebar`, `orchard-shell`, `orchard-upgrade`.

## `orchard shell`

A tmux wrapper that puts a live worktree/session sidebar beside your terminal — an outer tmux server hosting the sidebar pane plus a nested attach into your normal (inner) tmux session.

```bash
orchard shell                # boot (or reattach to) the wrapper
orchard shell doctor         # health check; --json for machine-readable output
```

| Key | Action |
|-----|--------|
| `M-s` | Collapse/expand the sidebar |
| `M-p` | Open the command palette popup |
| `M-d` | Detach the outer client |
| `M-Left` / `M-Right` | Focus sidebar / inner pane |
| `M-Up` / `M-Down` | Move sidebar selection from either pane |
| `M-1`…`M-9` | Jump selection to the nth visible card from either pane |
| `/` | Filter the sidebar list (sidebar focused) |
| `b` | Toggle the attention bell (sidebar focused) |
| `+` | Open the new-session modal |
| Click a card | Switch to that session |
| Right-click a card | Rename or close that session |

Details, layout, and full rationale: [docs/outer-shell.md](docs/outer-shell.md).

## Upgrading

```bash
orchard upgrade           # download, verify, and replace the installed suite
orchard upgrade --check   # report current/latest versions, change nothing
orchard upgrade --dry-run # download and verify, report what would change
```

The sidebar header shows `⇡` when a newer release is available (from a 24h-cached background check — no network call on render); the hint is to run `orchard upgrade`.

## Setup

```bash
orchard-tui init
```

This will:
1. Add the `orchard` shell function to your rc file (launches inside tmux)
2. Set up a tmux keybinding (default: `Ctrl-o`)
3. Optionally add orchard status to your tmux status bar
4. Install Claude Code hooks for session state detection

## Usage

```
orchard-tui                    Interactive dashboard
orchard-tui cleanup            Jump straight to cleanup view
orchard-tui heal               Audit state for drift (dry-run by default)
orchard-tui heal --fix         Apply repairs
orchard-tui setup-remote HOST  Provision a remote host for SSH worktrees
orchard-tui init               Setup wizard
orchard-tui --json             Full state as JSON (always fresh, never cached)
orchard-tui --schema           Print the JSON Schema for --json output and exit
```

### Agent-oriented output

`--json` is the single wire format. Agents that want a token-efficient
encoding should pipe through [`@toon-format/cli`](https://www.npmjs.com/package/@toon-format/cli):

```sh
npm i -g @toon-format/cli
orchard-tui --json | jq '.repos' | toon
```

`--schema` emits the JSON Schema for `--json` output — field names
(post-camelCase), enum string values, and nullability. Agents should read
it before writing jq filters so they target the real wire format instead
of guessing from Rust types:

```sh
orchard-tui --schema | jq '.properties | keys'
# ["hosts", "repos", "tmuxSessions", "version"]
```

The schema is generated at build time from the same types the binary
serializes, and CI fails if the committed `crates/orchard/schema.json`
drifts from the live types.

### Keybindings

| Key | Action |
|-----|--------|
| `↑/↓` or `j/k` | Navigate worktrees |
| `←/→` | Switch between repos |
| `Enter` | Switch to tmux session (creates one if needed) |
| `1-9` | Jump to worktree by number |
| `o` | Open PR in browser |
| `i` | Open issue in browser |
| `p` | Toggle priority flag |
| `d` | Delete worktree |
| `n` | New worktree |
| `t` | Transfer worktree (push/pull to remote) |
| `c` | Cleanup stale worktrees |
| `f` | Cycle filter mode |
| `/` | Search |
| `r` | Refresh |
| `R` | Reconnect (SSH) |
| `?` | Help |
| Mouse click | Select worktree |
| Mouse scroll | Navigate list |
| `q` | Quit |

## Configuration

### Global config

`~/.orchard/config.json` — register multiple repos:

```json
{
  "repos": [
    {
      "slug": "owner/repo",
      "path": "/path/to/repo",
      "remotes": [
        {
          "name": "gpu",
          "host": "user@server",
          "path": "/remote/path",
          "shell": "ssh"
        }
      ]
    }
  ],
  "tmux_sessions": [
    {
      "name": "shepherd",
      "command": "claude --continue",
      "cwd": "/path/to/repo",
      "start_on_launch": true
    }
  ]
}
```

If no global config exists, orchard auto-detects the current repo via `gh repo view`.

### Per-repo config

`.orchard.json` in the repo root (committable, team-shared):

```json
{
  "ci": {
    "ignore": ["codecov/patch", "deploy-preview"],
    "required": ["test", "build"]
  }
}
```

`.git/orchard.json` for local-only overrides (remotes, personal preferences).

See [ADR-003](docs/adr/003-per-repo-config.md) for the full design.

## The Orchardist (optional)

> **Optional power-user workflow.** Orchard is a fully functional worktree, PR, and session dashboard on its own. The orchardist is an advanced layer on top — skip this section unless you want it.

The **orchardist** is a persistent Claude Code session attached to a repo that acts like an always-on engineering lead: it reviews PRs, launches worktrees for issues, drives sessions to green, and keeps the orchard tidy. It lives in a dedicated tmux session that orchard starts and reconnects to automatically, so it's always one keystroke away from the dashboard.

### Easy setup

The recommended path is automated — no JSON editing required:

- From a Claude Code session inside the repo, run `/install-orchard` and follow the prompts, **or**
- Re-run `orchard-tui init`, which can offer to configure an orchardist for you.

Either option writes the right `tmux_sessions` entry into your config and starts the session.

### Using it

Once configured, the orchardist shows up as a row in the orchard dashboard:

- Press **Enter** on the orchardist row to jump straight into its tmux session.
- Delegate work with slash commands:
  - `/launch <issue>` — create a worktree and Claude session for a GitHub issue
  - `/drive-pr <pr>` — loop a PR to green (fix CI, address review comments)
  - `/orchard-view` — dashboard view of active worktrees
  - `/prune` — clean up stale worktrees and dead sessions
  - `/recover` — self-heal after a reboot or crash

### Manual config (reference)

If you'd rather configure it by hand, add a `tmux_sessions` entry to `~/.orchard/config.json`:

```json
"tmux_sessions": [
  {
    "name": "shepherd",
    "command": "claude --continue",
    "cwd": "/path/to/repo",
    "start_on_launch": true
  }
]
```

- `name` — tmux session name shown in the dashboard.
- `command` — command to run in the session (`claude --continue` resumes the last conversation).
- `cwd` — working directory for the session (usually the repo root).
- `start_on_launch` — if `true`, orchard starts the session automatically when it launches.

This is the same snippet the automated setup writes for you; edit it only if you want to customize what the skill produces.

## Claude Code Integration

Orchard detects Claude session state via hooks (not terminal scraping):

```bash
orchard-tui init  # installs hooks automatically
```

The hooks write structured JSON on every Claude event (tool use, stop, notification). Orchard reads these for accurate working/idle/input detection, plus context window usage and cost tracking.

## Architecture & deeper docs

See [docs/architecture.md](docs/architecture.md) for the full architecture guide and the **Functional Core, Imperative Shell** breakdown (source modules → `build_state()` → TUI / `--json`).

Contributor references:

- [docs/adr/](docs/adr/) — 23 Architecture Decision Records, including [ADR-003](docs/adr/003-per-repo-config.md) (per-repo config) and [ADR-013](docs/adr/013-orchard-cli-ecosystem.md) (CLI ecosystem design)
- [RULES.md](RULES.md) — the repo constitution
- [specs/features/](specs/features/) — BDD feature specs

## Requirements

- Git
- tmux (≥ 3.4 for `orchard shell`)
- [GitHub CLI](https://cli.github.com/) (`gh`) — for PR/issue data
- [terminal-notifier](https://github.com/julienXX/terminal-notifier) — for click-to-switch notifications (optional, falls back to osascript)
- Prebuilt install covers `linux/arm64`, `linux/amd64`, `darwin/arm64`, `darwin/amd64`

## License

MIT
