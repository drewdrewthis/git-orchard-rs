//! Binary entry point for the `orchard-tui` CLI.
//!
//! Parses CLI flags (`--json`, `--version`, `--help`), handles the `init`
//! sub-command (and rejects the removed `upgrade` sub-command with a pointer
//! to `orchard upgrade`), and dispatches to either the JSON output path or
//! the Ratatui TUI (re-launching itself inside a tmux popup when appropriate).
//!
//! The binary is named `orchard-tui` so it can coexist with the Go daemon's
//! `orchard` CLI on the same `$PATH`.
//!
//! # Domain subcommands (L1 / L6)
//!
//! Per the repo constitution L1 (operations live as scripts) and L6 (CLI is
//! standalone), `orchard-tui <domain> <op>` wraps the corresponding
//! `scripts/<domain>-<op>.sh --json` without requiring the daemon:
//!
//! ```text
//! orchard-tui gh            <op>  [flags]
//! orchard-tui git           <op>  [flags]
//! orchard-tui tmux          <op>  [flags]
//! orchard-tui ps            <op>  [flags]
//! orchard-tui host-services <op>  [flags]
//! orchard-tui claude-account <op> [flags]
//! orchard-tui daemon        <op>
//! ```
//!
//! Each domain also exposes a `raw [-- <args...>]` pass-through per S16b.
use std::env;
use std::io::IsTerminal;

use crossterm::{
    cursor,
    terminal::{self, LeaveAlternateScreen},
};
use orchard::build_state;
use orchard::cache;
use orchard::chat;
use orchard::cli;
use orchard::federation;
use orchard::global_config;
use orchard::heal;
use orchard::json_output::JsonOutput;
use orchard::logger;
use orchard::restore;
use orchard::setup_remote;
use orchard::shell;
use orchard::tui;

fn main() {
    // color_eyre's HookBuilder probes the terminal (opens /dev/tty) during
    // install. That fails with ENXIO in non-interactive contexts — cron,
    // systemd, CI runners, `ssh host "orchard-tui refresh"` without `-t`. Skip
    // panic-hook installation when stderr isn't a TTY; the default panic
    // handler is fine for batch invocations, and AC7 requires background
    // services (refresh / watch / --json) to work without a controlling TTY.
    if should_install_panic_hooks() {
        install_panic_hooks();
    }

    let args: Vec<String> = env::args().collect();

    // ---------------------------------------------------------------------------
    // Domain subcommand dispatch (L1 / L6 — standalone, no daemon required).
    //
    // If the first positional argument (args[1]) is a domain name, hand off
    // everything after it to the domain handler. All domain `run()` functions
    // call `std::process::exit` on every code path (success, error, or unknown
    // subcommand / usage), so this block never falls through.
    // ---------------------------------------------------------------------------
    if let Some(domain) = args.get(1).map(|s| s.as_str()) {
        let domain_args: Vec<String> = args.get(2..).unwrap_or(&[]).to_vec();
        match domain {
            "gh" => {
                cli::gh::run(&domain_args);
                unreachable!("gh::run always exits");
            }
            "git" => {
                cli::git::run(&domain_args);
                unreachable!("git::run always exits");
            }
            "tmux" => {
                cli::tmux_cmd::run(&domain_args);
                unreachable!("tmux_cmd::run always exits");
            }
            "ps" => {
                cli::ps::run(&domain_args);
                unreachable!("ps::run always exits");
            }
            "host-services" => {
                cli::host_services::run(&domain_args);
                unreachable!("host_services::run always exits");
            }
            "claude-account" => {
                cli::claude_account::run(&domain_args);
                unreachable!("claude_account::run always exits");
            }
            "daemon" => {
                cli::daemon_cmd::run(&domain_args);
                unreachable!("daemon_cmd::run always exits");
            }
            // Not a domain name — fall through to the existing handler below.
            _ => {}
        }
    }

    let mut json_flag = false;
    let mut schema_flag = false;
    let mut fix_flag = false;
    let mut command = String::new();

    let mut chat_target: Option<String> = None;
    let mut chat_message: Option<String> = None;
    let mut skip_next = false;

    for (i, arg) in args[1..].iter().enumerate() {
        if skip_next {
            skip_next = false;
            continue;
        }
        match arg.as_str() {
            "--json" => json_flag = true,
            "--schema" => schema_flag = true,
            "--fix" => fix_flag = true,
            "--version" | "-V" => {
                println!("orchard-tui {}", env!("CARGO_PKG_VERSION"));
                return;
            }
            "--help" | "-h" => {
                print_usage();
                return;
            }
            "--target" => {
                chat_target = args.get(i + 2).cloned();
                skip_next = true;
            }
            "--message" => {
                chat_message = args.get(i + 2).cloned();
                skip_next = true;
            }
            _ if !arg.starts_with('-') && command.is_empty() => command = arg.clone(),
            _ => {}
        }
    }

    if schema_flag {
        handle_schema();
        return;
    }

    logger::LOG.info(&format!(
        "startup: orchard-tui{}",
        if command.is_empty() {
            String::new()
        } else {
            format!(" command={command}")
        }
    ));

    match command.as_str() {
        "init" => handle_init(),
        "upgrade" => {
            eprintln!(
                "orchard-tui: 'upgrade' is no longer handled here. Run 'orchard upgrade' instead."
            );
            std::process::exit(1);
        }
        "setup-remote" => handle_setup_remote(&args),
        "heal" => handle_heal(fix_flag, json_flag),
        "chat" => handle_chat(chat_target.as_deref(), chat_message.as_deref()),
        "watch" => handle_watch(&args),
        "refresh" => handle_refresh(&args),
        "webhook-serve" => handle_webhook_serve(&args),
        "list-remotes" => handle_list_remotes(json_flag),
        "sessions" => handle_sessions(json_flag),
        "restore" => handle_restore(),
        _ => {
            if json_flag {
                handle_json();
            } else {
                handle_tui(&command);
            }
        }
    }
}

fn handle_init() {
    match shell::run_init_wizard() {
        Ok(()) => {}
        Err(e) => {
            eprintln!("{}", e);
            std::process::exit(1);
        }
    }
}

fn handle_setup_remote(args: &[String]) {
    // args[0] = binary, args[1] = "setup-remote", args[2] = host.
    let host = args.get(2).map(|s| s.as_str()).unwrap_or("");

    if host.is_empty() {
        eprintln!("Usage: orchard-tui setup-remote <host>");
        eprintln!("  <host> may be a remote name from config or a direct SSH target (user@host)");
        std::process::exit(1);
    }

    match setup_remote::run(host) {
        Ok(()) => {}
        Err(e) => {
            eprintln!("{e}");
            std::process::exit(1);
        }
    }
}

/// Outputs the list of configured remotes in JSON format.
///
/// When `--json` is present, serialises a [`federation::ListRemotesOutput`]
/// to stdout.  Without `--json`, prints a human-readable summary.
///
/// The JSON wire format is versioned with its OWN independent version constant
/// ([`federation::LIST_REMOTES_MIN_VERSION`]) — callers check `version >=
/// LIST_REMOTES_MIN_VERSION` (lower bound, NOT an exact whitelist).  This
/// avoids the version-skew trap in `JsonOutput`'s exact-whitelist design.
fn handle_list_remotes(json: bool) {
    let config = global_config::load_global_config();

    if json {
        let output = federation::build_list_remotes_output(&config);
        match serde_json::to_string_pretty(&output) {
            Ok(s) => println!("{s}"),
            Err(e) => {
                eprintln!("list-remotes: failed to serialize output: {e}");
                std::process::exit(1);
            }
        }
    } else {
        let all_remotes: Vec<_> = config.repos.iter().flat_map(|r| r.remotes.iter()).collect();
        if all_remotes.is_empty() {
            println!("No remotes configured.");
        } else {
            for r in &all_remotes {
                println!(
                    "{} ({}) — {}",
                    r.name,
                    r.host,
                    if r.allow_transitive {
                        "transitive"
                    } else {
                        "direct"
                    }
                );
            }
        }
    }
}

/// Runs the heal command in CLI mode (non-TUI).
///
/// Gathers live state, diagnoses the environment, and prints a report.
/// When `fix` is true, applies all actionable fixes. When `json` is true,
/// outputs machine-readable JSON instead of the formatted report.
fn handle_heal(fix: bool, json: bool) {
    use orchard::cache::{CachedTmuxSession, read_cache, tmux_cache_path};
    use orchard::cache_sources;
    use orchard::session::active_pane_cwd;
    use orchard::types::TmuxSession;

    let config = global_config::load_global_config();

    // Refresh the local tmux cache so active_pane_cwd reflects current state.
    // Best-effort: a failure here means we fall back to whatever is on disk.
    if let Err(e) = cache_sources::refresh_tmux_sessions(None) {
        eprintln!("heal: tmux cache refresh failed (using cached data): {e}");
    }

    // Capture the invoking session name before building the sessions list so
    // diagnose() can mark self-targeting KillSession findings with is_self=true.
    let current_session = orchard::tmux::current_session_name();

    // Build TmuxSession list from the cache, enriching each entry with the
    // live active-pane cwd derived from pane_paths + pane_active.
    let sessions: Vec<TmuxSession> = read_cache::<CachedTmuxSession>(&tmux_cache_path(None))
        .entries
        .into_iter()
        .map(|s| {
            let pane_title = s.pane_titles.first().cloned();
            let cwd = active_pane_cwd(&s);
            TmuxSession {
                name: s.name,
                path: s.path,
                attached: false,
                pane_title,
                active_pane_cwd: cwd,
            }
        })
        .collect();

    // Sidecar cleanup is handled daemon-side (the SidecarJanitor in
    // `internal/server/providers/claudeinstance/janitor.go` sweeps at every
    // daemon boot). Heal no longer touches `$TMPDIR/orchard-claude-*.json`.
    let cache_files = heal::gather_cache_files();
    let known_slugs: Vec<String> = config.repos.iter().map(|r| r.slug.clone()).collect();

    // Build HealWorktree entries from all configured repo caches.
    let worktrees = build_heal_worktrees_for_cli(&config);

    let report = heal::diagnose(&heal::HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        cache_files: &cache_files,
        known_repo_slugs: &known_slugs,
        current_session: current_session.as_deref(),
    });

    // Abort before applying any destructive fixes when the invoking session
    // has an Error-severity self-classification. A session in an unknown-bad
    // state cannot safely kill itself. Dry-run and JSON modes always continue
    // so consumers see the is_self danger flag without triggering the abort.
    if fix && let Some(self_err) = heal::detect_self_error(&report) {
        eprintln!(
            "orchard-tui heal: refusing to kill the session I'm running in; run from outside tmux"
        );
        eprintln!("  finding: {}", self_err.message);
        std::process::exit(1);
    }

    if json {
        let output = serde_json::to_string_pretty(&report).unwrap_or_else(|e| {
            eprintln!("Error serializing JSON: {e}");
            std::process::exit(1);
        });
        println!("{output}");
        return;
    }

    let fix_results = if fix {
        Some(heal::apply_fixes(&report.findings))
    } else {
        None
    };

    println!("{}", heal::format_report(&report, fix_results.as_deref()));
}

/// Builds `HealWorktree` entries for the CLI path (no TUI app state available).
fn build_heal_worktrees_for_cli(config: &global_config::GlobalConfig) -> Vec<heal::HealWorktree> {
    let mut result = Vec::new();
    for repo in &config.repos {
        let worktrees = orchard::cache::read_cache::<orchard::cache::CachedWorktree>(
            &orchard::cache::cache_path(repo.owner(), repo.repo_name(), "worktrees"),
        )
        .entries;
        let prs = orchard::cache::read_cache::<orchard::cache::CachedPr>(
            &orchard::cache::cache_path(repo.owner(), repo.repo_name(), "prs"),
        )
        .entries;
        let issues = orchard::cache::read_cache::<orchard::cache::CachedIssue>(
            &orchard::cache::cache_path(repo.owner(), repo.repo_name(), "issues"),
        )
        .entries;

        for wt in worktrees.iter().filter(|w| !w.is_bare) {
            let pr = prs.iter().find(|p| p.branch == wt.branch);
            let issue_number = orchard::github::extract_issue_number(&wt.branch);
            let issue_state = issue_number
                .and_then(|n| issues.iter().find(|i| i.number == n))
                .map(|i| i.state.clone());
            let expected_session_name =
                orchard::tmux::derive_main_session_name(&wt.path, Some(&wt.branch));
            result.push(heal::HealWorktree {
                path: wt.path.clone(),
                branch: wt.branch.clone(),
                expected_session_name: Some(expected_session_name),
                pr_state: pr.map(|p| p.state.clone()),
                pr_number: pr.map(|p| p.number),
                issue_state,
            });
        }
    }
    result
}

/// Handles `orchard-tui chat [--target <session>] [--message <text>]`.
///
/// Resolves the orchardist session and delivers the message via `tmux send-keys`.
/// Exits non-zero with usage if `--message` is missing or empty — the no-op-on-empty
/// behavior belongs in the wrapper script (`orchard-chat`), not the CLI.
fn handle_chat(target: Option<&str>, message: Option<&str>) {
    let message = match message {
        Some(m) if !m.is_empty() => m.to_string(),
        _ => {
            eprintln!("Error: --message is required and must not be empty");
            eprintln!();
            eprintln!("Usage: orchard-tui chat [--target <session>] --message <text>");
            std::process::exit(1);
        }
    };

    let config = global_config::load_global_config();
    let session = match chat::resolve_target(&config, target) {
        Ok(s) => s,
        Err(e) => {
            eprintln!("{e}");
            std::process::exit(1);
        }
    };

    if let Err(e) = chat::send_to_orchardist(&session, &message, chat::run_command) {
        eprintln!("{e}");
        std::process::exit(1);
    }
}

/// Builds the versioned JSON output from a **live** refresh — never cache.
///
/// Freshness contract (issue #374): `orchard-tui --json` is the source of truth,
/// not a cache view. It runs the same refresh path as `orchard refresh`
/// (probes hosts, fetches remote worktrees and tmux sessions over SSH,
/// re-runs `git worktree list` and `tmux list-panes` locally, refreshes
/// GitHub issues/PRs) before serialising. The TUI keeps its cache-fast
/// path; `--json` does not.
///
/// Practical implications for callers:
/// - Latency tracks the slowest reachable host's SSH round-trip plus the
///   GitHub API. Unreachable hosts are bounded by reachability-probe
///   timeouts. Use `orchard watch` if you need a low-latency feed.
/// - Side effect: caches written by `orchard refresh` (`~/.cache/orchard/*`)
///   are also written here. Subsequent TUI cold-starts benefit from this.
fn build_output() -> JsonOutput {
    let config = global_config::load_global_config();
    let state = build_state::refresh_and_build(&config);

    // Persist host reachability so subsequent cache-only reads
    // (TUI cold start, watch daemon) can populate OrchardState.hosts.
    if let Err(e) = cache::write_host_reachability(&state.hosts) {
        logger::LOG.warn(&format!("--json: failed to persist host reachability: {e}"));
    }

    JsonOutput::from(&state)
}

fn handle_json() {
    emit_json_deprecation_warning("orchard-tui --json");
    let output = build_output();
    let json = serde_json::to_string_pretty(&output).unwrap_or_else(|e| {
        eprintln!("Error serializing JSON: {e}");
        std::process::exit(1);
    });
    println!("{json}");
}

/// Emits a deprecation warning for `--json` flag surfaces (issue #438).
///
/// The daemon's GraphQL endpoint at `http://127.0.0.1:7777/graphql` is the
/// canonical read surface. Full removal of `--json` is blocked by #404
/// (peerproxy cross-host federation); the warning ships first so consumers
/// have a migration runway.
fn emit_json_deprecation_warning(surface: &str) {
    eprintln!(
        "[deprecated] `{surface}` is deprecated and will be removed. \
         Query the daemon's GraphQL endpoint at http://127.0.0.1:7777/graphql instead. \
         See https://github.com/drewdrewthis/orchardist/issues/438."
    );
}

/// Handles `orchard-tui sessions --json` — comprehensive sessions index keyed by host.
///
/// Returns every tmux session known to orchard across all managed hosts (not
/// just orchard-managed ones), classified by relationship to worktrees and
/// the protected-session list. Each entry carries `host` inline so consumers
/// like the orchardist's `/prune` skill can route per-host actions without
/// inferring from output order.
///
/// Freshness contract (issue #375) matches `--json`: this is a live read,
/// not a cache view. We refresh every reachable remote tmux source over
/// SSH plus the local `tmux list-panes` before classifying. `orchard
/// refresh` and the on-disk caches are reused as a side effect.
///
/// Wire format is versioned independently from `--json` (its own
/// `SESSIONS_INDEX_VERSION`), so additions here cannot break consumers of the
/// worktree-centric `--json` output.
fn handle_sessions(json: bool) {
    if json {
        emit_json_deprecation_warning("orchard-tui sessions --json");
        // `--json` keeps the legacy SessionsIndexOutput shape so the
        // orchardist `/prune` skill (and other downstream consumers that
        // pin on it) don't break. Issue #426 introduces the federated
        // daemon view via the no-flag interactive picker; rebinding
        // `--json` to that shape is a coordinated migration tracked
        // separately.
        let config = global_config::load_global_config();
        let _state = build_state::refresh_and_build(&config);
        let output = orchard::sessions_index::build_sessions_index(&config);
        let serialized = serde_json::to_string_pretty(&output).unwrap_or_else(|e| {
            eprintln!("Error serializing JSON: {e}");
            std::process::exit(1);
        });
        println!("{serialized}");
        return;
    }

    // No-flag mode is the new thin-shell federated picker.
    let code = orchard::sessions_cli::run(false);
    if code != 0 {
        std::process::exit(code);
    }
}

/// Handles `orchard-tui restore` — explicitly reconstruct dead tmux sessions
/// from the local manifest cache.
///
/// This is the **only** caller of [`restore::restore_all_local`] in production.
/// Read paths (`refresh_and_build`, `App::new`, `--json`) deliberately do NOT
/// invoke restore — killed sessions stay killed until the user runs this
/// subcommand. See issue #460.
///
/// Prints one line per cached entry classifying it as Restored / Skipped(reason)
/// / Failed(step). Exits 0 even on partial Failures (matching the historical
/// best-effort semantics of `restore_all_local`); a non-zero exit is reserved
/// for cases where the orchestration itself can't run.
fn handle_restore() {
    let report = restore::restore_all_local();

    if report.sessions.is_empty() {
        println!("restore: no cached sessions to restore");
        return;
    }

    let mut restored = 0usize;
    let mut skipped = 0usize;
    let mut failed = 0usize;
    for (name, outcome) in &report.sessions {
        match outcome {
            restore::SessionRestoreOutcome::Restored { windows, panes } => {
                restored += 1;
                println!("  restored: {name} ({windows} windows, {panes} panes)");
            }
            restore::SessionRestoreOutcome::Skipped(reason) => {
                skipped += 1;
                println!("  skipped:  {name} ({})", skip_reason_str(reason));
            }
            restore::SessionRestoreOutcome::Failed { step, error } => {
                failed += 1;
                println!("  failed:   {name} ({}: {error})", restore_step_str(step));
            }
        }
    }
    println!("restore: {restored} restored, {skipped} skipped, {failed} failed");
}

/// Maps a [`restore::SkipReason`] to a short user-facing phrase.
fn skip_reason_str(reason: &restore::SkipReason) -> &'static str {
    match reason {
        restore::SkipReason::AlreadyRunning => "already running",
        restore::SkipReason::WorktreeGone => "worktree gone",
        restore::SkipReason::RemoteNotSupported => "remote (not supported in v1)",
    }
}

/// Maps a [`restore::RestoreStep`] to a short user-facing phrase.
fn restore_step_str(step: &restore::RestoreStep) -> &'static str {
    match step {
        restore::RestoreStep::NewSession => "tmux new-session failed",
        restore::RestoreStep::InputValidation => "cache input rejected",
    }
}

/// Handles `orchard-tui refresh` — probes hosts, fetches remote data, writes
/// caches and snapshots, then exits.
///
/// This is the **only** entry point that makes SSH connections and writes
/// fresh data to disk. `orchard-tui --json` and the TUI cold-start both read
/// the caches written here. `orchard-tui watch` calls `refresh_and_build`
/// internally on its own schedule.
///
/// Flags:
/// - `--max-depth <n>`: override the maximum transitive federation depth
/// - `--per-hop-timeout <secs>`: override the per-hop SSH timeout in seconds
fn handle_refresh(args: &[String]) {
    use std::collections::HashSet;

    let mut max_depth: Option<u32> = None;
    let mut per_hop_timeout: Option<u64> = None;
    let mut skip_next = false;
    for (i, arg) in args.iter().enumerate() {
        if skip_next {
            skip_next = false;
            continue;
        }
        match arg.as_str() {
            "--max-depth" => {
                max_depth = args.get(i + 1).and_then(|v| v.parse().ok());
                skip_next = true;
            }
            "--per-hop-timeout" => {
                per_hop_timeout = args.get(i + 1).and_then(|v| v.parse().ok());
                skip_next = true;
            }
            _ => {}
        }
    }

    let config = global_config::load_global_config();
    let state =
        build_state::refresh_and_build_with_walker_config(&config, max_depth, per_hop_timeout);

    // Persist host reachability so subsequent cache-only reads
    // (--json, TUI cold start, watch daemon) can populate OrchardState.hosts.
    if let Err(e) = cache::write_host_reachability(&state.hosts) {
        logger::LOG.warn(&format!(
            "refresh: failed to persist host reachability: {e}"
        ));
    }

    // Count refreshed repos, unique remote hosts, and worktrees.
    let unique_hosts: HashSet<&str> = config
        .repos
        .iter()
        .flat_map(|r| r.remotes.iter().map(|rm| rm.host.as_str()))
        .collect();
    let remote_count = unique_hosts.len();
    let worktree_count: usize = state.repos.iter().map(|r| r.worktrees.len()).sum();

    eprintln!(
        "refreshed {} repos, {} remotes, {} worktrees",
        config.repos.len(),
        remote_count,
        worktree_count,
    );
}

/// Prints the committed JSON Schema for the `--json` wire format and exits.
///
/// The schema is embedded at compile time via `include_str!`, so it is always
/// in sync with the binary that emits it. Use `cargo build -p orchard` to
/// regenerate `schema.json` when the wire format changes.
fn handle_schema() {
    const SCHEMA: &str = include_str!("../schema.json");
    println!("{SCHEMA}");
}

/// Runs the TUI. If inside tmux and run directly (not via popup wrapper),
/// re-launches itself as a tmux popup using the wrapper script so that
/// session switching works correctly after the popup closes.
fn handle_tui(command: &str) {
    let inside_tmux = env::var("TMUX").is_ok();
    let is_tty = std::io::stdout().is_terminal();

    // If inside tmux and stdout is a TTY, we were run directly (not via popup).
    // Re-launch as a popup through the wrapper script.
    if inside_tmux && is_tty {
        let wrapper = dirs::home_dir()
            .map(|h| h.join(".local/bin/orchard-popup"))
            .unwrap_or_else(|| std::path::PathBuf::from("orchard-popup"));

        if wrapper.exists() {
            let mut cmd = std::process::Command::new("tmux");
            cmd.args([
                "display-popup",
                "-E",
                "-w",
                "90%",
                "-h",
                "80%",
                &wrapper.to_string_lossy(),
            ]);
            if !command.is_empty() {
                cmd.arg(command);
            }
            let _ = cmd.status();
        } else {
            eprintln!("Wrapper script not found. Run `orchard-tui init` first.");
            std::process::exit(1);
        }
        return;
    }

    // Inside popup (stdout captured) or outside tmux — run the TUI directly.
    match tui::run(command) {
        Ok(Some(session_name)) => {
            if inside_tmux {
                // Inside popup — print for wrapper to switch-client.
                println!("{session_name}");
            } else {
                // Outside tmux — attach to the session.
                let _ = std::process::Command::new("tmux")
                    .args(["attach-session", "-t", &session_name])
                    .status();
            }
        }
        Ok(None) => {}
        Err(e) => {
            eprintln!("{e}");
            std::process::exit(1);
        }
    }
}

/// Installs panic and error hooks that restore the terminal before printing
/// crash output, preventing terminal corruption when the TUI exits abnormally.
///
/// Must be called at the very start of `main()`, before any terminal setup.
fn install_panic_hooks() {
    let (panic_hook, eyre_hook) = color_eyre::config::HookBuilder::default().into_hooks();
    eyre_hook.install().expect("failed to install eyre hook");
    std::panic::set_hook(Box::new(move |info| {
        let _ = crossterm::execute!(std::io::stderr(), LeaveAlternateScreen);
        let _ = terminal::disable_raw_mode();
        let _ = crossterm::execute!(std::io::stderr(), cursor::Show);
        eprintln!("{}", panic_hook.panic_report(info));
    }));
}

/// Returns true iff the pretty panic hook should be installed.
///
/// `color_eyre::HookBuilder::default()` probes the terminal during install,
/// which opens `/dev/tty` and fails with ENXIO in any non-interactive
/// context (cron, systemd, CI runner, `ssh` with no `-t`). Batch commands
/// like `orchard-tui refresh` and `orchard-tui --json` must not require a
/// controlling TTY (AC7: background services never block or fail on terminal
/// absence), so we gate the install on `stderr` being a TTY. The default
/// Rust panic handler works fine for batch invocations — it just produces
/// less pretty output, which no one reads from cron anyway.
fn should_install_panic_hooks() -> bool {
    std::io::stderr().is_terminal()
}

fn print_usage() {
    eprintln!(
        r#"Usage:
  orchard-tui                        Interactive worktree manager (popup mode)
  orchard-tui init                   Interactive setup wizard for popup mode
  orchard-tui chat --message <msg>   Send a prompt to the orchardist tmux session
  orchard-tui chat [--target <s>] [--message <msg>]
                                       Default target: global_config.chat_target or first tmux_session
  orchard-tui setup-remote <host>    Provision a remote host for orchard
  orchard-tui heal                   Audit and repair drifted state (dry run)
  orchard-tui heal --fix             Apply all safe automatic repairs
  orchard-tui heal --json            Output health check results as JSON
  orchard-tui sessions --json        Print every tmux session across all managed hosts
                                       (worktree-attached, orphan, detached-claude, or
                                       protected) — host-tagged at the data plane.
                                       Live read: refreshes every reachable remote
                                       tmux source over SSH plus the local tmux state
                                       before classifying.
  orchard-tui refresh                Probe hosts, fetch from remotes, update caches, exit.
                                       Hot-loads the same caches that `orchard-tui --json`
                                       and `orchard-tui sessions --json` already refresh;
                                       useful when you want the side effects (warmed
                                       caches for the TUI cold start) without the JSON
                                       output. Use `orchard-tui watch` for a continuous
                                       background refresh.
  orchard-tui watch                  Run event-driven watch daemon (Ctrl-C to stop)
  orchard-tui watch --keep-diagnostic-caches
                                       Also write orphaned *_issues.json + *_prs.json
                                       caches (debug only; not consumed by the TUI
                                       post-#429, which reads from daemon WorkView).
  orchard-tui watch --subscribe --id <id> --session <session> [--pane <pane>]
                                       Register a tmux subscriber for watch events
  orchard-tui watch --unsubscribe --id <id>
                                       Unregister a tmux subscriber
  orchard-tui webhook-serve [--port <n>]
                                       Receive GitHub webhooks and append to events.jsonl
                                       Requires ORCHARD_WEBHOOK_SECRET env var
  orchard-tui restore                Reconstruct dead tmux sessions from the local
                                       cache. Read paths never resurrect killed
                                       sessions — this is the only deliberate path.

Domain subcommands (L1/L6 — standalone, no daemon required):
  orchard-tui gh            <op> [flags]  GitHub operations (gh CLI wrapper)
  orchard-tui git           <op> [flags]  Git operations (worktree-create/remove/move, fetch, pull, push)
  orchard-tui tmux          <op> [flags]  tmux operations (send-text)
  orchard-tui ps            <op> [flags]  Process table operations
  orchard-tui host-services <op> [flags]  Service lifecycle (start/stop/restart/status)
  orchard-tui claude-account <op> [flags] Claude CLI / ccusage operations
  orchard-tui daemon        <op>           Daemon lifecycle (start/stop/status/reload)

  Each domain also accepts `raw [-- <tool-args...>]` as a pass-through escape hatch (S16b).
  Run `orchard-tui <domain>` with no subcommand for domain-specific usage.

Options:
  --version, -V  Print version and exit
  --json         Output worktree data as JSON and exit. Live read — performs
                 the same refresh as `orchard-tui refresh` (SSH probes, remote
                 worktree + tmux fetches, local git/tmux re-stat, GitHub
                 issue/PR refresh) before serialising. The TUI uses a cache
                 path; --json does not.
  --schema       Print the JSON Schema for --json output and exit

Navigation:
  1-9     Jump to worktree by number
  ↑/↓     Select worktree
  Enter/t tmux into worktree (creates session if needed, then exits)
  d       Delete selected worktree
  c       Cleanup merged worktrees
  h       Run heal check
  r       Refresh list
  q/Esc   Close popup (no switch)

Keybindings (after orchard-tui init --install):
  prefix + o   Open orchard TUI popup
  prefix + O   Quick-chat to orchardist"#
    );
}

/// Handles the `orchard-tui watch` command.
///
/// Without flags: runs the event-driven daemon loop (Ctrl-C to stop).
/// With `--subscribe`: registers a tmux subscriber.
/// With `--unsubscribe`: removes a subscriber.
fn handle_watch(args: &[String]) {
    let mut config = global_config::load_global_config();

    let mut subscribe = false;
    let mut unsubscribe = false;
    let mut id = String::new();
    let mut session = String::new();
    let mut pane = "0.0".to_string();
    let mut skip_next = false;

    for (i, arg) in args.iter().enumerate() {
        if skip_next {
            skip_next = false;
            continue;
        }
        match arg.as_str() {
            "--subscribe" => subscribe = true,
            "--unsubscribe" => unsubscribe = true,
            "--keep-diagnostic-caches" => config.watch.keep_diagnostic_caches = true,
            "--id" => {
                id = args.get(i + 1).cloned().unwrap_or_default();
                skip_next = true;
            }
            "--session" => {
                session = args.get(i + 1).cloned().unwrap_or_default();
                skip_next = true;
            }
            "--pane" => {
                pane = args.get(i + 1).cloned().unwrap_or_default();
                skip_next = true;
            }
            _ => {}
        }
    }

    if subscribe {
        if id.is_empty() || session.is_empty() {
            eprintln!(
                "Usage: orchard-tui watch --subscribe --id <id> --session <session> [--pane <pane>]"
            );
            std::process::exit(1);
        }
        match orchard::watch::subscription::register(&id, &session, &pane) {
            Ok(()) => eprintln!("Subscribed: {id}"),
            Err(e) => {
                eprintln!("{e}");
                std::process::exit(1);
            }
        }
        return;
    }

    if unsubscribe {
        if id.is_empty() {
            eprintln!("Usage: orchard-tui watch --unsubscribe --id <id>");
            std::process::exit(1);
        }
        match orchard::watch::subscription::unregister(&id) {
            Ok(()) => eprintln!("Unsubscribed: {id}"),
            Err(e) => {
                eprintln!("{e}");
                std::process::exit(1);
            }
        }
        return;
    }

    // Default: run the daemon.
    if let Err(e) = orchard::watch::daemon::run(&config) {
        eprintln!("{e}");
        std::process::exit(1);
    }
}

/// Handles the `orchard-tui webhook-serve` command.
///
/// Parses `--port <n>`, resolves the final port via
/// `webhook::port::resolve_port(flag, env, config)`, validates
/// ORCHARD_WEBHOOK_SECRET is set and non-empty, and runs the hyper server.
fn handle_webhook_serve(args: &[String]) {
    // 1. Parse --port flag from args.
    let mut port_flag: Option<u16> = None;
    let mut skip_next = false;
    for (i, arg) in args.iter().enumerate() {
        if skip_next {
            skip_next = false;
            continue;
        }
        if arg == "--port"
            && let Some(v) = args.get(i + 1)
        {
            port_flag = v.parse::<u16>().ok();
            skip_next = true;
        }
    }

    // 2. Read ORCHARD_WEBHOOK_PORT env var.
    let port_env: Option<u16> = env::var("ORCHARD_WEBHOOK_PORT")
        .ok()
        .and_then(|v| v.parse::<u16>().ok());

    // 3. Load global config, read config.watch.webhook_port.
    let config = global_config::load_global_config();
    let port_config: Option<u16> = config.watch.webhook_port;

    // 4. Resolve port.
    let port = orchard::webhook::port::resolve_port(port_flag, port_env, port_config);

    // 5. Read ORCHARD_WEBHOOK_SECRET env var.
    let secret = match env::var("ORCHARD_WEBHOOK_SECRET") {
        Ok(s) if !s.is_empty() => s,
        _ => {
            eprintln!(
                "webhook-serve: ORCHARD_WEBHOOK_SECRET is not set. \
                 Set it to your GitHub webhook secret."
            );
            std::process::exit(1);
        }
    };

    // 6. Run the server.
    if let Err(e) = orchard::webhook::server::run(port, secret.into_bytes()) {
        eprintln!("{e}");
        std::process::exit(1);
    }
}

#[cfg(test)]
mod tests {
    fn should_exit(message: Option<&str>) -> bool {
        !matches!(message, Some(m) if !m.is_empty())
    }

    #[test]
    fn handle_chat_missing_message_exits_nonzero() {
        // We can't call handle_chat directly (it calls process::exit), but we can
        // verify the guard logic by checking the predicate.
        // The production path for None/empty is tested via binary integration.
        // This test documents the expected behaviour contract.
        assert!(
            should_exit(None),
            "missing message must trigger non-zero exit"
        );
    }

    #[test]
    fn handle_chat_empty_string_message_exits_nonzero() {
        assert!(
            should_exit(Some("")),
            "empty message must trigger non-zero exit"
        );
    }

    #[test]
    fn handle_chat_nonempty_message_does_not_exit() {
        assert!(
            !should_exit(Some("hello")),
            "non-empty message must not trigger exit"
        );
    }
}
