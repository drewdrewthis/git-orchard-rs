//! Adapter: maps a [`WorkViewSnapshot`] delivered by the daemon into the local
//! portion of an [`OrchardState`].
//!
//! # Responsibilities
//!
//! - Converts [`WorkViewRepo`] / [`WorkViewWorktree`] into cache-shaped tuples
//!   that [`crate::join::derive_all_repos`] can consume, reusing its full join
//!   pipeline (PR/issue linking, display group derivation, session enrichment).
//! - Performs the **client-side** sessions↔claude join by extracting the tmux
//!   session name from [`ClaudeInstance::pane`]
//!   (`TmuxPane:<host>:<session>:<window>:<index>`), converting each instance
//!   into a [`crate::claude_state::ClaudeStateFile`], and passing them through
//!   the existing [`crate::join::derive_all_repos`] / [`crate::classify`] path.
//! - Does **not** read disk caches and does **not** call `cache_sources::*`.
//!
//! # Session-to-worktree matching
//!
//! [`WorkViewTmuxSession`] carries an optional `path` (working directory). When
//! present it is used directly with [`crate::paths::session_belongs_to_worktree`].
//! When absent the adapter synthesises a minimal [`crate::cache::CachedTmuxSession`]
//! with an empty path; sessions with no path will not match any worktree and
//! will surface as standalone sessions — consistent with conservative behaviour.
//!
//! # Output
//!
//! Returns a **partial** [`OrchardState`] containing only LOCAL data
//! (`host == None` for local worktrees). Remote enrichment is folded in
//! separately via [`crate::merge_remote::merge_remote_snapshot`].

use std::collections::HashMap;

use crate::cache::{CachedIssue, CachedPr, CachedTmuxSession, CachedWorktree, WorktreeLayout};
use crate::claude_state::ClaudeStateFile;
use crate::daemon::types::{ClaudeInstance, WorkViewSnapshot};
use crate::global_config::GlobalConfig;
use crate::join::RepoCacheEntry;
use crate::orchard_state::{HostState, OrchardState, RepoState, WorktreeState};
use crate::session::{
    EnrichedSession, Host, SessionStatus, StandaloneConfig, StandaloneSessionRow, TmuxSessionInfo,
};

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

/// Builds the LOCAL portion of an [`OrchardState`] from a [`WorkViewSnapshot`].
///
/// Bypasses `cache_sources::refresh_*` entirely — the daemon has already
/// performed the PR/issue join server-side. This function:
///
/// 1. Converts `WorkView` types into cache-shaped tuples.
/// 2. Calls [`crate::join::derive_all_repos`] to reuse the full client-side
///    join pipeline (sessions↔claude, display group, sort key).
/// 3. Assembles per-repo [`RepoState`]s and standalone sessions.
///
/// The output is a partial [`OrchardState`] containing only LOCAL data.
/// Remote enrichment (`*_remote_worktrees.json` / `*_remote_tmux_sessions.json`)
/// is folded in by the existing `merge_remote::merge_remote_snapshot` path.
///
/// Ahead/behind counts come from `WorkViewWorktree.ahead` / `.behind` (daemon
/// issue #483) — populated server-side by the git provider.
pub fn build_local_state(
    snapshot: &WorkViewSnapshot,
    config: &GlobalConfig,
    hosts: &HashMap<String, HostState>,
) -> OrchardState {
    // 1. Convert ClaudeInstances → ClaudeStateFiles (indexed by session name).
    let claude_states: Vec<ClaudeStateFile> = snapshot
        .claude_instances
        .iter()
        .filter_map(claude_instance_to_state_file)
        .collect();

    // 2. Build RepoCacheEntry tuples from WorkViewRepos.
    //    Each repo may contain worktrees from multiple GitHub repos (when the
    //    working tree hosts multiple remotes — uncommon but handled). Group
    //    by GitHub slug derived from the worktree's `repo` field.
    let mut repo_entries: HashMap<String, RepoCacheEntry> = HashMap::new();

    for repo in &snapshot.repos {
        for wt in &repo.worktrees {
            let slug = wt.repo.clone();
            let entry = repo_entries
                .entry(slug.clone())
                .or_insert_with(|| (slug, Vec::new(), Vec::new(), Vec::new(), Vec::new()));

            // Issues: deduplicate by number.
            if let Some(ref issue) = wt.issue {
                let num = issue.number as u32;
                if !entry.1.iter().any(|i: &CachedIssue| i.number == num) {
                    entry.1.push(work_view_issue_to_cached(issue));
                }
            }

            // PRs: deduplicate by number.
            if let Some(ref pr) = wt.pr {
                let num = pr.number as u32;
                if !entry.2.iter().any(|p: &CachedPr| p.number == num) {
                    let linked_issue_num = wt.issue.as_ref().map(|i| i.number as u32);
                    entry
                        .2
                        .push(work_view_pr_to_cached(pr, &wt.branch, linked_issue_num));
                }
            }

            // Worktree entry. Ahead/behind come from the daemon (#483).
            entry.3.push(work_view_worktree_to_cached(wt));
        }
    }

    // 3. Populate sessions across all repo entries.
    //    Sessions are not per-repo; they live globally on the host.
    //    Inject all local sessions into every repo entry so `derive_worktree_rows`
    //    can match each session to its worktree.
    let sessions: Vec<CachedTmuxSession> = snapshot
        .tmux_sessions
        .iter()
        .map(work_view_session_to_cached)
        .collect();

    for entry in repo_entries.values_mut() {
        entry.4 = sessions.clone();
    }

    // 4. Derive worktree rows using the full join pipeline.
    let repo_caches: Vec<RepoCacheEntry> = repo_entries.into_values().collect();
    let rows = crate::join::derive_all_repos(&repo_caches, &claude_states, &[]);

    // 5. Group rows into RepoStates, preserving config ordering where possible.
    let mut repo_map: HashMap<String, Vec<WorktreeState>> = HashMap::new();
    for row in &rows {
        repo_map
            .entry(row.repo_slug.clone())
            .or_default()
            .push(WorktreeState::from(row));
    }

    // Follow config ordering for repos present in config; append extras at end.
    let mut repos: Vec<RepoState> = config
        .repos
        .iter()
        .filter_map(|r| {
            repo_map.remove(&r.slug).map(|worktrees| RepoState {
                slug: r.slug.clone(),
                worktrees,
                default_branch: None,
                main_ci_state: None,
            })
        })
        .collect();

    // Repos in the snapshot that are NOT in config (e.g. freshly added projects).
    for (slug, worktrees) in repo_map {
        repos.push(RepoState {
            slug,
            worktrees,
            default_branch: None,
            main_ci_state: None,
        });
    }

    // 6. Build standalone sessions.
    let all_worktree_paths: Vec<String> = repos
        .iter()
        .flat_map(|r| r.worktrees.iter().map(|w| w.path.clone()))
        .collect();
    let wt_path_refs: Vec<&str> = all_worktree_paths.iter().map(|s| s.as_str()).collect();

    let standalone_sessions =
        build_standalone_sessions(config, &sessions, &claude_states, &wt_path_refs);

    OrchardState {
        repos,
        standalone_sessions,
        hosts: hosts.clone(),
        transitive_errors: Vec::new(),
    }
}

// ---------------------------------------------------------------------------
// Standalone session builder
// ---------------------------------------------------------------------------

/// Builds [`StandaloneSessionRow`]s from sessions that don't match any worktree.
///
/// A session is standalone when its working directory (if known) is not inside
/// any worktree path, AND its name is not already matched to a worktree.
fn build_standalone_sessions(
    config: &GlobalConfig,
    sessions: &[CachedTmuxSession],
    claude_states: &[ClaudeStateFile],
    worktree_paths: &[&str],
) -> Vec<StandaloneSessionRow> {
    use crate::session::{ClaudeSessionInfo, PaneColumns, build_windows_and_panes};

    let configured_names: std::collections::HashSet<&str> = config
        .tmux_sessions
        .iter()
        .map(|c| c.name.as_str())
        .collect();

    let mut rows: Vec<StandaloneSessionRow> = Vec::new();

    // Emit configured standalone sessions first (in config order).
    for cfg in &config.tmux_sessions {
        let live = sessions.iter().find(|s| s.name == cfg.name);
        let status = if live.is_some() {
            SessionStatus::Running { attached: false }
        } else {
            SessionStatus::Dead
        };

        let claude = claude_states
            .iter()
            .find(|cs| cs.tmux_session == cfg.name)
            .filter(|cs| !crate::derive::is_state_stale_default(&cs.timestamp))
            .and_then(ClaudeSessionInfo::from_state_file);

        let (windows, panes) = live
            .map(|s| build_windows_and_panes(PaneColumns::from_cached(s)))
            .unwrap_or_default();

        let started_at = live.and_then(|s| s.created_at);
        let last_activity_at = live.and_then(|s| s.last_activity_at);

        rows.push(StandaloneSessionRow {
            session: EnrichedSession {
                tmux: TmuxSessionInfo {
                    host: Host::Local,
                    name: cfg.name.clone(),
                    status,
                },
                claude,
                windows,
                panes,
                started_at,
                last_activity_at,
            },
            config: cfg.clone(),
        });
    }

    // Discover standalone sessions not in config that don't match any worktree.
    for session in sessions {
        if configured_names.contains(session.name.as_str()) {
            continue;
        }

        // If the session has a path, check whether it belongs to a worktree.
        let inside_worktree = worktree_paths
            .iter()
            .any(|wt_path| crate::paths::session_belongs_to_worktree(&session.path, wt_path));
        if inside_worktree {
            continue;
        }

        // Session with no path ("") never matches any worktree. If a session
        // genuinely belongs to a worktree it must have been matched above by
        // the derive pipeline; unmatched sessions with empty paths are standalone.

        let claude = claude_states
            .iter()
            .find(|cs| cs.tmux_session == session.name)
            .filter(|cs| !crate::derive::is_state_stale_default(&cs.timestamp))
            .and_then(ClaudeSessionInfo::from_state_file);

        let (windows, panes) = build_windows_and_panes(PaneColumns::from_cached(session));

        rows.push(StandaloneSessionRow {
            session: EnrichedSession {
                tmux: TmuxSessionInfo {
                    host: Host::Local,
                    name: session.name.clone(),
                    status: SessionStatus::Running { attached: false },
                },
                claude,
                windows,
                panes,
                started_at: session.created_at,
                last_activity_at: session.last_activity_at,
            },
            config: StandaloneConfig {
                name: session.name.clone(),
                command: String::new(),
                cwd: session.path.clone(),
                start_on_launch: false,
            },
        });
    }

    rows
}

// ---------------------------------------------------------------------------
// Conversion helpers — WorkView types → cache types
// ---------------------------------------------------------------------------

/// Converts a [`WorkViewWorktree`] into a [`CachedWorktree`].
///
/// Ahead/behind are read directly from the daemon-supplied fields (#483).
fn work_view_worktree_to_cached(wt: &crate::daemon::types::WorkViewWorktree) -> CachedWorktree {
    CachedWorktree {
        path: wt.path.clone(),
        branch: wt.branch.clone(),
        is_bare: wt.bare,
        is_locked: false,
        host: if wt.host == "local" {
            None
        } else {
            Some(wt.host.clone())
        },
        ahead: wt.ahead,
        behind: wt.behind,
        last_commit_at: None,
        layout: WorktreeLayout::Bare,
    }
}

/// Converts a [`WorkViewPr`] into a [`CachedPr`].
///
/// The daemon pre-joins PR/issue associations. The `branch` parameter is the
/// worktree's branch (used as the PR's head branch since the daemon already
/// matched PR to worktree by branch). The `linked_issue` parameter carries
/// the issue number already resolved daemon-side (bypassing branch-name
/// convention for non-conventional branch names).
fn work_view_pr_to_cached(
    pr: &crate::daemon::types::WorkViewPr,
    branch: &str,
    linked_issue: Option<u32>,
) -> CachedPr {
    // Normalise state: daemon sends "OPEN"/"CLOSED"/"MERGED"; cache stores lowercase.
    let state = pr.state.to_lowercase();

    // The daemon carries `statusCheckRollup` (e.g. "SUCCESS") which maps to
    // `ci_code_state` ("passing"/"failing"/"pending"). Translate conservatively.
    let ci_code_state = pr.status_check_rollup.as_deref().map(rollup_to_ci_state);
    // `has_conflicts` maps to actual file-level merge conflicts only.
    // The pre-rip cache_sources path used `mergeable == "CONFLICTING"`.
    // `merge_state_status == "BLOCKED"` can mean CI failure or missing reviews —
    // not conflicts — so we must not conflate the two.
    let has_conflicts = pr.mergeable.as_deref() == Some("CONFLICTING");
    // Normalise review_decision to lowercase to match cache_sources convention.
    let review_decision = pr.review_decision.as_deref().map(|s| match s {
        "APPROVED" => "approved".to_string(),
        "CHANGES_REQUESTED" => "changes_requested".to_string(),
        "REVIEW_REQUIRED" => "review_required".to_string(),
        other => other.to_lowercase(),
    });

    #[allow(deprecated)]
    CachedPr {
        number: pr.number as u32,
        branch: branch.to_string(),
        linked_issue,
        state,
        review_decision,
        checks_state: ci_code_state.clone(),
        ci_code_state,
        ci_gate_state: None,
        ci_checks: crate::ci_state::CiChecks::default(),
        has_conflicts,
        unresolved_threads: 0,
        linked_issue_state: None,
        labels: pr.labels.clone(),
        title: Some(pr.title.clone()),
        is_draft: Some(pr.draft),
        author: None,
        requested_reviewers: Vec::new(),
        reviews: Vec::new(),
        additions: None,
        deletions: None,
        created_at: None,
        updated_at: None,
        last_commit_pushed_at: None,
        unresolved_thread_comment_timestamps: Vec::new(),
    }
}

/// Converts a [`WorkViewIssue`] into a [`CachedIssue`].
fn work_view_issue_to_cached(issue: &crate::daemon::types::WorkViewIssue) -> CachedIssue {
    CachedIssue {
        number: issue.number as u32,
        title: issue.title.clone(),
        state: issue.state.to_lowercase(),
        labels: issue.labels.clone(),
        assignees: Vec::new(),
        created_at: None,
        updated_at: None,
        blocked_by: Vec::new(),
        sub_issues: Vec::new(),
        parent: None,
    }
}

/// Converts a [`WorkViewTmuxSession`] into a [`CachedTmuxSession`].
///
/// The `path` field is taken from the session's optional working-directory.
/// When absent an empty string is used — sessions with no path will not
/// be matched to any worktree by the path-based join logic.
fn work_view_session_to_cached(s: &crate::daemon::types::WorkViewTmuxSession) -> CachedTmuxSession {
    CachedTmuxSession {
        name: s.name.clone(),
        path: s.path.clone().unwrap_or_default(),
        pane_targets: Vec::new(),
        pane_titles: Vec::new(),
        pane_commands: Vec::new(),
        window_names: Vec::new(),
        window_active: Vec::new(),
        window_layouts: Vec::new(),
        pane_paths: Vec::new(),
        pane_active: Vec::new(),
        host: None, // WorkView sessions are always local in v1
        created_at: None,
        last_activity_at: parse_rfc3339_to_epoch(s.last_activity_at.as_deref()),
        last_output_lines: Vec::new(),
        claude_state_raw: None,
    }
}

/// Converts a [`ClaudeInstance`] into a [`ClaudeStateFile`].
///
/// Extracts the tmux session name from the pane reference
/// (`TmuxPane:<host>:<session>:<window>:<pane>`). Returns `None` when the
/// pane reference cannot be parsed.
pub(crate) fn claude_instance_to_state_file(ci: &ClaudeInstance) -> Option<ClaudeStateFile> {
    let session_name = extract_session_from_pane(&ci.pane)?;
    Some(ClaudeStateFile {
        state: ci.state.clone(),
        session_id: ci.session_uuid.clone().unwrap_or_else(|| ci.id.clone()),
        tmux_session: session_name,
        cwd: String::new(),
        event: String::new(),
        timestamp: ci
            .last_activity_at
            .clone()
            .unwrap_or_else(|| chrono::Utc::now().format("%Y-%m-%dT%H:%M:%SZ").to_string()),
        model: ci.model.clone(),
        last_tool: None,
        current_task: None,
        session_start_ts: None,
        input_tokens: None,
        output_tokens: None,
        cache_creation_input_tokens: None,
        cache_read_input_tokens: None,
        stop_reason: None,
        inflight_tool_count: u32::try_from(ci.inflight_tool_count).ok(),
        state_changed_at: None,
    })
}

// ---------------------------------------------------------------------------
// Private helpers
// ---------------------------------------------------------------------------

/// Extracts the session name from a pane ID of the form
/// `TmuxPane:<host>:<session>:<window>:<index>`.
///
/// Session names may contain colons (tmux allows this), so this function
/// peels fields from the **right** rather than splitting left-to-right.
/// The format is positionally anchored from the right:
/// - rightmost field: `<pane_index>`
/// - second-from-right: `<window>`
/// - everything between `TmuxPane:<host>:` and `:<window>:<index>` is the session name.
///
/// Returns `None` when the format is not recognised (fewer than 5 colon-separated
/// fields, or the leading token is not `"TmuxPane"`).
fn extract_session_from_pane(pane: &str) -> Option<String> {
    // Peel trailing :<pane_index>
    let (without_index, _pane_index) = pane.rsplit_once(':')?;
    // Peel trailing :<window>
    let (without_window, _window) = without_index.rsplit_once(':')?;
    // without_window is now "TmuxPane:<host>:<session...>" where session may contain colons.
    // Strip the leading "TmuxPane:<host>:" prefix.
    let after_prefix = without_window.strip_prefix("TmuxPane:")?;
    // after_prefix is "<host>:<session...>"; strip the host.
    let (_host, session) = after_prefix.split_once(':')?;
    if session.is_empty() {
        return None;
    }
    Some(session.to_string())
}

/// Maps a GitHub status-check rollup string to the internal `ci_code_state` vocabulary.
///
/// | GitHub value     | Internal value |
/// |------------------|----------------|
/// | SUCCESS          | "passing"      |
/// | FAILURE / ERROR  | "failing"      |
/// | PENDING / EXPECTED | "pending"    |
/// | anything else    | "pending"      |
fn rollup_to_ci_state(rollup: &str) -> String {
    match rollup {
        "SUCCESS" => "passing".to_string(),
        "FAILURE" | "ERROR" => "failing".to_string(),
        "PENDING" | "EXPECTED" => "pending".to_string(),
        _ => "pending".to_string(),
    }
}

/// Parses an RFC 3339 timestamp string into Unix epoch seconds.
fn parse_rfc3339_to_epoch(ts: Option<&str>) -> Option<u64> {
    let s = ts?;
    crate::session::parse_iso8601_to_epoch(s)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::daemon::types::{
        ClaudeInstance, WorkViewIssue, WorkViewPr, WorkViewRepo, WorkViewSnapshot,
        WorkViewTmuxSession, WorkViewWorktree,
    };
    use crate::global_config::GlobalConfig;

    // -----------------------------------------------------------------------
    // WorkViewFixture — builder for test snapshots
    // -----------------------------------------------------------------------

    /// Builder for constructing a [`WorkViewSnapshot`] in tests.
    ///
    /// Used by tests in this module. For cross-module fixture sharing, expose
    /// via `#[cfg(test)] pub(crate)` in a future refactor.
    pub struct WorkViewFixture {
        snapshot: WorkViewSnapshot,
    }

    impl WorkViewFixture {
        /// Creates a new empty fixture.
        pub fn new() -> Self {
            Self {
                snapshot: WorkViewSnapshot {
                    repos: Vec::new(),
                    tmux_sessions: Vec::new(),
                    claude_instances: Vec::new(),
                },
            }
        }

        /// Adds a project with the given slug and directory.
        pub fn project(mut self, name: &str, directory: &str) -> Self {
            self.snapshot.repos.push(WorkViewRepo {
                slug: name.to_string(),
                path: directory.to_string(),
                worktrees: Vec::new(),
            });
            self
        }

        /// Adds a worktree to the last project.
        pub fn worktree(
            mut self,
            path: &str,
            branch: &str,
            repo: &str,
            pr: Option<WorkViewPr>,
            issue: Option<WorkViewIssue>,
        ) -> Self {
            let project = self.snapshot.repos.last_mut().expect("add a project first");
            project.worktrees.push(WorkViewWorktree {
                path: path.to_string(),
                branch: branch.to_string(),
                head: "deadbeef".to_string(),
                bare: false,
                host: "local".to_string(),
                repo: repo.to_string(),
                ahead: None,
                behind: None,
                pr,
                issue,
            });
            self
        }

        /// Adds a tmux session.
        pub fn session(mut self, name: &str, path: Option<&str>) -> Self {
            self.snapshot.tmux_sessions.push(WorkViewTmuxSession {
                id: format!("TmuxSession:local:{}", name),
                name: name.to_string(),
                attached: false,
                active_attached: false,
                last_activity_at: None,
                attached_clients: 0,
                windows: 1,
                current_window: None,
                path: path.map(|p| p.to_string()),
            });
            self
        }

        /// Adds a Claude instance associated with the given session.
        ///
        /// Uses the current UTC time as `last_activity_at` so the state file
        /// is never stale relative to the HOOK_STATE_STALENESS_SECS threshold.
        pub fn claude(mut self, session: &str, state: &str, uuid: &str) -> Self {
            let now = chrono::Utc::now().format("%Y-%m-%dT%H:%M:%SZ").to_string();
            self.snapshot.claude_instances.push(ClaudeInstance {
                id: format!("ClaudeInstance:local:{}", uuid),
                pane: format!("TmuxPane:local:{}:0:0", session),
                process: "claude".to_string(),
                state: state.to_string(),
                session_uuid: Some(uuid.to_string()),
                rc_enabled: false,
                last_activity_at: Some(now),
                model: None,
                inflight_tool_count: 0,
            });
            self
        }

        /// Returns the built [`WorkViewSnapshot`].
        pub fn build(self) -> WorkViewSnapshot {
            self.snapshot
        }
    }

    fn minimal_pr(number: u64, _branch: &str) -> WorkViewPr {
        WorkViewPr {
            number,
            state: "OPEN".to_string(),
            title: format!("PR {}", number),
            status_check_rollup: Some("SUCCESS".to_string()),
            review_decision: Some("APPROVED".to_string()),
            merge_state_status: Some("CLEAN".to_string()),
            mergeable: Some("MERGEABLE".to_string()),
            draft: false,
            labels: Vec::new(),
        }
    }

    fn minimal_issue(number: u64) -> WorkViewIssue {
        WorkViewIssue {
            number,
            state: "OPEN".to_string(),
            title: format!("Issue {}", number),
            labels: Vec::new(),
        }
    }

    fn empty_config() -> GlobalConfig {
        GlobalConfig::default()
    }

    // -----------------------------------------------------------------------
    // Test 1: builds_local_repo_state_from_work_view
    // -----------------------------------------------------------------------

    #[test]
    fn builds_local_repo_state_from_work_view() {
        let snapshot = WorkViewFixture::new()
            .project("orchardist", "/repos/orchardist")
            .worktree(
                "/repos/orchardist/.worktrees/issue429",
                "issue429/spec",
                "owner/repo",
                Some(minimal_pr(429, "issue429/spec")),
                Some(minimal_issue(429)),
            )
            .build();

        let state = build_local_state(&snapshot, &empty_config(), &HashMap::new());

        assert_eq!(state.repos.len(), 1);
        let repo = &state.repos[0];
        assert_eq!(repo.slug, "owner/repo");

        let wts: Vec<&crate::orchard_state::WorktreeState> =
            repo.worktrees.iter().filter(|w| !w.is_bare).collect();
        assert_eq!(wts.len(), 1);
        let wt = wts[0];

        // PR badge present
        assert!(wt.pr.is_some(), "expected PR to be present");
        let pr = wt.pr.as_ref().unwrap();
        assert_eq!(pr.number, 429);
        assert_eq!(pr.state.as_deref(), Some("open"));

        // Issue linked
        assert!(wt.issue.is_some(), "expected issue to be present");
        let issue = wt.issue.as_ref().unwrap();
        assert_eq!(issue.number, 429);

        // Host is None (local worktrees carry no host tag)
        assert!(wt.host.is_none(), "local worktree should have host == None");
    }

    // -----------------------------------------------------------------------
    // Test 2: joins_sessions_to_worktrees_via_path
    // -----------------------------------------------------------------------

    #[test]
    fn joins_sessions_to_worktrees_via_path() {
        let wt_path = "/repos/orchardist/.worktrees/issue429";
        let snapshot = WorkViewFixture::new()
            .project("orchardist", "/repos/orchardist")
            .worktree(wt_path, "issue429/spec", "owner/repo", None, None)
            .session("issue429", Some(wt_path))
            .build();

        let state = build_local_state(&snapshot, &empty_config(), &HashMap::new());

        let wt = state
            .repos
            .iter()
            .flat_map(|r| r.worktrees.iter())
            .find(|w| w.path == wt_path)
            .expect("worktree not found");

        assert!(
            !wt.sessions.is_empty(),
            "session should have been joined to the worktree"
        );
        assert_eq!(wt.sessions[0].name, "issue429");
        assert!(
            state.standalone_sessions.is_empty(),
            "the matched session should not appear as standalone"
        );
    }

    // -----------------------------------------------------------------------
    // Test 3: joins_claude_to_session_via_pane_reference
    // -----------------------------------------------------------------------

    #[test]
    fn joins_claude_to_session_via_pane_reference() {
        let wt_path = "/repos/orchardist/.worktrees/issue429";
        let session_uuid = "550e8400-e29b-41d4-a716-446655440000";

        let snapshot = WorkViewFixture::new()
            .project("orchardist", "/repos/orchardist")
            .worktree(wt_path, "issue429/spec", "owner/repo", None, None)
            .session("issue429", Some(wt_path))
            .claude("issue429", "working", session_uuid)
            .build();

        let state = build_local_state(&snapshot, &empty_config(), &HashMap::new());

        let wt = state
            .repos
            .iter()
            .flat_map(|r| r.worktrees.iter())
            .find(|w| w.path == wt_path)
            .expect("worktree not found");

        assert!(
            !wt.sessions.is_empty(),
            "session should be joined to worktree"
        );
        let session = &wt.sessions[0];
        assert!(
            session.claude.is_some(),
            "claude enrichment should be attached to the session"
        );
        let claude = session.claude.as_ref().unwrap();
        assert_eq!(
            claude.status,
            crate::claude_state::ClaudeState::Working,
            "claude state should be 'working'"
        );
    }

    // -----------------------------------------------------------------------
    // Test 4: unmatched_sessions_become_standalone
    // -----------------------------------------------------------------------

    #[test]
    fn unmatched_sessions_become_standalone() {
        let snapshot = WorkViewFixture::new()
            .project("orchardist", "/repos/orchardist")
            .worktree(
                "/repos/orchardist/.worktrees/issue429",
                "issue429/spec",
                "owner/repo",
                None,
                None,
            )
            // This session's path does NOT match any worktree path.
            .session("shepherd", Some("/home/user"))
            .build();

        let state = build_local_state(&snapshot, &empty_config(), &HashMap::new());

        // The shepherd session should be standalone, not attached to a worktree.
        let worktree_sessions: Vec<&str> = state
            .repos
            .iter()
            .flat_map(|r| r.worktrees.iter())
            .flat_map(|w| w.sessions.iter())
            .map(|s| s.name.as_str())
            .collect();
        assert!(
            !worktree_sessions.contains(&"shepherd"),
            "shepherd session must NOT be in any worktree"
        );

        let standalone_names: Vec<&str> = state
            .standalone_sessions
            .iter()
            .map(|s| s.session.tmux.name.as_str())
            .collect();
        assert!(
            standalone_names.contains(&"shepherd"),
            "shepherd session should be in standalone_sessions"
        );
    }

    // -----------------------------------------------------------------------
    // Test 5: empty_snapshot_yields_empty_state_with_passthrough_hosts
    // -----------------------------------------------------------------------

    #[test]
    fn empty_snapshot_yields_empty_state_with_passthrough_hosts() {
        let snapshot = WorkViewSnapshot {
            repos: Vec::new(),
            tmux_sessions: Vec::new(),
            claude_instances: Vec::new(),
        };
        let mut hosts = HashMap::new();
        hosts.insert(
            "boxd@vm.example.com".to_string(),
            HostState { reachable: true },
        );

        let state = build_local_state(&snapshot, &empty_config(), &hosts);

        assert!(state.repos.is_empty());
        assert!(state.standalone_sessions.is_empty());
        assert_eq!(state.hosts.len(), 1);
        assert!(state.hosts["boxd@vm.example.com"].reachable);
    }

    // -----------------------------------------------------------------------
    // Unit: extract_session_from_pane
    // -----------------------------------------------------------------------

    #[test]
    fn extracts_session_from_valid_pane_id() {
        assert_eq!(
            extract_session_from_pane("TmuxPane:local:issue429:editor:0"),
            Some("issue429".to_string())
        );
    }

    #[test]
    fn extract_session_returns_none_for_malformed_pane_id() {
        assert_eq!(extract_session_from_pane("not-a-pane-id"), None);
        assert_eq!(extract_session_from_pane("TmuxPane:only:three"), None);
    }

    // -----------------------------------------------------------------------
    // Unit: rollup_to_ci_state
    // -----------------------------------------------------------------------

    #[test]
    fn rollup_maps_success_to_passing() {
        assert_eq!(rollup_to_ci_state("SUCCESS"), "passing");
    }

    #[test]
    fn rollup_maps_failure_to_failing() {
        assert_eq!(rollup_to_ci_state("FAILURE"), "failing");
        assert_eq!(rollup_to_ci_state("ERROR"), "failing");
    }

    #[test]
    fn rollup_maps_pending_to_pending() {
        assert_eq!(rollup_to_ci_state("PENDING"), "pending");
    }

    // -----------------------------------------------------------------------
    // Concern #1: issue_labels_round_trip_through_adapter
    // -----------------------------------------------------------------------

    #[test]
    fn issue_labels_round_trip_through_adapter() {
        let issue = WorkViewIssue {
            number: 42,
            state: "OPEN".to_string(),
            title: "Test issue".to_string(),
            labels: vec!["bug".to_string(), "needs-investigation".to_string()],
        };
        let cached = work_view_issue_to_cached(&issue);
        assert_eq!(cached.labels, vec!["bug", "needs-investigation"]);
    }

    // -----------------------------------------------------------------------
    // Concern #3: pr_has_conflicts_only_when_mergeable_conflicting
    // -----------------------------------------------------------------------

    #[test]
    fn pr_has_conflicts_only_when_mergeable_conflicting() {
        let make_pr = |mergeable: Option<&str>, merge_state_status: Option<&str>| WorkViewPr {
            number: 1,
            state: "OPEN".to_string(),
            title: "PR".to_string(),
            status_check_rollup: None,
            review_decision: None,
            merge_state_status: merge_state_status.map(|s| s.to_string()),
            mergeable: mergeable.map(|s| s.to_string()),
            draft: false,
            labels: Vec::new(),
        };

        // CONFLICTING → has_conflicts true
        let pr = make_pr(Some("CONFLICTING"), Some("BLOCKED"));
        let cached = work_view_pr_to_cached(&pr, "feat/x", None);
        assert!(
            cached.has_conflicts,
            "CONFLICTING should yield has_conflicts true"
        );

        // MERGEABLE → has_conflicts false
        let pr = make_pr(Some("MERGEABLE"), Some("CLEAN"));
        let cached = work_view_pr_to_cached(&pr, "feat/x", None);
        assert!(
            !cached.has_conflicts,
            "MERGEABLE should yield has_conflicts false"
        );

        // UNKNOWN → has_conflicts false
        let pr = make_pr(Some("UNKNOWN"), None);
        let cached = work_view_pr_to_cached(&pr, "feat/x", None);
        assert!(
            !cached.has_conflicts,
            "UNKNOWN should yield has_conflicts false"
        );

        // None → has_conflicts false
        let pr = make_pr(None, None);
        let cached = work_view_pr_to_cached(&pr, "feat/x", None);
        assert!(
            !cached.has_conflicts,
            "None mergeable should yield has_conflicts false"
        );

        // Regression case: BLOCKED merge_state_status but MERGEABLE → has_conflicts false
        let pr = make_pr(Some("MERGEABLE"), Some("BLOCKED"));
        let cached = work_view_pr_to_cached(&pr, "feat/x", None);
        assert!(
            !cached.has_conflicts,
            "BLOCKED merge_state_status with MERGEABLE should yield has_conflicts false"
        );
    }

    // -----------------------------------------------------------------------
    // unresolved_thread_count flows daemon -> cache (#607)
    // -----------------------------------------------------------------------

    #[test]
    fn pr_unresolved_thread_count_flows_into_cached_pr() {
        // classify.rs and types.rs both gate on CachedPr::unresolved_threads,
        // so a daemon-sourced PR that drops the count is silently reported as
        // ready when threads are still blocking it.
        let pr = WorkViewPr {
            number: 3298,
            state: "OPEN".to_string(),
            title: "PR".to_string(),
            status_check_rollup: Some("SUCCESS".to_string()),
            review_decision: Some("APPROVED".to_string()),
            merge_state_status: Some("BLOCKED".to_string()),
            mergeable: Some("MERGEABLE".to_string()),
            draft: false,
            labels: Vec::new(),
            unresolved_thread_count: 2,
        };
        let cached = work_view_pr_to_cached(&pr, "feature/threads", None);
        assert_eq!(
            cached.unresolved_threads, 2,
            "daemon unresolvedThreadCount must reach CachedPr::unresolved_threads"
        );
    }

    #[test]
    fn pr_unresolved_thread_count_defaults_to_zero() {
        let pr = minimal_pr(1, "feat/x");
        let cached = work_view_pr_to_cached(&pr, "feat/x", None);
        assert_eq!(cached.unresolved_threads, 0);
    }

    // -----------------------------------------------------------------------
    // Concern #4: extract_session_from_pane_handles_colons_in_session_name
    // -----------------------------------------------------------------------

    #[test]
    fn extract_session_from_pane_handles_colons_in_session_name() {
        // Session name "my:session" contains a colon
        assert_eq!(
            extract_session_from_pane("TmuxPane:local:my:session:1:0"),
            Some("my:session".to_string())
        );
    }

    #[test]
    fn extract_session_simple_name_still_works() {
        // Existing behaviour preserved
        assert_eq!(
            extract_session_from_pane("TmuxPane:local:issue429:editor:0"),
            Some("issue429".to_string())
        );
    }

    // -----------------------------------------------------------------------
    // Concern #5: rollup fallthrough test
    // -----------------------------------------------------------------------

    #[test]
    fn pr_rollup_maps_unknown_to_pending() {
        // Any unrecognised rollup value should map to "pending" per the doc table.
        assert_eq!(rollup_to_ci_state("ACTION_REQUIRED"), "pending");
        assert_eq!(rollup_to_ci_state("SKIPPED"), "pending");
        assert_eq!(rollup_to_ci_state(""), "pending");
    }

    // -----------------------------------------------------------------------
    // Concern #7: linked_issue_carries_through_to_cached_pr
    // -----------------------------------------------------------------------

    #[test]
    fn linked_issue_carries_through_to_cached_pr() {
        let pr = WorkViewPr {
            number: 100,
            state: "OPEN".to_string(),
            title: "Fix something".to_string(),
            status_check_rollup: None,
            review_decision: None,
            merge_state_status: None,
            mergeable: Some("MERGEABLE".to_string()),
            draft: false,
            labels: Vec::new(),
        };
        let cached = work_view_pr_to_cached(&pr, "fix/something", Some(429));
        assert_eq!(
            cached.linked_issue,
            Some(429),
            "linked issue 429 should carry through to CachedPr"
        );
    }
}
