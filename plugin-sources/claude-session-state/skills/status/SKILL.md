---
name: status
description: Query live Claude Code session state (working/idle/needs-input, mission, activity) for this machine. Use when asked what sessions are running, what they're doing, or which need attention.
---

# Session status

Live per-session state files: `${CLAUDE_SESSION_STATE_DIR:-~/.local/state/claude-sessions/state}/<sid>.json`
(contract: `${CLAUDE_PLUGIN_ROOT}/docs/STATE_SCHEMA.md`).

1. Preferred: `orchard status --json` (pid-checked, formatted). If the `orchard` binary is absent:
2. Read the state dir directly. A file is live only if `kill -0 <pid>` succeeds — skip dead-pid files (crashed sessions).
3. `state` = working|idle|input; `message` says why a session needs attention; `first_prompt` is its mission; `transcript_path` has full history.
