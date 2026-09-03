#!/usr/bin/env bash
# orchard-sidebar.tmux — TPM entrypoint. Binds a toggle key for the sidebar pane
# so users never hand-edit tmux.conf beyond the standard @plugin line:
#   set -g @plugin 'drewdrewthis/orchardist'
#
# Options (set -g @orchard_sidebar_key 'o'):
#   @orchard_sidebar_key    prefix key to toggle          (default: o)
#   @orchard_sidebar_width  pane width in columns         (default: 42)
#   @orchard_sidebar_auto   auto-open in new sessions     (default: on; set 'off' to disable)

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

get_opt() { tmux show-option -gqv "$1"; }

key="$(get_opt @orchard_sidebar_key)"
key="${key:-o}"

tmux bind-key "$key" run-shell "$CURRENT_DIR/scripts/sidebar-toggle.sh"

auto="$(get_opt @orchard_sidebar_auto)"
if [ "${auto:-on}" != "off" ]; then
  tmux set-hook -g session-created "run-shell '$CURRENT_DIR/scripts/sidebar-open.sh #{q:hook_session_name}'"
fi
