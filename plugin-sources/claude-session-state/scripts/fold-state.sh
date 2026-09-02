#!/usr/bin/env bash
# fold-state.sh — fold every Claude Code lifecycle event into a per-session state file.
#
# Contract: docs/STATE_SCHEMA.md. Self-contained (bash + jq only — no orchard binary).
# MUST always exit 0: a nonzero exit from PreToolUse denies the tool call.

STATE_DIR="${CLAUDE_SESSION_STATE_DIR:-$HOME/.local/state/claude-sessions/state}"

{
  umask 077
  mkdir -p "$STATE_DIR"

  input="$(cat)"
  sid="$(jq -r '.session_id // empty' <<<"$input")"
  event="$(jq -r '.hook_event_name // empty' <<<"$input")"
  [ -n "$sid" ] && [ -n "$event" ] || exit 0

  file="$STATE_DIR/$sid.json"

  if [ "$event" = "SessionEnd" ]; then
    rm -f "$file"
    exit 0
  fi

  prev="{}"
  [ -f "$file" ] && prev="$(cat "$file")"

  # $PPID is often a transient hook shell that dies after the event — climb to
  # the long-lived claude process so consumer pid-liveness checks hold between
  # events (ancestry-scoped match, not a global pgrep).
  pid=$PPID
  for _ in 1 2 3 4 5; do
    case "$(ps -o comm= -p "$pid" 2>/dev/null)" in
      *claude*|*node*) break ;;
    esac
    parent="$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')"
    { [ -n "$parent" ] && [ "$parent" -gt 1 ]; } 2>/dev/null || break
    pid=$parent
  done

  # Stop: fold the first line of the last assistant text from the transcript tail.
  last_response=""
  if [ "$event" = "Stop" ]; then
    tp="$(jq -r '.transcript_path // empty' <<<"$input")"
    if [ -n "$tp" ] && [ -f "$tp" ]; then
      # per-line fromjson? tolerates the tail starting mid-JSONL-record
      last_response="$(tail -c 65536 "$tp" \
        | jq -rRs '[split("\n")[] | fromjson? | select(.type == "assistant")
                    | .message.content[]? | select(.type == "text") | .text]
                   | last // ""' 2>/dev/null \
        | head -n 1 | cut -c 1-500)"
    fi
  fi

  tmp="$(mktemp "$STATE_DIR/.tmp.XXXXXX")"
  jq -n -c \
    --argjson prev "$prev" \
    --argjson ev "$input" \
    --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg pid "$pid" \
    --arg pane "${TMUX_PANE:-}" \
    --arg last_response "$last_response" '
    def trunc(n): if type == "string" and length > n then .[:n] + "…" else . end;

    ($ev.hook_event_name) as $event |
    ($prev + {
      sid: $ev.session_id,
      cwd: ($ev.cwd // $prev.cwd),
      transcript_path: ($ev.transcript_path // $prev.transcript_path),
      pid: ($pid | tonumber),
      pane: (if $pane == "" then ($prev.pane // null) else $pane end),
      started_at: ($prev.started_at // $ts),
      last_event: $event,
      ts: $ts,
      tool_calls: ($prev.tool_calls // 0)
    }) |

    if   $event == "SessionStart" then .state = "idle"
    elif $event == "UserPromptSubmit" then
      # harness-injected turns (task notifications, slash-command echoes,
      # system reminders) arrive through this hook too — real work, not a
      # human prompt; prefix-gated so real prompts CONTAINING a marker stay
      (($ev.prompt // "")
       | test("^(<task-notification>|<local-command-caveat>|<command-name>|\\[SYSTEM NOTIFICATION|<system-reminder>)") | not) as $human |
      .state = "working"
      | .message = null
      | (if $human then
           .last_prompt = ($ev.prompt | trunc(500))
           | .first_prompt = (.first_prompt // ($ev.prompt | trunc(500)))
         else . end)
    elif $event == "PreToolUse" then
      # a tool call means the permission ask was answered — clear it, or a stale
      # "needs your permission" rides along on a session that is plainly working
      .state = "working" | .message = null | .last_tool = $ev.tool_name | .tool_calls += 1
    elif $event == "PostToolUse" then
      .state = "working" | .message = null | .last_tool = $ev.tool_name
    elif $event == "Notification" then
      # the idle nag ("waiting for your input") is not a real input request —
      # keep it out of state=input, but never downgrade a pending permission ask
      if (($ev.message // "") | test("waiting for your input"; "i")) then
        (if .state == "input" then . else .state = "idle" | .message = null end)
      else
        .state = "input" | .message = ($ev.message | trunc(500))
      end
    elif $event == "Stop" then
      .state = "idle"
      | .message = null
      | (if $last_response != "" then .last_response = $last_response else . end)
    else . end
  ' > "$tmp" && mv "$tmp" "$file"
} 2>/dev/null

exit 0
