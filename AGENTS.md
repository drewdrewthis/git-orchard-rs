# Agent Guidelines

## Common Mistakes & Corrections

| Mistake | Correct Behavior |
|---------|-----------------|
| Running `cargo clean` — this deletes the release binary that the user's symlink points to (`~/Library/pnpm/git-orchard` → `target/release/git-orchard`). The user's live command breaks until `cargo build --release` runs. | Never run `cargo clean` unless explicitly asked. If you must, immediately `cargo build --release` after. |
| Agents finishing at different times overwrite each other's changes to shared files (e.g., `tui/list.rs`, `tui/mod.rs`). Late-arriving agents revert fixes made after earlier agents completed. | Never run two agents that modify the same file in parallel. Sequence them, or have the second agent read the file fresh before editing. |
| Test assertions that are wrong (e.g., `backlog_pagination_second_page` expected 15 items but correct answer is 10). Agent writes the test, orchestrator trusts it, but the test fails. | Orchestrator must run `cargo test` after EACH agent completes, not just at the end. Fix failures before launching the next agent. |
| Tmux runtime bindings (`tmux bind-key`) take precedence over config-file bindings. Running `tmux source-file` does NOT override a runtime binding. | Always `tmux unbind-key <key>` before `tmux source-file` when replacing a binding. |
| Tmux hooks from the old architecture (`session-closed[99]`) persist across config reloads and can undo new bindings when legacy sessions die. | Explicitly `set-hook -gu session-closed[99]` during cleanup. |
| Adding `mod foo;` to `main.rs` but the agent's code has compile errors — the build breaks for everyone. | Agents should produce compilable code. Orchestrator must verify `cargo build` after each agent, not batch. |
| Multiple agents both add `mod` declarations to `main.rs`, creating duplicates or conflicts. | Only one agent should touch `main.rs` at a time, or a single agent should handle all `mod` declarations. |
| Agent creates methods referenced in `mod.rs` but puts them in a file (`dialogs.rs`) that the calling code can't see due to missing `impl` blocks or imports. | Agent must verify that all referenced methods exist in the correct `impl` block and are importable from the call site. |
| Dead code warnings accumulate because new modules aren't wired into the app yet. | Expected during staged implementation. Don't treat warnings as errors during development, but clean them up before the final review. |
| `#[serde(rename_all = "snake_case")]` on enums — Rust's `InProgress` serializes as `in_progress` via serde, but `format!("{:?}", status)` gives `InProgress`. Using debug formatting for event logging produces wrong strings. | Use a dedicated `fn status_str(s: TaskStatus) -> &'static str` for display/logging, not `Debug` formatting. |

## Architecture Notes

### Popup Model (current)
- The binary launches a TUI directly — no dedicated tmux session
- On Enter: creates worktree/session if needed, prints session name to stdout, exits
- A wrapper script (`~/.local/bin/orchard-popup`) captures stdout and runs `tmux switch-client`
- The tmux keybinding is: `bind-key o display-popup -E -w 90% -h 80% "$HOME/.local/bin/orchard-popup"`
- `q`/`Escape` exits with empty stdout (no switch)

### State System
- `~/.local/state/git-orchard/state.json` — persistent task state (atomic writes)
- `~/.local/state/git-orchard/events.jsonl` — structured event log (append-only, rotates at 50MB)
- `~/.local/state/git-orchard/status.txt` — tmux status bar segment (written on each refresh)
- `~/.local/state/git-orchard/debug.log` — low-level debug output

### File Ownership (which agents can touch which files)
- `src/main.rs` — orchestrator only (mod declarations, CLI dispatch)
- `src/tui/mod.rs` — one agent at a time (App struct, event loop)
- `src/tui/list.rs` — one agent at a time (rendering, key handlers)
- `src/tui/dialogs.rs` — one agent at a time (dialog rendering)
- `src/tui/state.rs` — one agent at a time (ViewState enum)
- `src/state.rs`, `src/events.rs`, `src/issue_sync.rs`, `src/session_discovery.rs` — independent, safe to parallelize
- `src/tmux.rs`, `src/remote.rs`, `src/git.rs`, `src/github.rs` — independent, safe to parallelize
- `Cargo.toml` — orchestrator only (dependency changes)

## Testing Rules

- Run `cargo test && cargo build --release` after EVERY agent completes
- Fix failures immediately before launching the next agent
- Never trust agent-written test assertions without verifying the logic
- Dead code warnings are OK during staged implementation but must be zero at final review
