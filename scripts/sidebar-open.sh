#!/usr/bin/env bash
# sidebar-open.sh [target-session] — open the orchard sidebar in a session's
# current window. Idempotent: no-op if that window already has a sidebar pane.
# Used by the session-created hook (auto-open) and by sidebar-toggle.sh.

set -u

target="${1:-}"
width="$(tmux show-option -gqv @orchard_sidebar_width)"
width="${width:-42}"

bin="$(command -v orchard-sidebar || true)"
if [ -z "$bin" ]; then
  repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  [ -x "$repo_dir/bin/orchard-sidebar" ] && bin="$repo_dir/bin/orchard-sidebar"
fi
[ -z "$bin" ] && exit 0

win="${target:+$target:}"
existing="$(tmux list-panes ${target:+-t "$win"} -F '#{pane_id} #{pane_start_command}' 2>/dev/null \
  | awk '/orchard-sidebar/ {print $1; exit}')"
[ -n "$existing" ] && exit 0

if [ -n "$target" ]; then
  tmux split-window -hb -d -l "$width" -t "$win" "$bin"
else
  tmux split-window -hb -l "$width" "$bin"
  tmux select-pane -t '{right}'
fi
