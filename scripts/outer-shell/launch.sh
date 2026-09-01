#!/usr/bin/env bash
# launch.sh — Boot (or attach to) the outer tmux wrapper around a nested
# inner tmux session. Issue #747 spike: proves outer-wraps-inner tmux
# nesting with shell + tmux only, no daemon/client changes.
#
# Usage: launch.sh INNER_SOCKET SESSION
#
# INNER_SOCKET — the `-L` socket name of the already-running inner tmux
#                server (e.g. the daemon's per-worktree tmux server).
# SESSION      — session name inside the inner server to attach the right
#                pane to.
#
# Env:
#   OUTER_SOCKET — outer server `-L` socket name. Default: orchard-shell.
#                  Override lets verify.sh drive an isolated test instance
#                  without touching the real outer-shell socket.
#
# Idempotent: if the outer session is already up, this just attaches.
# Nothing here is a daemon mutation — see docs/outer-shell-prototype.md
# for the ADR-016 follow-up (switch-client/popup as GraphQL mutations).
set -euo pipefail

INNER_SOCKET="${1:?usage: launch.sh INNER_SOCKET SESSION}"
SESSION="${2:?usage: launch.sh INNER_SOCKET SESSION}"
OUTER_SOCKET="${OUTER_SOCKET:-orchard-shell}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
OUTER_CONF="$SCRIPT_DIR/outer.conf"
OUTER_SESSION="shell"

# tmux -L alone does not suppress loading ~/.tmux.conf — -f must always be
# passed explicitly, or this server silently inherits the user's real config.
outer() { tmux -L "$OUTER_SOCKET" -f "$OUTER_CONF" "$@"; }

if ! outer has-session -t "$OUTER_SESSION" 2>/dev/null; then
  if ! tmux -L "$INNER_SOCKET" has-session -t "$SESSION" 2>/dev/null; then
    echo "error: inner session '$SESSION' not found on socket '$INNER_SOCKET'" >&2
    exit 1
  fi

  COLS="$(tput cols 2>/dev/null || echo 160)"
  ROWS="$(tput lines 2>/dev/null || echo 45)"

  outer new-session -d -s "$OUTER_SESSION" -x "$COLS" -y "$ROWS"

  # split-window -h -b -l 40: new pane goes before (-b, i.e. left of) the
  # target, exact width 40. The NEW pane becomes 0.0 (left); the pane that
  # existed before the split becomes 0.1 (right).
  #
  # Split MUST happen before either send-keys call. Sending a command to
  # "0.0" before the split targets the pre-split sole pane — which the
  # split then renumbers to 0.1 — so a command sent to "0.0" first and a
  # command sent to "0.1" second both land in the SAME physical pane (the
  # original one), while the true post-split 0.0 gets nothing. That was
  # exactly the bug: watch ended up in 0.1 with the attach keystrokes typed
  # into its already-running watch(1) TUI and swallowed, never executed.
  outer split-window -h -b -l 40 -t "$OUTER_SESSION:0"

  outer send-keys -t "$OUTER_SESSION:0.0" \
    "watch -n1 \"tmux -L $INNER_SOCKET list-windows -a\"" Enter

  # TMUX= clears the outer session's own $TMUX before exec'ing the inner
  # attach. Without it tmux hard-refuses to nest: "sessions should be
  # nested with care, unset $TMUX to force" — the attach never connects,
  # this is not a cosmetic warning.
  outer send-keys -t "$OUTER_SESSION:0.1" \
    "TMUX= tmux -L $INNER_SOCKET attach -t $SESSION" Enter
fi

exec tmux -L "$OUTER_SOCKET" -f "$OUTER_CONF" attach -t "$OUTER_SESSION"
