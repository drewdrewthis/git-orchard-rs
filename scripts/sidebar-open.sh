#!/usr/bin/env bash
# sidebar-open.sh [target-session] — open the orchard sidebar in a session's
# current window. Idempotent: no-op if that window already has a sidebar pane.
# Used by the session-created hook (auto-open) and by sidebar-toggle.sh.

set -u

target="${1:-}"
width="$(tmux show-option -gqv @orchard_sidebar_width)"
width="${width:-42}"
# floor: below this the sidebar drops to name-only compact mode (minWidth in
# cmd/orchard-sidebar/main.go), so never open one narrower than it.
case "$width" in
  ''|*[!0-9]*) width=42 ;;
  *) [ "$width" -lt 34 ] && width=34 ;;
esac

bin="$(command -v orchard-sidebar || true)"
if [ -z "$bin" ]; then
  repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  [ -x "$repo_dir/bin/orchard-sidebar" ] && bin="$repo_dir/bin/orchard-sidebar"
fi
[ -z "$bin" ] && exit 0

win="${target:+$target:}"
existing="$(tmux list-panes ${target:+-t "$win"} -F '#{pane_id} #{pane_start_command}' 2>/dev/null \
  | awk '/orchard-sidebar/ {print $1; exit}')"
if [ -n "$existing" ]; then
  # already open: heal a pane that got squeezed under the floor (divider drag,
  # a later split, or a pane opened before the floor existed)
  cur="$(tmux display-message -p -t "$existing" '#{pane_width}' 2>/dev/null || echo "$width")"
  case "$cur" in
    ''|*[!0-9]*) : ;;
    *) [ "$cur" -lt "$width" ] && tmux resize-pane -t "$existing" -x "$width" ;;
  esac
  exit 0
fi

if [ -n "$target" ]; then
  tmux split-window -hb -d -l "$width" -t "$win" "$bin"
else
  tmux split-window -hb -l "$width" "$bin"
  tmux select-pane -t '{right}'
fi
