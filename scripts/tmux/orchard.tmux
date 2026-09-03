#!/usr/bin/env bash
# orchard.tmux — tmux-plugin entry point for the orchard pane picker.
#
# tmux runs this once at config load. It owns its own wiring: it installs
# the `prefix + <key>` binding that refreshes @orchard_pane_label and then
# opens an expanded choose-tree rendering those labels. Nothing needs to be
# pasted into tmux.conf beyond the single run-shell line, and no copy of
# the helper needs to live in ~/.local/bin.
#
# Install (in-repo):
#   run-shell '/path/to/git-orchard-rs/scripts/tmux/orchard.tmux'
#
# Install (TPM, if this directory is ever split into its own repo):
#   set -g @plugin 'drewdrewthis/orchard.tmux'
#
# Configuration — tmux user options, read at load time:
#   @orchard_daemon_url    full GraphQL endpoint  (default http://127.0.0.1:7777/graphql)
#   @orchard_key           prefix key to bind     (default s)
#   @orchard_heartbeat_dir Claude hook state dir  (default: helper's own
#                          $ORCHARD_HEARTBEAT_DIR / $TMPDIR / /tmp resolution)
#
# Flags:
#   --verbose   print the resolved configuration and the binding installed.
#               Silent otherwise, so tmux does not pop a window at config load.
set -euo pipefail

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LABELER="$CURRENT_DIR/pane-labels.sh"

VERBOSE="${ORCHARD_TMUX_VERBOSE:-0}"
if [[ "${1:-}" == "--verbose" ]]; then
  VERBOSE=1
fi

# choose-tree row format. Rows are heterogeneous, so the format branches on
# row type: panes render @orchard_pane_label (falling back to command+path
# when the labeler has not run yet), windows render #W, sessions render #S.
FORMAT='#{?pane_format,#{?@orchard_pane_label,#{E:@orchard_pane_label},#{pane_current_command}  #{b:pane_current_path}},#{?window_format,#{?window_active,▶,◼} #W,#{?session_attached,● ,○ }#S}}'

tmux_option() {
  local name="$1" default="$2" value
  value="$(tmux show-option -gqv "$name" 2>/dev/null || true)"
  if [[ -z "$value" ]]; then
    printf '%s' "$default"
  else
    printf '%s' "$value"
  fi
}

# The bound command is a two-command sequence, so it must be handed to
# bind-key as ONE tmux command string: passing `run-shell ... \; choose-tree
# ...` as separate argv elements makes tmux's own top-level parser eat the
# `;` and run choose-tree immediately instead of binding it.
#
# That means THREE nested layers, not two:
#
#   1. tmux's command parser reads the bound string. It must use SINGLE quotes
#      — a tmux double-quoted string expands `#{...}`, which would evaluate the
#      choose-tree format at bind time.
#   2. run-shell FORMAT-EXPANDS its command before running it. Measured on
#      tmux 3.6a: `run-shell -b 'touch "/tmp/x#{session_name}"'` creates
#      /tmp/xNAME, not a literal. So a `#` in any interpolated path or URL is
#      a live format directive here and must be doubled.
#   3. /bin/sh, which run-shell invokes, so paths with spaces need sh double
#      quotes.
#
# Layer 2 was missing from this comment, and from the code — which is
# plausibly how the label-side format injection (finding 1) went unnoticed.
sh_dquote() {
  printf '"%s"' "$(printf '%s' "$1" | sed 's/[\\"$`]/\\&/g')"
}

# Layer 2's escape: tmux formats have no escape but doubling `#`.
tmux_fmt_escape() {
  printf '%s' "${1//#/##}"
}

# A literal single quote cannot appear inside a tmux single-quoted string, and
# tmux offers no escape for it there, so a value carrying one cannot be wired
# into the bound command at all. That disables the LABELER, not the picker:
# reporting it and binding the picker alone is the degradation the README
# promises. Exiting here instead would leave the user with no binding.
has_squote() {
  case "$2" in
    *\'*)
      echo "orchard.tmux: $1 contains a single quote, which tmux cannot quote here; labels disabled: $2" >&2
      return 0
      ;;
  esac
  return 1
}

DAEMON_URL="$(tmux_option '@orchard_daemon_url' 'http://127.0.0.1:7777/graphql')"
KEY="$(tmux_option '@orchard_key' 's')"
HEARTBEAT_DIR="$(tmux_option '@orchard_heartbeat_dir' '')"

# Every reason the labeler cannot be wired up is collected before binding, so
# the user sees all of them in one pass rather than one per reload.
LABELS_OK=1
if [[ ! -x "$LABELER" ]]; then
  # Packaging error, not a runtime one.
  echo "orchard.tmux: helper not executable: $LABELER" >&2
  LABELS_OK=0
fi
# Written as `if` rather than `cmd && LABELS_OK=0` so the no-quote case (a
# non-zero return) cannot be read as a `set -e` trap by the next reader.
if has_squote "labeler path" "$LABELER"; then LABELS_OK=0; fi
if has_squote "@orchard_daemon_url" "$DAEMON_URL"; then LABELS_OK=0; fi
if has_squote "@orchard_heartbeat_dir" "$HEARTBEAT_DIR"; then LABELS_OK=0; fi

PICKER="choose-tree -Zs -F '$FORMAT'"

# tmux keeps a binding until something removes it, so moving @orchard_key
# would otherwise leave the picker on BOTH keys with no way back to the old
# one short of a manual unbind or a server restart. Remember which key this
# plugin took and vacate it first. The vacated key is left unbound rather
# than restored to its stock binding — this plugin overwrote that binding and
# does not know what it was.
PREV_KEY="$(tmux show-option -gqv '@orchard_bound_key' 2>/dev/null || true)"
if [[ -n "$PREV_KEY" && "$PREV_KEY" != "$KEY" ]]; then
  tmux unbind-key -T prefix "$PREV_KEY" 2>/dev/null || true
fi

if [[ "$LABELS_OK" == "1" ]]; then
  LABEL_CMD="$(sh_dquote "$LABELER") --daemon-url $(sh_dquote "$DAEMON_URL")"
  if [[ -n "$HEARTBEAT_DIR" ]]; then
    LABEL_CMD="$LABEL_CMD --heartbeat-dir $(sh_dquote "$HEARTBEAT_DIR")"
  fi
  # Applied to the whole assembled command: every `#` in it came from an
  # interpolated value, since nothing this script writes contains one.
  LABEL_CMD="$(tmux_fmt_escape "$LABEL_CMD")"
  # -b backgrounds the refresh: choose-tree opens immediately on the
  # previous labels rather than blocking on the daemon round-trip. A daemon
  # that is down or slow therefore costs the picker nothing — the helper
  # degrades to empty results on its own.
  tmux bind-key -T prefix "$KEY" "run-shell -b '$LABEL_CMD' ; $PICKER"
else
  # Still bind the picker so the key keeps working; labels just fall back to
  # command + path, per the README's Degradation section.
  tmux bind-key -T prefix "$KEY" "$PICKER"
fi

tmux set-option -g '@orchard_bound_key' "$KEY"

if [[ "$VERBOSE" == "1" ]]; then
  echo "orchard.tmux: dir=$CURRENT_DIR"
  echo "orchard.tmux: labeler=$LABELER"
  echo "orchard.tmux: @orchard_daemon_url=$DAEMON_URL"
  echo "orchard.tmux: @orchard_key=$KEY"
  echo "orchard.tmux: @orchard_heartbeat_dir=${HEARTBEAT_DIR:-<helper default>}"
  tmux list-keys -T prefix "$KEY"
fi
