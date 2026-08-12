#!/usr/bin/env bash
# sidebar-toggle.sh — toggle the orchard sidebar pane in the current tmux window.
# Left pane, fixed width. Idempotent: running sidebar in this window → kill it.

set -u

# already open in this window? (match by pane command, positional-safe)
existing="$(tmux list-panes -F '#{pane_id} #{pane_start_command}' \
  | awk -v b="orchard-sidebar" '$0 ~ b {print $1; exit}')"

if [ -n "$existing" ]; then
  tmux kill-pane -t "$existing"
else
  if ! command -v orchard-sidebar >/dev/null 2>&1 \
     && [ ! -x "$(dirname "${BASH_SOURCE[0]}")/../bin/orchard-sidebar" ]; then
    tmux display-message "orchard-sidebar not found — run 'orchard init'"
    exit 0
  fi
  exec "$(dirname "${BASH_SOURCE[0]}")/sidebar-open.sh"
fi
