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
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." >/dev/null 2>&1 && pwd)"
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

  # TMUX= clears the outer session's own $TMUX before exec'ing the inner
  # attach. Without it tmux hard-refuses to nest: "sessions should be
  # nested with care, unset $TMUX to force" — the attach never connects,
  # this is not a cosmetic warning.
  outer send-keys -t "$OUTER_SESSION:0.1" \
    "TMUX= tmux -L $INNER_SOCKET attach -t $SESSION" Enter

  # The inner attach above runs ITS tmux client on pane 0.1's own pty — same
  # device, now also registered as a client on the INNER server — so that
  # client's #{client_tty} (as the inner server sees it) equals outer's 0.1
  # #{pane_tty} here. Resolved after the attach is sent so pane 0.0's
  # sidebar can be told exactly which inner client is "this wrapper's own"
  # (ORCHARD_TMUX_CLIENT, below) instead of guessing from activity.
  INNER_TTY="$(outer display -p -t "$OUTER_SESSION:0.1" '#{pane_tty}')"

  # Outer 0.1's own pane id (e.g. %1) — stable across resizes/redraws, unlike
  # a tty path. Handed to the sidebar as ORCHARD_OUTER_PANE so it can give
  # focus back to this pane (the inner client) on the OUTER server after a
  # switch-client it drove on the INNER one; see main.go's outerCmd/
  # handBackFocus and docs/outer-shell-prototype.md, "Sidebar focus
  # hand-back".
  OUTER_PANE_ID="$(outer display -p -t "$OUTER_SESSION:0.1" '#{pane_id}')"

  # Pane 0.0 runs the real sidebar when a binary can be found —
  # ORCHARD_TMUX_SOCKET tells its tmux execs (switch-client, list-clients,
  # list-panes, width sync) to target the INNER server instead of the outer
  # one they'd otherwise resolve to by virtue of running as an outer-server
  # pane's command, ORCHARD_TMUX_CLIENT scopes switch-client/list-clients to
  # the inner client on THIS wrapper's own pane 0.1 — on a shared inner
  # server, an unscoped switch-client lets tmux pick an arbitrary attached
  # client to move instead (#747 defect 2) — and ORCHARD_OUTER_PANE (above)
  # is where the sidebar hands keyboard focus back to after driving that
  # switch. See docs/outer-shell-prototype.md, "Routing orchard-sidebar's
  # own tmux execs" and "Sidebar focus hand-back".
  #
  # Resolved to an absolute path rather than passed as a bare command name:
  # a bare `orchard-sidebar` re-resolves against the pane shell's own PATH
  # at exec time, which can silently pick a stale build instead of the one
  # this launch actually meant. $REPO_ROOT/bin/orchard-sidebar (the local
  # build output) wins when present; otherwise whatever `command -v` finds
  # on PATH. Falls back to the watch(1) placeholder when neither exists.
  if [[ -x "$REPO_ROOT/bin/orchard-sidebar" ]]; then
    SIDEBAR_BIN="$REPO_ROOT/bin/orchard-sidebar"
  else
    SIDEBAR_BIN="$(command -v orchard-sidebar || true)"
  fi

  if [[ -n "$SIDEBAR_BIN" ]]; then
    outer send-keys -t "$OUTER_SESSION:0.0" \
      "ORCHARD_TMUX_SOCKET=$INNER_SOCKET ORCHARD_TMUX_CLIENT=$INNER_TTY ORCHARD_OUTER_PANE=$OUTER_PANE_ID $SIDEBAR_BIN" Enter
  else
    outer send-keys -t "$OUTER_SESSION:0.0" \
      "watch -n1 \"tmux -L $INNER_SOCKET list-windows -a\"" Enter
  fi
fi

# Focus 0.1 (the inner client), not 0.0 (the newly-split pane tmux leaves
# active by default) — with mouse on but no prefix, a user landing on 0.0
# had no way to move focus at all (#747 live defect). Runs unconditionally,
# after the boot block above AND on every idempotent re-run, so a client
# that attaches via the exec below always lands able to type into the inner
# shell immediately.
outer select-pane -t "$OUTER_SESSION:0.1"

exec tmux -L "$OUTER_SOCKET" -f "$OUTER_CONF" attach -t "$OUTER_SESSION"
