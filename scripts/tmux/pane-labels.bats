#!/usr/bin/env bats
# Label composition for tmux/pane-labels.sh.
#
# Every test drives the script through --print, so no tmux server is needed
# and the assertions are made on the exact string that would be written to
# @orchard_pane_label. The daemon is faked at the wire (testdata/fake-daemon.py)
# or simply unreachable — nothing this repo owns is stubbed.

setup() {
  SCRIPT="$BATS_TEST_DIRNAME/pane-labels.sh"
  TESTDATA="$BATS_TEST_DIRNAME/testdata"
  TMPD="$(mktemp -d)"
  HOOKS="$TMPD/hooks"
  mkdir -p "$HOOKS"
  # Port 9 (discard) refuses TCP connections, so this is a genuine
  # unreachable daemon rather than a simulated failure.
  UNREACHABLE="http://127.0.0.1:9/graphql"
}

teardown() {
  if [ -n "${FAKE_PID:-}" ]; then
    kill "$FAKE_PID" 2>/dev/null || true
    wait "$FAKE_PID" 2>/dev/null || true
  fi
  [ -n "${TMPD:-}" ] && rm -rf "$TMPD"
}

# Writes a pane table row in the layout pane-labels.sh expects:
# pane_id, session, window_index, pane_index, current_path, current_command.
_pane_row() {
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "0" "0" "$3" "$4"
}

# Writes a hook state file in the exact shape ~/.claude/hooks/orchard-state.sh
# emits: {state, session_id, tmux_session, cwd, event, timestamp}.
_hook_state() {
  local session="$1" state="$2" cwd="$3"
  cat > "$HOOKS/orchard-claude-${session}.json" <<EOF
{
  "state": "$state",
  "session_id": "00000000-0000-0000-0000-000000000000",
  "tmux_session": "$session",
  "cwd": "$cwd",
  "event": "PostToolUse",
  "timestamp": "2026-08-28T12:00:00Z"
}
EOF
}

# Starts the fake daemon on the given canned response and exports FAKE_URL.
_start_fake_daemon_with() {
  local body="$1" portfile="$TMPD/port"
  python3 "$TESTDATA/fake-daemon.py" "$body" "$portfile" &
  FAKE_PID=$!
  local i=0
  while [ ! -s "$portfile" ] && [ "$i" -lt 100 ]; do
    sleep 0.05
    i=$((i + 1))
  done
  [ -s "$portfile" ] || return 1
  FAKE_URL="http://127.0.0.1:$(cat "$portfile")/graphql"
}

# Starts the fake daemon on the ordinary happy-path response.
_start_fake_daemon() {
  _start_fake_daemon_with "$TESTDATA/daemon-response.json"
}

_label_for() {
  # Prints the label column for the given pane id from --print output.
  awk -F'\t' -v id="$1" '$1 == id { print $2 }'
}

@test "hook state present: renders the claude state cell" {
  _hook_state "alpha" "working" "/tmp/orchard-bats-alpha"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha" "zsh" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  [[ "$label" == *"⏺ working"* ]]
}

@test "hook state absent: renders no claude state cell but still labels the pane" {
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha" "zsh" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  [ -n "$label" ]
  [[ "$label" == *"/tmp/orchard-bats-alpha"* ]]
  [[ "$label" == *"⏵ zsh"* ]]
  [[ "$label" != *"working"* ]]
  [[ "$label" != *"idle"* ]]
  [[ "$label" != *"input"* ]]
}

@test "hook state carries no model or context fields: neither is placeholdered" {
  _hook_state "alpha" "idle" "/tmp/orchard-bats-alpha"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha" "zsh" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  [[ "$label" == *"⏸ idle"* ]]
  # The hook payload has no model and no context_window_pct, so nothing
  # stands in for them — no empty cell, no "None", no bare percentage.
  [[ "$label" != *"None"* ]]
  [[ "$label" != *"%"* ]]
}

@test "hook state for another cwd does not stamp an unrelated pane in the same session" {
  _hook_state "alpha" "working" "/tmp/orchard-bats-alpha"
  # Same session name, different directory, not running claude.
  _pane_row "%2" "alpha" "/tmp/orchard-bats-elsewhere" "vim" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%2")"
  [[ "$label" == *"⏵ vim"* ]]
  [[ "$label" != *"working"* ]]
}

@test "hook state reaches a claude pane whose cwd has drifted from the recorded one" {
  _hook_state "alpha" "input" "/tmp/orchard-bats-alpha"
  _pane_row "%3" "alpha" "/tmp/orchard-bats-elsewhere" "claude" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%3")"
  [[ "$label" == *"⌨ input"* ]]
}

@test "unreadable hook state file is skipped rather than failing the run" {
  printf 'not json at all' > "$HOOKS/orchard-claude-alpha.json"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha" "zsh" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  [[ "$label" == *"⏵ zsh"* ]]
}

@test "daemon unreachable: exits 0 and still produces a label for every pane" {
  _pane_row "%1" "alpha" "/tmp/orchard-bats-wt" "zsh" > "$TMPD/panes"
  _pane_row "%2" "beta" "/tmp/orchard-bats-elsewhere" "vim" >> "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [ -n "$(printf '%s\n' "$output" | _label_for "%1")" ]
  [ -n "$(printf '%s\n' "$output" | _label_for "%2")" ]
  # With no daemon there is no worktree data, so the label falls back to the
  # pane's own path rather than branch/repo chrome.
  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"/tmp/orchard-bats-wt"* ]]
}

@test "daemon reachable: label carries the worktree branch and repo slug" {
  _start_fake_daemon
  _pane_row "%1" "alpha" "/tmp/orchard-bats-wt" "zsh" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$FAKE_URL" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  [[ "$label" == *"issue-900/tmux-plugin"* ]]
  [[ "$label" == *"orchardist"* ]]
}

@test "daemon reachable plus hook state: both worktree chrome and claude state render" {
  _start_fake_daemon
  _hook_state "alpha" "working" "/tmp/orchard-bats-wt"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-wt" "zsh" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$FAKE_URL" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  [[ "$label" == *"issue-900/tmux-plugin"* ]]
  [[ "$label" == *"⏺ working"* ]]
}

# --- tmux format-string injection (review finding 1) -----------------------
#
# choose-tree renders the label through `#{E:@orchard_pane_label}`, which
# expands the option value as a tmux format. `#(cmd)` in a format runs `cmd`.
# Every one of these vectors is attacker-influenceable, so none may reach the
# option with a live `#`: tmux's only escape is doubling it to `##`.

# Asserts every `#` in the label is either an escaped `##` pair or one of the
# style cells this script emits itself. Anything left over is a live format
# directive built from untrusted data.
#
# Order matters: collapse the `##` pairs FIRST, because escaped data can end
# in a `#` that would otherwise pair with the `#` opening the next style cell.
_assert_no_live_format() {
  local label="$1" residue
  residue="$(printf '%s' "$label" \
    | sed 's/##//g' \
    | sed 's/#\[fg=[a-z,]*\]//g; s/#\[default\]//g')"
  [[ "$residue" != *'#'* ]]
}

@test "malicious branch name cannot inject a tmux format directive" {
  _start_fake_daemon_with "$TESTDATA/daemon-response-evil.json"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-wt" "zsh" > "$TMPD/panes"
  rm -f "$TMPD/PWNED"

  run bash "$SCRIPT" --daemon-url "$FAKE_URL" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  # The payload is still shown to the user — escaped, as inert literal text.
  [[ "$label" == *'##(touch'* ]]
  _assert_no_live_format "$label"
  [ ! -e "$TMPD/PWNED" ]
}

@test "malicious issue title and PR label cannot inject a tmux format directive" {
  ORCHARD_LABEL_ENRICH=1 _start_fake_daemon_with "$TESTDATA/daemon-response-evil.json"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-wt" "zsh" > "$TMPD/panes"

  ORCHARD_LABEL_ENRICH=1 run bash "$SCRIPT" --daemon-url "$FAKE_URL" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  _assert_no_live_format "$label"
}

@test "malicious hook state value cannot inject a tmux format directive" {
  cat > "$HOOKS/orchard-claude-alpha.json" <<JSON
{
  "state": "#(touch $TMPD/PWNED)",
  "model": "#{E:@orchard_pane_label}",
  "tmux_session": "alpha",
  "cwd": "/tmp/orchard-bats-alpha",
  "event": "PostToolUse"
}
JSON
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha" "claude" > "$TMPD/panes"
  rm -f "$TMPD/PWNED"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  _assert_no_live_format "$label"
  [ ! -e "$TMPD/PWNED" ]
}

@test "malicious pane path cannot inject a tmux format directive" {
  _pane_row "%1" "alpha" '/tmp/#(touch /tmp/orchard-bats-pwn)x' "zsh" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  _assert_no_live_format "$label"
}

# --- degradation edges (review findings 4, 10, 12) -------------------------

@test "HOME unset: still labels every pane instead of crashing" {
  _pane_row "%1" "alpha" "/tmp/orchard-bats-elsewhere" "zsh" > "$TMPD/panes"

  run env -u HOME bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  [[ "$label" == *"/tmp/orchard-bats-elsewhere"* ]]
  [[ "$label" == *"⏵ zsh"* ]]
}

@test "HOME set: the home prefix is still abbreviated to ~" {
  _pane_row "%1" "alpha" "/home/orchard-bats/work" "zsh" > "$TMPD/panes"

  run env HOME=/home/orchard-bats bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"~/work"* ]]
}

@test "HOME empty: the path is left alone rather than shredded" {
  _pane_row "%1" "alpha" "/tmp/orchard-bats-elsewhere" "zsh" > "$TMPD/panes"

  run env HOME= bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  # An empty HOME must not be substituted between every character.
  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"/tmp/orchard-bats-elsewhere"* ]]
}

@test "daemon returns 200 with a non-JSON body: still labels every pane" {
  # A proxy error page or a truncated response passes `curl -sf` (2xx) and
  # then fails to parse — a worse outcome than the handled unreachable case
  # if it is left to raise.
  _start_fake_daemon_with "$TESTDATA/daemon-response-malformed.json"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-wt" "zsh" > "$TMPD/panes"
  _pane_row "%2" "beta" "/tmp/orchard-bats-elsewhere" "vim" >> "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$FAKE_URL" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  # Same fallback as the unreachable daemon: no worktree chrome, but every
  # pane still gets a label from local data.
  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"/tmp/orchard-bats-wt"* ]]
  [[ "$(printf '%s\n' "$output" | _label_for "%2")" == *"⏵ vim"* ]]
}

@test "daemon returns 200 with JSON that is not an object: still labels every pane" {
  printf '[]' > "$TMPD/array-response.json"
  _start_fake_daemon_with "$TMPD/array-response.json"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-wt" "zsh" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$FAKE_URL" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]
  [ -n "$(printf '%s\n' "$output" | _label_for "%1")" ]
}

@test "hook state with cwd=/ attaches a non-claude pane beneath it" {
  # "/" + "/" == "//", which no absolute pane path starts with, so the pane
  # used to fall through to the is_claude_pane fallback and miss.
  _hook_state "alpha" "working" "/"
  _pane_row "%1" "alpha" "/work" "vim" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"⏺ working"* ]]
}

@test "hook state with a trailing slash in cwd still attaches its pane" {
  _hook_state "alpha" "idle" "/tmp/orchard-bats-alpha/"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha/sub" "vim" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"⏸ idle"* ]]
}

@test "hook state cwd is still a path boundary, not a string prefix" {
  # /tmp/orchard-bats-alpha must not claim /tmp/orchard-bats-alpha-other.
  _hook_state "alpha" "working" "/tmp/orchard-bats-alpha"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha-other" "vim" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" != *"working"* ]]
}

@test "worktree at / does not swallow the branch match for a pane beneath it" {
  printf '{"data":{"repos":[{"slug":"rooted","worktrees":[{"branch":"main","path":"/","host":"local"}]}]}}' \
    > "$TMPD/root-wt.json"
  _start_fake_daemon_with "$TMPD/root-wt.json"
  _pane_row "%1" "alpha" "/work" "zsh" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$FAKE_URL" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  # A worktree registered at / genuinely contains /work, so its chrome applies.
  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"rooted"* ]]
}

# --- daemon url shapes (review finding 6) ----------------------------------
#
# `--daemon-url` / `@orchard_daemon_url` are documented as FULL endpoints;
# `$ORCHARD_DAEMON_URL` was historically a BASE url with /graphql appended.
# Mirroring the documented option value into the env var must not produce
# `.../graphql/graphql` and silently empty results.

@test "ORCHARD_DAEMON_URL accepts a full endpoint without doubling /graphql" {
  _start_fake_daemon
  _pane_row "%1" "alpha" "/tmp/orchard-bats-wt" "zsh" > "$TMPD/panes"

  ORCHARD_DAEMON_URL="$FAKE_URL" run bash "$SCRIPT" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"issue-900/tmux-plugin"* ]]
}

@test "ORCHARD_DAEMON_URL still accepts a bare base url" {
  _start_fake_daemon
  _pane_row "%1" "alpha" "/tmp/orchard-bats-wt" "zsh" > "$TMPD/panes"

  ORCHARD_DAEMON_URL="${FAKE_URL%/graphql}" run bash "$SCRIPT" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"issue-900/tmux-plugin"* ]]
}

@test "a base url passed to --daemon-url is completed too" {
  _start_fake_daemon
  _pane_row "%1" "alpha" "/tmp/orchard-bats-wt" "zsh" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "${FAKE_URL%/graphql}" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"issue-900/tmux-plugin"* ]]
}

@test "a trailing slash on the daemon url does not break the endpoint" {
  _start_fake_daemon
  _pane_row "%1" "alpha" "/tmp/orchard-bats-wt" "zsh" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "${FAKE_URL}/" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"issue-900/tmux-plugin"* ]]
}

# --- sidecar provenance (review finding 2) ---------------------------------
#
# The heartbeat dir defaults to /tmp, which is world-writable, so any local
# unprivileged process can drop an `orchard-claude-<session>.json` naming a
# session it does not own. A sidecar is only believed when the caller owns it.

@test "a sidecar owned by another user is ignored" {
  _hook_state "alpha" "working" "/tmp/orchard-bats-alpha"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha" "claude" > "$TMPD/panes"

  # `daemon` exists on both macOS and the CI image and is never the test user.
  if ! chown daemon "$HOOKS/orchard-claude-alpha.json" 2>/dev/null; then
    skip "cannot chown (not root)"
  fi

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  [[ "$label" != *"working"* ]]
  [[ "$label" == *"⏵ claude"* ]]
}

@test "a world-writable sidecar is ignored" {
  _hook_state "alpha" "working" "/tmp/orchard-bats-alpha"
  chmod 666 "$HOOKS/orchard-claude-alpha.json"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha" "claude" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" != *"working"* ]]
}

@test "a sidecar that is a symlink is ignored" {
  _hook_state "alpha" "working" "/tmp/orchard-bats-alpha"
  mv "$HOOKS/orchard-claude-alpha.json" "$TMPD/real.json"
  ln -s "$TMPD/real.json" "$HOOKS/orchard-claude-alpha.json"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha" "claude" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" != *"working"* ]]
}

@test "a world-writable heartbeat dir without the sticky bit is refused" {
  _hook_state "alpha" "working" "/tmp/orchard-bats-alpha"
  chmod 777 "$HOOKS"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha" "claude" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" != *"working"* ]]
}

@test "a world-writable sticky heartbeat dir is accepted (the /tmp default)" {
  _hook_state "alpha" "working" "/tmp/orchard-bats-alpha"
  chmod 1777 "$HOOKS"
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha" "claude" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  [[ "$(printf '%s\n' "$output" | _label_for "%1")" == *"⏺ working"* ]]
}

@test "a state value outside the known enum is dropped, not rendered" {
  cat > "$HOOKS/orchard-claude-alpha.json" <<'JSON'
{"state": "pwned", "tmux_session": "alpha", "cwd": "/tmp/orchard-bats-alpha"}
JSON
  _pane_row "%1" "alpha" "/tmp/orchard-bats-alpha" "claude" > "$TMPD/panes"

  run bash "$SCRIPT" --daemon-url "$UNREACHABLE" \
    --heartbeat-dir "$HOOKS" --panes-file "$TMPD/panes" --print
  [ "$status" -eq 0 ]

  label="$(printf '%s\n' "$output" | _label_for "%1")"
  [[ "$label" != *"pwned"* ]]
  [[ "$label" == *"⏵ claude"* ]]
}
