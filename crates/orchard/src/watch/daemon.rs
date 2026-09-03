//! Watch daemon loop.
//!
//! Polls local and full sources on configured intervals, diffs the resulting
//! `OrchardState`, delivers events to subscribers, and optionally sends
//! desktop notifications for key events.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

use crate::cache_sources;
use crate::events::events_path;
use crate::global_config::GlobalConfig;
use crate::merge_remote;
use crate::orchard_state::OrchardState;
use crate::watch::diff;
use crate::watch::event::{EventKind, WatchEvent};
use crate::watch::subscription;
use crate::webhook::tailer::Tailer;

// ---------------------------------------------------------------------------
// Public entry point
// ---------------------------------------------------------------------------

/// Runs the watch daemon until interrupted by Ctrl-C.
///
/// Polls local sources every `config.watch.local_poll_secs` seconds and full
/// sources every `config.watch.full_poll_secs` seconds. On each cycle it diffs
/// the state, delivers events to subscribers, and optionally sends desktop
/// notifications.
pub fn run(config: &GlobalConfig) -> anyhow::Result<()> {
    let running = Arc::new(AtomicBool::new(true));
    let r = running.clone();
    ctrlc::set_handler(move || {
        r.store(false, Ordering::SeqCst);
    })?;

    eprintln!(
        "orchard watch: starting daemon (local={}s, full={}s)",
        config.watch.local_poll_secs, config.watch.full_poll_secs
    );

    let mut claude_debounce = crate::watch::debounce::ClaudeDebounceState::new();
    let mut last_local = Instant::now();
    let mut last_full = Instant::now();

    // Tailer tracks new webhook lines in events.jsonl and forces an immediate
    // full refresh when any arrive. The 1s sleep + tailer check guarantees
    // webhook-triggered refreshes happen within ~2s of the append (AC #35).
    let mut tailer = Tailer::new(events_path());

    // Force a full refresh on startup.
    refresh_all_sources(config, config.watch.keep_diagnostic_caches);
    let hosts = crate::cache::read_host_reachability();
    let initial = merge_remote::build_state_with_cached_snapshots(config, &hosts);
    let mut previous_state: Option<OrchardState> = Some(initial);

    while running.load(Ordering::SeqCst) {
        std::thread::sleep(Duration::from_secs(1));

        let now = Instant::now();
        // Webhook lines force an immediate full refresh regardless of poll
        // intervals. Multiple lines arriving between iterations collapse to
        // one refresh (AC #36 debounce).
        let webhook_fired = webhook_triggered_refresh(&mut tailer);
        let do_full =
            webhook_fired || now.duration_since(last_full).as_secs() >= config.watch.full_poll_secs;
        let do_local = now.duration_since(last_local).as_secs() >= config.watch.local_poll_secs;

        if do_full {
            refresh_all_sources(config, config.watch.keep_diagnostic_caches);
            last_full = now;
            last_local = now;
        } else if do_local {
            refresh_local_sources(config);
            last_local = now;
        } else {
            continue;
        }

        let hosts = crate::cache::read_host_reachability();
        let new_state = merge_remote::build_state_with_cached_snapshots(config, &hosts);

        // Diff
        let mut events: Vec<WatchEvent> = Vec::new();
        if let Some(ref old) = previous_state {
            events.extend(diff::diff(old, &new_state, &mut claude_debounce));
        }

        previous_state = Some(new_state.clone());

        // Emit a heartbeat on full refresh cycles when no other events fired.
        if do_full && events.is_empty() {
            let worktree_count = new_state.repos.iter().map(|r| r.worktrees.len()).sum();
            let session_count = new_state
                .repos
                .iter()
                .flat_map(|r| r.worktrees.iter())
                .map(|wt| wt.sessions.len())
                .sum::<usize>()
                + new_state.standalone_sessions.len();
            events.push(WatchEvent::now(EventKind::Heartbeat {
                worktree_count,
                session_count,
            }));
        }

        if events.is_empty() {
            continue;
        }

        // Log events
        for event in &events {
            log_watch_event(event);
        }

        // Desktop notifications
        if config.watch.notifications {
            fire_notifications(&events, config);
        }

        // Deliver to subscribers
        let subs_file = subscription::read_subscriptions();
        let pruned = subscription::prune_stale(&subs_file);
        // Persist the pruned list so dead sessions are removed on disk.
        if let Err(e) = subscription::write_subscriptions(&pruned) {
            crate::logger::LOG.warn(&format!(
                "watch: failed to persist pruned subscriptions: {e}"
            ));
        }
        for sub in &pruned.subscriptions {
            if let Err(e) = subscription::deliver(sub, &events) {
                crate::logger::LOG.warn(&format!("watch: delivery to {} failed: {e}", sub.id));
            }
        }
    }

    eprintln!("orchard watch: stopped");
    Ok(())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/// Refreshes worktrees, tmux sessions, and the transitive federation walker.
///
/// **Diagnostic caches** (`*_issues.json` + `*_prs.json`) are gated behind
/// `keep_diagnostic_caches`. Post-#429 the TUI dashboard reads issues + PRs
/// from `daemon::Client::work_view`, so the cache files are orphans for the
/// TUI consumer. Other consumers (`--json` mode, `heal`) still read
/// `*_worktrees.json` + `tmux_sessions.json`, which is why those refresh
/// calls remain unconditional.
///
/// Per-repo refreshes fan out concurrently so one slow GitHub API response
/// can't delay the next repo.
pub(crate) fn refresh_all_sources(config: &GlobalConfig, keep_diagnostic_caches: bool) {
    crate::refresh_parallel::for_each_repo_parallel(config, |repo| {
        if keep_diagnostic_caches {
            if let Err(e) = cache_sources::refresh_issues(repo) {
                crate::logger::LOG.warn(&format!("watch: refresh issues failed: {e}"));
            }
            if let Err(e) = cache_sources::refresh_prs(repo) {
                crate::logger::LOG.warn(&format!("watch: refresh PRs failed: {e}"));
            }
        }
        if let Err(e) = cache_sources::refresh_worktrees(repo) {
            crate::logger::LOG.warn(&format!("watch: refresh worktrees failed: {e}"));
        }
    });
    if let Err(e) = cache_sources::refresh_tmux_sessions(None) {
        crate::logger::LOG.warn(&format!("watch: refresh tmux sessions failed: {e}"));
    }

    // Run the transitive federation walker so depth-2+ remotes are written to
    // cache and picked up by the subsequent `build_state_with_cached_snapshots`.
    refresh_transitive_federation(config);
}

/// Runs the transitive federation walker for all `allow_transitive=true`
/// OrchardProxy roots in the config, writing per-host snapshot files and
/// updating `federation_topology.json`.
///
/// Called exclusively from `refresh_all_sources` (full-refresh cycle). The
/// written snapshots are then read back by `build_state_with_cached_snapshots`
/// on the next state build.
fn refresh_transitive_federation(config: &GlobalConfig) {
    use crate::remote_adapter::{ProcessSshExec, RemoteKind};
    use crate::transitive_walker::{WalkerConfig, walk};
    use std::collections::HashSet;

    let transitive_roots: Vec<(String, bool)> = {
        let mut seen = HashSet::new();
        config
            .repos
            .iter()
            .flat_map(|r| r.remotes.iter())
            .filter(|rm| rm.kind == RemoteKind::OrchardProxy && seen.insert(rm.host.clone()))
            .map(|rm| (rm.host.clone(), rm.allow_transitive))
            .collect()
    };

    if transitive_roots.is_empty() {
        return;
    }

    let roots_ref: Vec<(&str, bool)> = transitive_roots
        .iter()
        .map(|(h, a)| (h.as_str(), *a))
        .collect();

    let ssh = Arc::new(ProcessSshExec) as Arc<dyn crate::remote_adapter::SshExec>;
    let walker_cfg = WalkerConfig::new(ssh);
    let walker_result = walk(&roots_ref, &walker_cfg);

    // Log walker errors but don't abort — partial results are still useful.
    for err in &walker_result.errors {
        crate::logger::LOG.warn(&format!(
            "watch: transitive federation error for {} ({}:{}): {}",
            err.dedup_key,
            err.phase,
            err.reason,
            err.discovery_path.join(" → ")
        ));
    }

    // Write per-host snapshots and collect topology entries.
    let mut topology_entries: Vec<(Vec<String>, String)> = Vec::new();
    for (discovery_path, snapshot) in &walker_result.snapshots {
        if discovery_path.len() > 2 {
            // depth-2+: write snapshot to cache.
            let host = discovery_path.last().cloned().unwrap_or_default();
            let dedup_key =
                crate::federation::host_dedup_key(&host).unwrap_or_else(|_| host.clone());
            let _ = crate::orchard_snapshot::write_snapshot(&host, snapshot);
            topology_entries.push((discovery_path.clone(), dedup_key));
        }
    }

    // Persist topology so the next cold-start reads the transitive hosts.
    if !topology_entries.is_empty() {
        let topology = crate::federation_topology::build_topology(&topology_entries);
        let _ = crate::federation_topology::write_topology(&topology);

        // GC snapshots that are no longer in the topology.
        let topology_read = crate::federation_topology::read_topology();
        crate::federation_topology::gc_orphan_snapshots(topology_read.as_ref(), config);
    }
}

/// Refreshes only local (fast) sources: worktrees and tmux sessions.
///
/// Intentionally serial: each `refresh_worktrees` is a local `git worktree
/// list` — single-digit milliseconds. Thread-spawn overhead would cost more
/// than it would save. `refresh_all_sources` is the hot path that needs
/// parallelism.
fn refresh_local_sources(config: &GlobalConfig) {
    for repo in &config.repos {
        if let Err(e) = cache_sources::refresh_worktrees(repo) {
            crate::logger::LOG.warn(&format!("watch: refresh worktrees failed: {e}"));
        }
    }
    if let Err(e) = cache_sources::refresh_tmux_sessions(None) {
        crate::logger::LOG.warn(&format!("watch: refresh tmux sessions failed: {e}"));
    }
}

/// Sends desktop notifications for key event kinds.
pub fn fire_notifications(events: &[WatchEvent], config: &GlobalConfig) {
    let terminal_app = config.terminal_app.as_str();
    for event in events {
        if let Some((title, message, session)) = event.kind.notification() {
            crate::notify::send_notification_with_session(title, &message, session, terminal_app);
        }
    }
}

/// Returns true if the tailer found new webhook lines that should force an
/// immediate full refresh. Always returns false when the tailer sees nothing.
///
/// Multiple webhook lines arriving between iterations all collapse into a
/// single `true` return (AC #36 debounce): we call `tailer.poll()` once,
/// collect all new lines, and return `!lines.is_empty()`.
fn webhook_triggered_refresh(tailer: &mut Tailer) -> bool {
    !tailer.poll().is_empty()
}

/// Logs a watch event to the structured event log.
///
/// Extracts the `type` field from the serialized tagged enum instead of
/// maintaining a manual match table.
fn log_watch_event(event: &WatchEvent) {
    let details = serde_json::to_string(&event.kind).unwrap_or_default();
    let event_type = serde_json::from_str::<serde_json::Value>(&details)
        .ok()
        .and_then(|v| v.get("type").and_then(|t| t.as_str().map(String::from)))
        .unwrap_or_else(|| "unknown".to_string());
    crate::events::log_watch_event(&event_type, &details);
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use tempfile::NamedTempFile;

    fn webhook_line(kind: &str) -> String {
        format!(
            r#"{{"source":"webhook","kind":"{}","ts":"2024-01-01T00:00:00Z","data":{{}}}}"#,
            kind
        )
    }

    /// AC #35: webhook helper returns true when a new webhook line is present.
    /// The daemon loop wires this into do_full=true, so a refresh happens within
    /// the next 1s sleep iteration — well within the 2s guarantee.
    #[test]
    fn webhook_triggered_refresh_returns_true_when_line_present() {
        let mut f = NamedTempFile::new().unwrap();
        let mut tailer = Tailer::new(f.path().to_path_buf());

        writeln!(f, "{}", webhook_line("pull_request.opened")).unwrap();
        f.flush().unwrap();

        assert!(
            webhook_triggered_refresh(&mut tailer),
            "helper returns true when webhook line appended"
        );
    }

    /// AC #36: multiple webhook lines between iterations debounce to one refresh.
    /// `webhook_triggered_refresh` drains all new lines in one poll call and
    /// returns a single bool — the daemon only calls refresh_all_sources once.
    #[test]
    fn webhook_triggered_refresh_debounces_multiple_lines() {
        let mut f = NamedTempFile::new().unwrap();
        let mut tailer = Tailer::new(f.path().to_path_buf());

        for _ in 0..5 {
            writeln!(f, "{}", webhook_line("push")).unwrap();
        }
        f.flush().unwrap();

        // All 5 lines consumed in one call; helper returns true (not 5 trues).
        assert!(
            webhook_triggered_refresh(&mut tailer),
            "5 lines still one true"
        );
        // Subsequent poll finds nothing — offset advanced past all 5.
        assert!(
            !webhook_triggered_refresh(&mut tailer),
            "offset advanced past all lines"
        );
    }

    /// AC #37: missing events.jsonl → helper returns false, daemon falls back to
    /// poll-only intervals.
    #[test]
    fn fallback_when_events_file_missing() {
        let path = std::env::temp_dir().join("orchard_daemon_test_missing_events.jsonl");
        let _ = std::fs::remove_file(&path);

        let mut tailer = Tailer::new(path.clone());
        assert!(
            !webhook_triggered_refresh(&mut tailer),
            "missing file → false, no panic"
        );
        assert!(
            !webhook_triggered_refresh(&mut tailer),
            "still false on repeat"
        );
    }

    // -------------------------------------------------------------------------
    // AC8 — diagnostic cache gate
    // -------------------------------------------------------------------------
    //
    // These tests use a fake `gh` binary injected via PATH and a redirected HOME
    // (so cache_dir() resolves into a tempdir) to observe whether refresh_issues
    // and refresh_prs are called by refresh_all_sources.
    //
    // The crate-wide env lock serialises the environment-variable writes so
    // parallel test threads don't clobber each other's HOME or PATH.

    use crate::test_support::env_lock;

    /// Builds a minimal GlobalConfig with one test repo at `repo_path`.
    fn make_test_config(slug: &str, repo_path: &str) -> GlobalConfig {
        use crate::global_config::{RepoConfig, WatchConfig};
        GlobalConfig {
            repos: vec![RepoConfig {
                slug: slug.to_owned(),
                path: repo_path.to_owned(),
                remotes: vec![],
            }],
            watch: WatchConfig {
                keep_diagnostic_caches: false,
                ..WatchConfig::default()
            },
            ..GlobalConfig::default()
        }
    }

    /// Creates a fake `gh` shell script in `bin_dir` that writes a marker file
    /// to `marker_path` each time it is invoked, then exits 1 (simulating an
    /// API failure so cache_sources doesn't attempt to parse output).
    #[cfg(unix)]
    fn write_fake_gh(bin_dir: &std::path::Path, marker_path: &std::path::Path) {
        use std::os::unix::fs::PermissionsExt;
        let script = format!("#!/bin/sh\ntouch \"{}\"\nexit 1\n", marker_path.display());
        let script_path = bin_dir.join("gh");
        std::fs::write(&script_path, script).unwrap();
        std::fs::set_permissions(&script_path, std::fs::Permissions::from_mode(0o755)).unwrap();
    }

    /// AC8 (false branch): `refresh_all_sources` with `keep_diagnostic_caches: false`
    /// must NOT invoke `refresh_issues` or `refresh_prs` — verified by confirming
    /// the fake `gh` binary is never called.
    #[test]
    #[cfg(unix)]
    fn watch_does_not_write_orphaned_issue_pr_caches_by_default() {
        let _guard = env_lock();

        let home_dir = tempfile::tempdir().unwrap();
        let bin_dir = tempfile::tempdir().unwrap();
        let gh_marker = home_dir.path().join("gh_was_called");

        write_fake_gh(bin_dir.path(), &gh_marker);

        // Override HOME so cache_dir() resolves into our tempdir.
        // Override PATH so our fake `gh` is found before the real one.
        let old_home = std::env::var("HOME").ok();
        let old_path = std::env::var("PATH").ok();
        unsafe {
            std::env::set_var("HOME", home_dir.path());
            std::env::set_var(
                "PATH",
                format!(
                    "{}:{}",
                    bin_dir.path().display(),
                    old_path.as_deref().unwrap_or("/usr/bin:/bin")
                ),
            );
        }

        let config = make_test_config("testwatch/diag-false", home_dir.path().to_str().unwrap());

        // Call with keep_diagnostic_caches: false — refresh_issues/refresh_prs
        // must not be invoked.
        refresh_all_sources(&config, false);

        // Restore environment before any assertion (so we don't leave a dirty
        // state if the assertion panics).
        unsafe {
            match old_home {
                Some(v) => std::env::set_var("HOME", v),
                None => std::env::remove_var("HOME"),
            }
            match old_path {
                Some(v) => std::env::set_var("PATH", v),
                None => std::env::remove_var("PATH"),
            }
        }

        // The fake `gh` must NOT have been invoked — its marker file must not
        // exist. If it does, the gate is broken.
        assert!(
            !gh_marker.exists(),
            "refresh_all_sources(false) must not call `gh` — \
             found marker written by the fake gh binary at {:?}",
            gh_marker
        );

        // The orphaned cache files must not have been created.
        let cache_base = home_dir.path().join(".cache/orchard");
        let issues_path = cache_base.join("testwatch_diag-false_issues.json");
        let prs_path = cache_base.join("testwatch_diag-false_prs.json");
        assert!(
            !issues_path.exists(),
            "issues cache must not be written when keep_diagnostic_caches=false"
        );
        assert!(
            !prs_path.exists(),
            "prs cache must not be written when keep_diagnostic_caches=false"
        );
    }

    /// AC8 (true branch): `refresh_all_sources` with `keep_diagnostic_caches: true`
    /// DOES invoke `refresh_issues` and `refresh_prs` — verified by confirming
    /// the fake `gh` binary is called (it exits 1 to simulate an API failure,
    /// so no real cache write happens, but the call itself is observable).
    #[test]
    #[cfg(unix)]
    fn keep_diagnostic_caches_flag_reenables_orphan_writes_for_debugging() {
        let _guard = env_lock();

        let home_dir = tempfile::tempdir().unwrap();
        let bin_dir = tempfile::tempdir().unwrap();
        let gh_marker = home_dir.path().join("gh_was_called");

        write_fake_gh(bin_dir.path(), &gh_marker);

        let old_home = std::env::var("HOME").ok();
        let old_path = std::env::var("PATH").ok();
        unsafe {
            std::env::set_var("HOME", home_dir.path());
            std::env::set_var(
                "PATH",
                format!(
                    "{}:{}",
                    bin_dir.path().display(),
                    old_path.as_deref().unwrap_or("/usr/bin:/bin")
                ),
            );
        }

        // Pre-write a Fresh worktrees cache containing a branch that has an issue
        // number, so refresh_issues finds issue_numbers and actually calls `gh`.
        let cache_dir = home_dir.path().join(".cache/orchard");
        std::fs::create_dir_all(&cache_dir).unwrap();
        let wt_cache = cache_dir.join("testwatch_diag-true_worktrees.json");
        // A minimal valid worktrees cache entry with an issue branch.
        let wt_json = r#"{"last_refreshed":"2099-01-01T00:00:00Z","entries":[{"branch":"issue42/my-feature","path":"/tmp/test-wt","is_bare":false,"is_locked":false}]}"#;
        std::fs::write(&wt_cache, wt_json).unwrap();

        let config = make_test_config("testwatch/diag-true", home_dir.path().to_str().unwrap());

        // Call with keep_diagnostic_caches: true — refresh_issues/refresh_prs
        // must be invoked (even though they'll fail due to fake gh).
        refresh_all_sources(&config, true);

        unsafe {
            match old_home {
                Some(v) => std::env::set_var("HOME", v),
                None => std::env::remove_var("HOME"),
            }
            match old_path {
                Some(v) => std::env::set_var("PATH", v),
                None => std::env::remove_var("PATH"),
            }
        }

        // The fake `gh` must have been invoked — its marker file must exist.
        // This proves refresh_issues (and/or refresh_prs) reached the point
        // of spawning the gh process.
        assert!(
            gh_marker.exists(),
            "refresh_all_sources(true) must call `gh` for issue/PR refresh — \
             marker written by fake gh was not found at {:?}",
            gh_marker
        );
    }
}
