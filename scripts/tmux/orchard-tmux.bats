#!/usr/bin/env bats
# Wiring assertions for tmux/orchard.tmux — the plugin entry script.
#
# Each test runs the script against a throwaway tmux server on its own
# socket, started with `-f /dev/null` so the developer's real tmux.conf and
# the live prefix bindings are never read or written. Isolation is done with
# a `tmux` shim on PATH, so the script under test runs completely unmodified.

setup() {
  if ! command -v tmux >/dev/null 2>&1; then
    skip "tmux not available"
  fi
  SCRIPT="$BATS_TEST_DIRNAME/orchard.tmux"
  TMPD="$(mktemp -d)"
  # Socket lives inside the per-test tmpdir so teardown reclaims it; a named
  # socket (-L) would leave a dead file behind in the shared tmux dir per run.
  SOCK="$TMPD/tmux.sock"
  REAL_TMUX="$(command -v tmux)"

  mkdir -p "$TMPD/bin"
  cat > "$TMPD/bin/tmux" <<EOF
#!/bin/sh
exec "$REAL_TMUX" -S "$SOCK" -f /dev/null "\$@"
EOF
  chmod +x "$TMPD/bin/tmux"

  "$TMPD/bin/tmux" new-session -d -s harness
}

teardown() {
  if [ -n "${TMPD:-}" ] && [ -x "$TMPD/bin/tmux" ]; then
    "$TMPD/bin/tmux" kill-server 2>/dev/null || true
  fi
  [ -n "${TMPD:-}" ] && rm -rf "$TMPD"
}

_run_plugin() {
  PATH="$TMPD/bin:$PATH" run bash "$SCRIPT" "$@"
}

_binding_for() {
  PATH="$TMPD/bin:$PATH" tmux list-keys -T prefix "$1" 2>/dev/null
}

_set_opt() {
  PATH="$TMPD/bin:$PATH" tmux set-option -g "$1" "$2"
}

@test "installs a prefix binding that refreshes labels then opens choose-tree" {
  _run_plugin
  [ "$status" -eq 0 ]

  binding="$(_binding_for s)"
  [[ "$binding" == *"pane-labels.sh"* ]]
  [[ "$binding" == *"choose-tree"* ]]
}

@test "binding resolves the helper beside the plugin, not a fixed ~/.local/bin copy" {
  _run_plugin
  [ "$status" -eq 0 ]

  binding="$(_binding_for s)"
  [[ "$binding" == *"$BATS_TEST_DIRNAME/pane-labels.sh"* ]]
  [[ "$binding" != *".local/bin"* ]]
}

@test "choose-tree format renders @orchard_pane_label on pane rows" {
  _run_plugin
  [ "$status" -eq 0 ]

  binding="$(_binding_for s)"
  [[ "$binding" == *"@orchard_pane_label"* ]]
  [[ "$binding" == *"pane_format"* ]]
}

@test "@orchard_key moves the picker to another prefix key" {
  _set_opt "@orchard_key" "g"
  _run_plugin
  [ "$status" -eq 0 ]

  [[ "$(_binding_for g)" == *"choose-tree"* ]]
}

@test "@orchard_daemon_url is passed through to the helper" {
  _set_opt "@orchard_daemon_url" "http://10.0.0.5:8080/graphql"
  _run_plugin
  [ "$status" -eq 0 ]

  [[ "$(_binding_for s)" == *"http://10.0.0.5:8080/graphql"* ]]
}

@test "daemon url defaults to the local daemon when the option is unset" {
  _run_plugin
  [ "$status" -eq 0 ]

  [[ "$(_binding_for s)" == *"http://127.0.0.1:7777/graphql"* ]]
}

@test "@orchard_heartbeat_dir is forwarded only when set" {
  _run_plugin
  [ "$status" -eq 0 ]
  [[ "$(_binding_for s)" != *"--heartbeat-dir"* ]]

  _set_opt "@orchard_heartbeat_dir" "/tmp/orchard-bats-hb"
  _run_plugin
  [ "$status" -eq 0 ]
  [[ "$(_binding_for s)" == *"--heartbeat-dir"* ]]
  [[ "$(_binding_for s)" == *"/tmp/orchard-bats-hb"* ]]
}

@test "missing helper still binds the picker so the key keeps working" {
  cp "$SCRIPT" "$TMPD/orchard.tmux"
  chmod +x "$TMPD/orchard.tmux"

  PATH="$TMPD/bin:$PATH" run bash "$TMPD/orchard.tmux"
  [ "$status" -eq 0 ]
  [[ "$output" == *"helper not executable"* ]]

  binding="$(_binding_for s)"
  [[ "$binding" == *"choose-tree"* ]]
  [[ "$binding" != *"pane-labels.sh"* ]]
}

@test "re-running the plugin is idempotent" {
  _run_plugin
  first="$(_binding_for s)"
  _run_plugin
  second="$(_binding_for s)"

  [ "$first" = "$second" ]
}

# --- degradation, not death (review finding 5) -----------------------------
#
# A literal single quote cannot appear inside a tmux single-quoted string and
# tmux offers no escape for it there, so the labeler cannot be wired up. That
# is a reason to install the picker WITHOUT the labeler — the degradation the
# README promises — not a reason to install nothing.

@test "apostrophe in the daemon url still binds the picker" {
  _set_opt "@orchard_daemon_url" "http://o'brien.example/graphql"
  _run_plugin
  [ "$status" -eq 0 ]
  [[ "$output" == *"single quote"* ]]

  binding="$(_binding_for s)"
  [[ "$binding" == *"choose-tree"* ]]
  [[ "$binding" != *"pane-labels.sh"* ]]
}

@test "apostrophe in the heartbeat dir still binds the picker" {
  _set_opt "@orchard_heartbeat_dir" "/Users/O'Brien/tmp"
  _run_plugin
  [ "$status" -eq 0 ]
  [[ "$output" == *"single quote"* ]]
  [[ "$(_binding_for s)" == *"choose-tree"* ]]
}

@test "apostrophe in the labeler path still binds the picker" {
  # Copy the whole plugin into a path containing an apostrophe so the script
  # resolves a labeler it cannot quote, rather than one that is merely absent.
  mkdir -p "$TMPD/O'Brien"
  cp "$SCRIPT" "$BATS_TEST_DIRNAME/pane-labels.sh" "$TMPD/O'Brien/"
  chmod +x "$TMPD/O'Brien/orchard.tmux" "$TMPD/O'Brien/pane-labels.sh"

  PATH="$TMPD/bin:$PATH" run bash "$TMPD/O'Brien/orchard.tmux"
  [ "$status" -eq 0 ]
  [[ "$output" == *"single quote"* ]]
  [[ "$(_binding_for s)" == *"choose-tree"* ]]
}
