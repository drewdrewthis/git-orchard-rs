//! `orchard` — thin dispatcher for the Orchard CLI ecosystem.
//!
//! Implements the `git`/`cargo`/`kubectl` dispatcher pattern: this binary owns
//! the user-facing `orchard` name, parses the first positional argument, and
//! execs the matching helper binary (`orchard-tui`, `orchard-daemon`,
//! `orchard-worktree`, `orchard-chat`, `orchard-shell`, `orchard-upgrade`)
//! with the remaining arguments.
//!
//! Bare-verb shortcuts (`orchard new <N>` ≡ `orchard worktree new <N>`) are
//! reserved for the worktree primary unit per ADR-013. Adding a second primary
//! unit requires a new ADR.
//!
//! # Discovery rule
//!
//! For a verb `V`, the dispatcher searches for `orchard-V` in this order:
//!
//! 1. The directory containing the `orchard` executable itself (bundled
//!    install — `brew install orchard` puts every binary in the same `bin/`).
//! 2. `$PATH` (third-party plugins, matching `kubectl`'s plugin model).
//!
//! Found-first wins. This avoids accidentally invoking a stale `$PATH` entry
//! when the bundled binary is what the user installed.
//!
//! # Why no clap
//!
//! The dispatcher must NOT parse helper-binary arguments — it just forwards
//! them. Using a CLI framework would invite incidental coupling. Hand-rolled
//! argv handling keeps this file small and the dispatch contract obvious.

use std::env;
use std::io::{self, Write};
use std::path::PathBuf;
use std::process::{Command, ExitCode};

/// Bare-verb shortcuts that resolve to the worktree primary unit.
///
/// Per ADR-013: only the project's primary unit gets bare verbs. Adding a
/// second primary unit (e.g. remotes becoming first-class) requires a new ADR.
const WORKTREE_BARE_VERBS: &[&str] = &["new", "rm", "prune", "mv", "ls", "path"];

/// Bare-verb shortcuts that resolve to the chat unit (per #495).
///
/// `orchard send #general "hi"` ≡ `orchard chat send #general "hi"`. Chat
/// isn't a "primary unit" in the ADR-013 sense, but `send` reads natural
/// at the top level and stays unambiguous because room/handle targets carry
/// their own sigil.
const CHAT_BARE_VERBS: &[&str] = &["send"];

/// Verbs that map directly to a helper binary `orchard-<verb>`.
const NAMESPACE_VERBS: &[&str] = &["tui", "daemon", "worktree", "chat", "shell", "upgrade"];

/// Verbs that orchard-tui handles internally.
///
/// These flat verbs predate ADR-013 and live inside the orchard-tui binary's
/// argv parser (see `crates/orchard/src/main.rs::main`). The dispatcher
/// forwards `orchard <verb> <args>` to `orchard-tui <verb> <args>` for each
/// of these. Backwards-compat: every name listed here matches what
/// `orchard-tui` accepts today.
///
/// Step 6's namespaced grammar (`remote setup`, `hook ingest`, etc.) is
/// future work — these flat names continue to work and remain documented
/// for at least one minor version per ADR-013.
const TUI_VERBS: &[&str] = &[
    "init",
    "heal",
    "refresh",
    "watch",
    "sessions",
    "setup-remote",
    "list-remotes",
    "hook-enrich",
    "webhook-serve",
    "restore",
];

const HELP: &str = "\
orchard — Git worktree, tmux session, and PR dashboard.

Usage:
  orchard                       Open the TUI dashboard (default).
  orchard <command> [args...]   Run a subcommand.
  orchard --help                Show this help.
  orchard --version             Show dispatcher version.

Commands:
  tui                           Run the TUI dashboard explicitly.
  daemon <subcmd>               Manage the daemon (start, stop, status, ...).
  worktree <subcmd>             Manage worktrees (new, rm, prune, mv, ls, path).
  chat <subcmd>                 Agent-to-agent chat (send, broadcast, tail).
  shell [args...]               Boot the outer tmux wrapper with the live sidebar.
  upgrade [--check]             Upgrade the installed suite to the latest release.

  init                          Run the first-time setup wizard.
  heal [--fix] [--json]         Diagnose and repair local state.
  refresh                       Refresh cached worktree/PR data.
  watch                         Tail the local event stream.
  sessions [--json]             Inspect active tmux/claude sessions.
  setup-remote <host>           Provision orchard on a remote SSH host.
  list-remotes [--json]         List configured remotes.
  hook-enrich --transcript ...  Claude Code hook entry point.
  webhook-serve                 Run the GitHub webhook receiver.
  restore                       Reconstruct dead tmux sessions from cache.

Worktree shortcuts (the worktree is Orchard's primary unit):
  orchard new <issue>           ≡ orchard worktree new <issue>
  orchard rm <id>               ≡ orchard worktree rm <id>
  orchard prune [filter]        ≡ orchard worktree prune [filter]
  orchard mv <id> <host>        ≡ orchard worktree mv <id> <host>
  orchard ls [--json]           ≡ orchard worktree ls [--json]
  orchard path <id>             ≡ orchard worktree path <id>

Chat shortcuts:
  orchard send <target> <text>  ≡ orchard chat send <target> <text>

Discovery:
  Helper binaries are looked up first in the directory containing this
  `orchard` executable, then on $PATH. See ADR-013 for details.
";

const VERSION: &str = env!("CARGO_PKG_VERSION");

fn main() -> ExitCode {
    let args: Vec<String> = env::args().skip(1).collect();

    match args.first().map(String::as_str) {
        None => exec("tui", &[]),
        Some("--help" | "-h") => {
            // Ignore broken-pipe so `orchard --help | head` exits cleanly
            // instead of panicking with "failed printing to stdout".
            let _ = io::stdout().write_all(HELP.as_bytes());
            ExitCode::SUCCESS
        }
        Some("--version" | "-V") => {
            let _ = writeln!(io::stdout(), "orchard {VERSION}");
            ExitCode::SUCCESS
        }
        Some(verb) if WORKTREE_BARE_VERBS.contains(&verb) => {
            // `orchard new 412` → `orchard-worktree new 412`
            let mut forwarded = vec![verb.to_string()];
            forwarded.extend(args.iter().skip(1).cloned());
            exec("worktree", &forwarded)
        }
        Some(verb) if CHAT_BARE_VERBS.contains(&verb) => {
            // `orchard send #general "hi"` → `orchard-chat send #general "hi"`
            let mut forwarded = vec![verb.to_string()];
            forwarded.extend(args.iter().skip(1).cloned());
            exec("chat", &forwarded)
        }
        Some(verb) if NAMESPACE_VERBS.contains(&verb) => {
            let forwarded: Vec<String> = args.iter().skip(1).cloned().collect();
            exec(verb, &forwarded)
        }
        Some(verb) if TUI_VERBS.contains(&verb) => {
            // `orchard heal` / `orchard refresh` / `orchard setup-remote …` —
            // forward the verb itself plus the remaining args to orchard-tui,
            // which has the original argv parser for these.
            let mut forwarded = vec![verb.to_string()];
            forwarded.extend(args.iter().skip(1).cloned());
            exec("tui", &forwarded)
        }
        Some(verb) => {
            // Unknown verb — try to dispatch anyway so third-party plugins on
            // $PATH can resolve. If no binary exists, the resolver below
            // surfaces the error.
            let forwarded: Vec<String> = args.iter().skip(1).cloned().collect();
            exec(verb, &forwarded)
        }
    }
}

/// Executes `orchard-<verb>` with `forwarded` as its arguments.
///
/// On `Err` resolving the binary, prints a clear message to stderr and exits
/// with a non-zero code. On successful exec, returns the child's exit code.
fn exec(verb: &str, forwarded: &[String]) -> ExitCode {
    let binary_name = format!("orchard-{verb}");
    let resolved = match resolve_helper(&binary_name) {
        Some(path) => path,
        None => {
            eprintln!(
                "orchard: unknown command '{verb}'.\n\
                 No '{binary_name}' found beside the orchard binary or on $PATH.\n\
                 Run 'orchard --help' to see available commands."
            );
            return ExitCode::from(127);
        }
    };

    let status = Command::new(&resolved).args(forwarded).status();

    match status {
        Ok(s) => ExitCode::from(child_exit_code(&s)),
        Err(e) => {
            eprintln!("orchard: failed to exec '{}': {}", resolved.display(), e);
            ExitCode::from(126)
        }
    }
}

/// Maps a child's `ExitStatus` to a `u8` exit code.
///
/// On Unix, signal-killed children encode as `128 + signum` (POSIX shell
/// convention — `kill -9` → 137). Normal exits return their status code.
/// On non-Unix or when neither is available, returns 1 as a fallback.
fn child_exit_code(status: &std::process::ExitStatus) -> u8 {
    if let Some(code) = status.code() {
        return code as u8;
    }
    #[cfg(unix)]
    {
        use std::os::unix::process::ExitStatusExt;
        if let Some(signum) = status.signal() {
            return (128 + (signum as u32 & 0x7f)) as u8;
        }
    }
    1
}

/// Resolves a helper binary by name.
///
/// Search order:
/// 1. Directory containing the running `orchard` executable.
/// 2. Each entry of `$PATH`.
fn resolve_helper(binary_name: &str) -> Option<PathBuf> {
    if let Ok(self_path) = env::current_exe()
        && let Some(dir) = self_path.parent()
    {
        let candidate = dir.join(binary_name);
        if is_executable(&candidate) {
            return Some(candidate);
        }
    }

    if let Some(path) = env::var_os("PATH") {
        for dir in env::split_paths(&path) {
            let candidate = dir.join(binary_name);
            if is_executable(&candidate) {
                return Some(candidate);
            }
        }
    }

    None
}

/// Reports whether `path` exists and is executable by the current user.
#[cfg(unix)]
fn is_executable(path: &std::path::Path) -> bool {
    use std::os::unix::fs::PermissionsExt;
    path.metadata()
        .map(|m| m.is_file() && m.permissions().mode() & 0o111 != 0)
        .unwrap_or(false)
}

/// Windows fallback: existence-check only (no posix permission bits).
#[cfg(not(unix))]
fn is_executable(path: &std::path::Path) -> bool {
    path.metadata().map(|m| m.is_file()).unwrap_or(false)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn worktree_bare_verbs_do_not_collide_with_namespace_verbs() {
        for v in WORKTREE_BARE_VERBS {
            assert!(
                !NAMESPACE_VERBS.contains(v),
                "bare verb '{v}' must not also be a namespace verb (would create ambiguity)"
            );
        }
    }

    #[test]
    fn chat_bare_verbs_do_not_collide_with_other_verb_sets() {
        for v in CHAT_BARE_VERBS {
            assert!(
                !NAMESPACE_VERBS.contains(v),
                "chat bare verb '{v}' must not also be a namespace verb"
            );
            assert!(
                !WORKTREE_BARE_VERBS.contains(v),
                "chat bare verb '{v}' must not also be a worktree bare verb"
            );
            assert!(
                !TUI_VERBS.contains(v),
                "chat bare verb '{v}' must not also be a TUI verb"
            );
        }
    }

    #[test]
    fn chat_bare_verbs_match_issue_495() {
        // Locked to the #495 contract. Adding a chat verb (e.g. `broadcast`)
        // is a deliberate ADR-style decision — bump this test along with it.
        assert_eq!(CHAT_BARE_VERBS, &["send"]);
    }

    #[test]
    fn worktree_bare_verbs_match_adr() {
        // ADR-013 §3 specifies the exact set; if this list changes, update the ADR too.
        assert_eq!(
            WORKTREE_BARE_VERBS,
            &["new", "rm", "prune", "mv", "ls", "path"]
        );
    }

    #[test]
    fn namespace_verbs_match_adr() {
        // ADR-013 §2 lists the helper binaries — the dispatcher routes to each.
        // #747 amendment: shell + upgrade joined the helper-binary set.
        assert_eq!(
            NAMESPACE_VERBS,
            &["tui", "daemon", "worktree", "chat", "shell", "upgrade"]
        );
    }

    #[test]
    fn shell_and_upgrade_are_namespace_verbs_not_tui_verbs() {
        // #747 AC4: shell and upgrade became real helper binaries
        // (orchard-shell, orchard-upgrade), not orchard-tui subcommands.
        for v in ["shell", "upgrade"] {
            assert!(
                NAMESPACE_VERBS.contains(&v),
                "'{v}' must be a namespace verb (routes to orchard-{v})"
            );
            assert!(
                !TUI_VERBS.contains(&v),
                "'{v}' must not be a TUI verb (orchard-tui no longer handles it)"
            );
        }
    }

    #[test]
    fn help_lists_all_namespace_verbs() {
        for v in NAMESPACE_VERBS {
            assert!(
                HELP.contains(&format!("  {v}")) || HELP.contains(&format!("  {v} ")),
                "HELP must document the '{v}' verb"
            );
        }
    }

    #[test]
    fn help_lists_all_bare_verbs() {
        for v in WORKTREE_BARE_VERBS {
            assert!(
                HELP.contains(&format!("orchard {v} ")) || HELP.contains(&format!("orchard {v}\n")),
                "HELP must document the bare verb '{v}'"
            );
        }
        for v in CHAT_BARE_VERBS {
            assert!(
                HELP.contains(&format!("orchard {v} ")) || HELP.contains(&format!("orchard {v}\n")),
                "HELP must document the chat bare verb '{v}'"
            );
        }
    }

    #[test]
    fn tui_verbs_do_not_collide_with_other_verb_sets() {
        for v in TUI_VERBS {
            assert!(
                !NAMESPACE_VERBS.contains(v),
                "TUI verb '{v}' must not also be a namespace verb"
            );
            assert!(
                !WORKTREE_BARE_VERBS.contains(v),
                "TUI verb '{v}' must not also be a bare-worktree verb"
            );
        }
    }

    #[test]
    fn tui_verbs_cover_orchard_tui_argv_parser() {
        // Anchor: orchard-tui's main.rs accepts these as the first positional
        // argument. If a verb is added/removed there, this list must follow.
        // See `crates/orchard/src/main.rs::main` match arms.
        assert_eq!(
            TUI_VERBS,
            &[
                "init",
                "heal",
                "refresh",
                "watch",
                "sessions",
                "setup-remote",
                "list-remotes",
                "hook-enrich",
                "webhook-serve",
                "restore",
            ]
        );
    }

    #[test]
    fn help_lists_every_tui_verb() {
        for v in TUI_VERBS {
            assert!(
                HELP.contains(&format!("  {v}")),
                "HELP must document the TUI verb '{v}'"
            );
        }
    }

    #[test]
    fn is_executable_returns_false_for_nonexistent_path() {
        assert!(!is_executable(std::path::Path::new("/this/does/not/exist")));
    }

    #[test]
    fn is_executable_returns_true_for_known_executable() {
        // /bin/sh is on every UNIX-like system the dispatcher targets.
        assert!(is_executable(std::path::Path::new("/bin/sh")));
    }
}
