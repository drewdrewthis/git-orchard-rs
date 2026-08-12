# State file contract — `~/.local/state/claude-sessions/state/<sid>.json`

One folded JSON document per live Claude Code session (~1-2KB). Written atomically
(tmp + rename, 0600) by `scripts/fold-state.sh` on every lifecycle hook event.
Override the directory with `CLAUDE_SESSION_STATE_DIR`.

| Field | Set | Meaning |
| --- | --- | --- |
| `sid` | every event | Claude session UUID |
| `cwd` | every event (sticky) | session working directory |
| `transcript_path` | every event (sticky) | the full event history — the transcript IS the log; no separate event log exists |
| `pid` | every event | hook parent pid; consumers MUST `kill -0` it — a stale file with a dead pid is a crashed session |
| `pane` | when in tmux (sticky) | `$TMUX_PANE` id, joinable to tmux |
| `started_at` | set-once | first event's timestamp |
| `state` | event-mapped | `working` (UserPromptSubmit/Pre/PostToolUse) \| `idle` (SessionStart/Stop) \| `input` (Notification) |
| `last_event` / `ts` | every event | latest event name + UTC timestamp |
| `message` | Notification; cleared on prompt/Stop | why the session needs attention |
| `first_prompt` | set-once, ≤500 chars | the session's mission |
| `last_prompt` | UserPromptSubmit, ≤500 chars | latest user ask |
| `last_tool` | Pre/PostToolUse | latest tool name |
| `tool_calls` | counter (PreToolUse) | activity volume |
| `last_response` | Stop, ≤500 chars | first line of last assistant text, from transcript tail |

Rules:

- **`SessionEnd` deletes the file.** Live files + pid check = live sessions.
- **Content-class payloads are never stored** (`tool_input.content/new_string/old_string`,
  `tool_response`, file bodies). Prompts/messages/response truncate at 500 chars.
- Unknown events fold to `last_event`/`ts` only — the reducer must never error (exit 0 always;
  a nonzero PreToolUse exit denies the tool call).
