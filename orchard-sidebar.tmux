#!/usr/bin/env bash
# orchard-sidebar.tmux — TPM entrypoint. Binds a toggle key for the sidebar pane
# so users never hand-edit tmux.conf beyond the standard @plugin line:
#   set -g @plugin 'drewdrewthis/orchardist'
#
# Options (set -g @orchard_sidebar_key 'o'):
#   @orchard_sidebar_key    prefix key to toggle   (default: o)
#   @orchard_sidebar_width  pane width in columns  (default: 42)

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

get_opt() { tmux show-option -gqv "$1"; }

key="$(get_opt @orchard_sidebar_key)"
key="${key:-o}"

tmux bind-key "$key" run-shell "$CURRENT_DIR/scripts/sidebar-toggle.sh"
