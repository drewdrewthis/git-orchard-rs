# git-orchard 🌲🌳🌴

Interactive TUI for managing git worktrees, PR status, and tmux sessions.

Built with [Rust](https://www.rust-lang.org/) + [Ratatui](https://ratatui.rs/).

![License](https://img.shields.io/badge/license-MIT-blue)

## Features

- **Worktree management** — list, create, delete git worktrees
- **PR status** — see GitHub PR review status, CI checks, merge conflicts at a glance
- **tmux integration** — switch into worktree sessions, preview pane output
- **Remote worktrees** — push/pull worktrees between local and remote machines via SSH
- **Cleanup** — bulk-delete worktrees with merged/closed PRs
- **Progressive loading** — data appears as it arrives (git → tmux → PRs → enrichment)
- **Auto-refresh** — updates every 60 seconds

## Install

### From source (requires Rust toolchain)

```bash
cargo install --git https://github.com/drewdrewthis/git-orchard-rs
```

### From releases

Download the binary for your platform from [GitHub Releases](https://github.com/drewdrewthis/git-orchard-rs/releases/latest).

## Setup

git-orchard works best with a persistent tmux session. Add the shell function to your rc file:

```bash
git-orchard init
# Follow the printed instructions to add the orchard() function to ~/.zshrc or ~/.bashrc
```

Then use `orchard` to launch.

## Usage

```
$ git-orchard              Interactive worktree manager
$ git-orchard init         Print shell function for tmux integration
$ git-orchard cleanup      Find worktrees with merged PRs to remove
$ git-orchard --json       Output worktree data as JSON
```

### Keybindings

| Key | Action |
|-----|--------|
| `1-9` | Jump to worktree by number |
| `↑/↓` or `j/k` | Navigate |
| `Enter` or `t` | Switch to tmux session |
| `o` | Open PR in browser |
| `p` | Push/pull to remote |
| `d` | Delete worktree |
| `c` | Cleanup merged worktrees |
| `r` | Refresh |
| `q` | Quit |

## Configuration

Create `.git/orchard.json` in your repo to configure remote worktree support:

```json
{
  "remote": {
    "host": "user@server.example.com",
    "repoPath": "/home/user/projects/my-repo",
    "shell": "ssh"
  }
}
```

## Requirements

- Git
- tmux (for session management)
- [GitHub CLI](https://cli.github.com/) (`gh`) — for PR status (optional)

## License

MIT
