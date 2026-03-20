# Git Orchard (Rust)

## Build & Test

After **every** code change, run both:

```bash
cargo test
cargo build --release
```

Both must pass before work is considered complete. No exceptions.

The release binary is symlinked from `~/Library/pnpm/orchard` → `target/release/orchard`, so `cargo build --release` automatically updates the user's installed command.

## Project Structure

- Rust + Ratatui TUI for managing git worktrees
- Supports local and remote (SSH) worktrees with tmux session management
- Config lives in `.git/orchard.json`
