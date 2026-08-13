#!/usr/bin/env bats
# fold-state.sh reducer: harness-injected turns must not pollute prompt fields.

setup() {
  export CLAUDE_SESSION_STATE_DIR="$(mktemp -d)"
  REDUCER="$BATS_TEST_DIRNAME/fold-state.sh"
}

teardown() { rm -rf "$CLAUDE_SESSION_STATE_DIR"; }

send() { jq -nc --arg p "$2" '{session_id: "t", hook_event_name: "UserPromptSubmit", prompt: $p}' | "$REDUCER"; }
field() { jq -r ".$1 // \"null\"" "$CLAUDE_SESSION_STATE_DIR/t.json"; }

@test "real prompt sets first_prompt and last_prompt" {
  send t "fix the login bug"
  [ "$(field first_prompt)" = "fix the login bug" ]
  [ "$(field last_prompt)" = "fix the login bug" ]
}

@test "task-notification block is not captured as a prompt" {
  send t $'<task-notification>\n<task-id>abc</task-id>\n</task-notification>'
  [ "$(field first_prompt)" = "null" ]
  [ "$(field last_prompt)" = "null" ]
  [ "$(field state)" = "working" ]  # the turn itself is real work
}

@test "harness-injected prefixes are all skipped" {
  for p in '<local-command-caveat>x' '<command-name>/compact</command-name>' '[SYSTEM NOTIFICATION] x' '<system-reminder>x'; do
    send t "$p"
  done
  [ "$(field first_prompt)" = "null" ]
}

@test "first_prompt skips injected turns and takes the first REAL prompt" {
  send t $'<task-notification>\nx'
  send t "the actual mission"
  [ "$(field first_prompt)" = "the actual mission" ]
}

notify() { jq -nc --arg m "$2" '{session_id: $ARGS.positional[0], hook_event_name: "Notification", message: $m}' --args "$1" | "$REDUCER"; }

@test "permission notification sets state=input with message" {
  notify t "Claude needs your permission to use Bash"
  [ "$(field state)" = "input" ]
  [ "$(field message)" = "Claude needs your permission to use Bash" ]
}

@test "idle-nag notification stays idle, no message stored" {
  notify t "Claude is waiting for your input"
  [ "$(field state)" = "idle" ]
  [ "$(field message)" = "null" ]
}

@test "idle-nag after an answered permission clears the stale message" {
  notify t "Claude needs your permission to use Bash"
  jq -nc '{session_id: "t", hook_event_name: "PreToolUse", tool_name: "Bash"}' | "$REDUCER"
  notify t "Claude is waiting for your input"
  [ "$(field state)" = "idle" ]
  [ "$(field message)" = "null" ]
}

@test "idle-nag does not downgrade a pending permission request" {
  notify t "Claude needs your permission to use Bash"
  notify t "Claude is waiting for your input"
  [ "$(field state)" = "input" ]
  [ "$(field message)" = "Claude needs your permission to use Bash" ]
}

@test "a real prompt merely containing the marker mid-text is kept" {
  send t 'the side bar says "<task-notification>", which is weird?'
  [ "$(field last_prompt)" = 'the side bar says "<task-notification>", which is weird?' ]
}
