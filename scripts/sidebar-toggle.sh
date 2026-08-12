#!/usr/bin/env bash
# sidebar-toggle.sh — toggle the orchard sidebar pane in the current tmux window.
# Left pane, fixed width. Idempotent: running sidebar in this window → kill it.

set -u

width="$(tmux show-option -gqv @orchard_sidebar_width)"
width="${width:-42}"

# find the sidebar binary: PATH first, then a repo-local build
bin="$(command -v orchard-sidebar || true)"
if [ -z "$bin" ]; then
  repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  [ -x "$repo_dir/bin/orchard-sidebar" ] && bin="$repo_dir/bin/orchard-sidebar"
fi
if [ -z "$bin" ]; then
  tmux display-message "orchard-sidebar not found — run 'orchard init' or 'make sidebar'"
  exit 0
fi

# already open in this window? (match by pane command/title, positional-safe)
existing="$(tmux list-panes -F '#{pane_id} #{pane_start_command}' \
  | awk -v b="orchard-sidebar" '$0 ~ b {print $1; exit}')"

if [ -n "$existing" ]; then
  tmux kill-pane -t "$existing"
else
  tmux split-window -hb -l "$width" "$bin"
  tmux select-pane -t '{right}'
fi
