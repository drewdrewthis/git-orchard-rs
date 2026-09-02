//! Thin compatibility shim for non-TUI consumers of the on-disk cache.
//!
//! Post-#429 (TUI dashboard sources local data from `daemon::Client::work_view`),
//! this module is **not** the dashboard's primary refresh path. It is retained
//! as a documented shim with the following named owners:
//!
//! | Surface | Owner / consumer | Removal phase |
//! |---|---|---|
//! | `refresh_remote_worktrees`, `refresh_remote_tmux_sessions` | `tui::App::start_full_refresh` (remote enrichment) | #429 Phase 3 — gated on daemon Workstream F |
//! | `refresh_issues`, `refresh_prs` | `watch::daemon` (when `--keep-diagnostic-caches` is set) + `heal` subcommand | separate refactor |
//! | `refresh_worktrees`, `refresh_tmux_sessions` | `watch::daemon` + `heal` + `--json` mode + cold-start cache fallback | separate refactor |
//! | `TMUX_SESSION_FORMAT` (constant) | `remote_adapter` — pure-data string, NOT `Command::new` | rename follow-up |
//! | `parse_tmux_sessions_from_panes` | `remote_adapter` — pure-data parser | rename follow-up |
//! | `snapshot_fork_hosts_for_remote` | `tui::App::start_full_refresh` (pre-refresh snapshot for vanished-fork detection) | #429 Phase 3 — kept for symmetry with remote refresh |
//!
//! Every function in this module either:
//! 1. Writes a cache file consumed by a non-TUI surface above, OR
//! 2. Provides a pure-data primitive (constant, parser) referenced by other modules.
//!
//! New TUI dashboard refreshes MUST go through `daemon::Client::work_view` and
//! `daemon::work_view_adapter::build_local_state`, not through this module.
//! See `docs/architecture.md` for the rationale.
use std::collections::HashMap;
use std::process::Command;

use crate::cache::{
    self, CachedIssue, CachedPr, CachedRepoMeta, CachedReview, CachedSubIssue, CachedTmuxSession,
    CachedWorktree,
};
use crate::ci_state::{
    CheckInfo, CiChecks, GateMatcher, classify_check, map_check_run_conclusion,
    map_status_context_state, rollup_code_state, rollup_gate_state,
};
use crate::global_config::RepoConfig;
use crate::logger::LOG;
use crate::remote;

// ---------------------------------------------------------------------------
// Parse helpers
// ---------------------------------------------------------------------------

/// Counts review threads that still block merge.
///
/// Matches GitHub's "Require conversation resolution before merging" gate,
/// which blocks on `isResolved != true` alone — `isOutdated` has no bearing
/// on it. An outdated thread (anchored to a diff hunk no longer on the head
/// commit) still blocks merge until it is resolved. Mirrors the fix in
/// `internal/server/providers/gh/types.go` `UnresolvedThreadCount()` (#607).
fn count_actionable_threads(nodes: &serde_json::Value) -> u32 {
    let Some(arr) = nodes.as_array() else {
        return 0;
    };
    arr.iter()
        .filter(|t| t["isResolved"].as_bool() != Some(true))
        .count() as u32
}

/// Collects the latest-comment timestamp (epoch seconds) for each review thread
/// that is unresolved AND not outdated (`isResolved=false AND isOutdated!=true`).
///
/// Each qualifying thread contributes at most one timestamp: the `createdAt` of
/// its last comment, parsed from ISO 8601 into epoch seconds. Threads whose
/// last comment timestamp cannot be parsed are silently skipped — the count
/// field (`unresolved_threads`) is always authoritative for presence, this
/// data is used only for `since_epoch`.
///
/// Deliberately still excludes outdated threads, unlike `count_actionable_threads`:
/// this feeds a recency signal for a PR that's already blocking, not the
/// blocking decision itself, so the two filters no longer need to match.
fn collect_thread_comment_timestamps(nodes: &serde_json::Value) -> Vec<i64> {
    let Some(arr) = nodes.as_array() else {
        return Vec::new();
    };
    arr.iter()
        .filter(|t| {
            t["isResolved"].as_bool() != Some(true) && t["isOutdated"].as_bool() != Some(true)
        })
        .filter_map(|t| {
            let created_at = t["comments"]["nodes"]
                .as_array()
                .and_then(|nodes| nodes.last())
                .and_then(|n| n["createdAt"].as_str())?;
            // Parse ISO 8601 → epoch seconds. We only need second-level precision.
            let ts = ::chrono::DateTime::parse_from_rfc3339(created_at).ok()?;
            Some(ts.timestamp())
        })
        .collect()
}

/// Parses raw JSON output from `gh issue list --json number,title,state,labels`
/// into a `Vec<CachedIssue>`.
pub fn parse_issues_json(json: &str) -> Vec<CachedIssue> {
    let values: Vec<serde_json::Value> = match serde_json::from_str(json) {
        Ok(v) => v,
        Err(e) => {
            LOG.warn(&format!("cache_sources: failed to parse issues JSON: {e}"));
            return Vec::new();
        }
    };

    values
        .into_iter()
        .filter_map(|v| {
            let number = v["number"].as_u64()? as u32;
            let title = v["title"].as_str().unwrap_or("").to_string();
            let state = v["state"].as_str().unwrap_or("open").to_lowercase();
            let labels = v["labels"]
                .as_array()
                .map(|arr| {
                    arr.iter()
                        .filter_map(|l| l["name"].as_str().map(|s| s.to_string()))
                        .collect()
                })
                .unwrap_or_default();
            Some(CachedIssue {
                number,
                title,
                state,
                labels,
                assignees: vec![],
                created_at: None,
                updated_at: None,
                blocked_by: vec![],
                sub_issues: vec![],
                parent: None,
            })
        })
        .collect()
}

/// Builds a per-issue GraphQL query using aliased `repository.issue(number:N)` fields.
///
/// Uses a `IssueFields` fragment to avoid repeating the field set for each alias.
/// The `subIssues` and `parent` fields require the `GraphQL-Features: sub_issues`
/// header to be passed to `gh api graphql`.
///
/// If `issue_numbers` is empty, returns a minimal stub query that only fetches
/// the repository name (no issue aliases).
pub fn issue_graphql_query(owner: &str, name: &str, issue_numbers: &[u32]) -> String {
    if issue_numbers.is_empty() {
        return format!(r#"query {{ repository(owner: "{owner}", name: "{name}") {{ name }} }}"#);
    }

    let aliases: Vec<String> = issue_numbers
        .iter()
        .map(|n| format!(r#"    i_{n}: issue(number: {n}) {{ ...IssueFields }}"#))
        .collect();

    format!(
        r#"query {{
  repository(owner: "{owner}", name: "{name}") {{
{aliases}
  }}
}}

fragment IssueFields on Issue {{
  number
  title
  state
  labels(first: 100) {{ nodes {{ name }} }}
  assignees(first: 20) {{ nodes {{ login }} }}
  createdAt
  updatedAt
  body
  subIssues(first: 50) {{ nodes {{ number title state }} }}
  parent {{ number }}
}}"#,
        aliases = aliases.join("\n")
    )
}

/// Extracts issue numbers that block `body` text using the blocking-reference regex.
///
/// Matches: "blocked by #N", "depends on #N", "waiting on #N" (case-insensitive).
/// Does NOT match: "fixes #N", "closes #N", or bare "#N".
fn extract_blocked_by(body: &str) -> Vec<u32> {
    use std::sync::OnceLock;
    static RE: OnceLock<regex::Regex> = OnceLock::new();
    let re = RE.get_or_init(|| {
        regex::Regex::new(r"(?i)(?:blocked\s+by|depends\s+on|waiting\s+on)\s+#(\d+)")
            .expect("blocking regex is valid")
    });
    re.captures_iter(body)
        .filter_map(|caps| caps[1].parse::<u32>().ok())
        .collect()
}

/// Parses a per-issue aliased GraphQL response into `Vec<CachedIssue>`.
///
/// Expects shape: `{"data":{"repository":{"i_42":{...}, "i_99":{...}, ...}}}`.
/// Iterates over all keys in `data.repository` that start with `i_`, maps each
/// to a `CachedIssue` with enriched fields including sub-issues, parent, assignees,
/// createdAt, and blocking references parsed from the body.
///
/// GraphQL returns state in uppercase (`OPEN`/`CLOSED`); this function normalizes
/// to lowercase.
pub fn parse_issues_graphql(json: &str) -> Vec<CachedIssue> {
    let root: serde_json::Value = match serde_json::from_str(json) {
        Ok(v) => v,
        Err(e) => {
            LOG.warn(&format!(
                "cache_sources: failed to parse issues GraphQL JSON: {e}"
            ));
            return Vec::new();
        }
    };

    let repo = match root["data"]["repository"].as_object() {
        Some(obj) => obj,
        None => {
            LOG.warn("cache_sources: issues GraphQL response missing data.repository");
            return Vec::new();
        }
    };

    let mut issues = Vec::new();

    for (key, val) in repo {
        if !key.starts_with("i_") {
            continue;
        }

        // A null value means the issue doesn't exist (deleted / wrong number).
        if val.is_null() {
            continue;
        }

        let Some(number) = val["number"].as_u64().map(|n| n as u32) else {
            continue;
        };

        let title = val["title"].as_str().unwrap_or("").to_string();
        let state = val["state"].as_str().unwrap_or("OPEN").to_lowercase();

        let labels = val["labels"]["nodes"]
            .as_array()
            .map(|arr| {
                arr.iter()
                    .filter_map(|l| l["name"].as_str().map(|s| s.to_string()))
                    .collect()
            })
            .unwrap_or_default();

        let assignees = val["assignees"]["nodes"]
            .as_array()
            .map(|arr| {
                arr.iter()
                    .filter_map(|a| a["login"].as_str().map(|s| s.to_string()))
                    .collect()
            })
            .unwrap_or_default();

        let created_at = val["createdAt"].as_str().map(|s| s.to_string());
        // GitHub GraphQL exposes `updatedAt` on issues; used by TUI SINCE
        // column for Blocked / Paused statuses (issue #251).
        let updated_at = val["updatedAt"].as_str().map(|s| s.to_string());

        let body = val["body"].as_str().unwrap_or("");
        let blocked_by = extract_blocked_by(body);

        let sub_issues = val["subIssues"]["nodes"]
            .as_array()
            .map(|arr| {
                arr.iter()
                    .filter_map(|s| {
                        let sub_number = s["number"].as_u64()? as u32;
                        let sub_title = s["title"].as_str().unwrap_or("").to_string();
                        let sub_state = s["state"].as_str().unwrap_or("OPEN").to_lowercase();
                        Some(CachedSubIssue {
                            number: sub_number,
                            title: sub_title,
                            state: sub_state,
                        })
                    })
                    .collect()
            })
            .unwrap_or_default();

        let parent = val["parent"]["number"].as_u64().map(|n| n as u32);

        issues.push(CachedIssue {
            number,
            title,
            state,
            labels,
            assignees,
            created_at,
            updated_at,
            blocked_by,
            sub_issues,
            parent,
        });
    }

    issues
}

/// Parses GraphQL PR response JSON into a `Vec<CachedPr>`.
///
/// Expected shape: `{"data":{"repository":{"pullRequests":{"nodes":[...]}}}}`
/// Each node has: number, headRefName, state, reviewDecision, mergeable,
/// reviewThreads, closingIssuesReferences, commits (for status checks with
/// per-check context breakdown via `contexts(first: 100)`).
///
/// The `matcher` is used to classify each check as code or gate. Build it
/// from `GlobalConfig.ci_gate_patterns` via `GateMatcher::new`.
pub fn parse_prs_graphql(json: &str, matcher: &GateMatcher) -> Vec<CachedPr> {
    let root: serde_json::Value = match serde_json::from_str(json) {
        Ok(v) => v,
        Err(e) => {
            LOG.warn(&format!(
                "cache_sources: failed to parse PRs GraphQL JSON: {e}"
            ));
            return Vec::new();
        }
    };

    let nodes = match root["data"]["repository"]["pullRequests"]["nodes"].as_array() {
        Some(n) => n,
        None => {
            LOG.warn("cache_sources: PRs GraphQL response missing expected path");
            return Vec::new();
        }
    };

    nodes
        .iter()
        .filter_map(|v| {
            let number = v["number"].as_u64()? as u32;
            let branch = v["headRefName"].as_str().unwrap_or("").to_string();
            let base = v["baseRefName"].as_str().unwrap_or("");
            let state = v["state"].as_str().unwrap_or("OPEN").to_lowercase();

            // Skip PRs where head == base (e.g., cross-fork PRs showing "main" → "main")
            if branch == base {
                return None;
            }

            let review_decision = match v["reviewDecision"].as_str().unwrap_or("") {
                "APPROVED" => Some("approved".to_string()),
                "CHANGES_REQUESTED" => Some("changes_requested".to_string()),
                "REVIEW_REQUIRED" => Some("review_required".to_string()),
                _ => None,
            };

            let (ci_code_state, ci_gate_state, ci_checks) =
                derive_ci_state_graphql(v, number, matcher);
            // Legacy checks_state mirrors ci_code_state only — a code-green
            // gate-blocked PR stays "passing" for backward-compat consumers.
            let checks_state = ci_code_state.clone();

            let has_conflicts = v["mergeable"].as_str().unwrap_or("") == "CONFLICTING";

            let unresolved_threads = count_actionable_threads(&v["reviewThreads"]["nodes"]);
            let unresolved_thread_comment_timestamps =
                collect_thread_comment_timestamps(&v["reviewThreads"]["nodes"]);

            // Use GitHub's closingIssuesReferences (first linked issue).
            let first_linked = v["closingIssuesReferences"]["nodes"]
                .as_array()
                .and_then(|arr| arr.first());
            let linked_issue = first_linked
                .and_then(|issue| issue["number"].as_u64())
                .map(|n| n as u32);
            let linked_issue_state = first_linked.and_then(|issue| {
                let s = issue["state"].as_str()?;
                let reason = issue["stateReason"].as_str().unwrap_or("");
                let normalised = if s == "OPEN" {
                    "open"
                } else if reason == "COMPLETED" {
                    "completed"
                } else {
                    "closed"
                };
                Some(normalised.to_string())
            });

            // GraphQL shape is `labels: { nodes: [{ name: "..." }, ...] }` —
            // unlike `parse_issues_json` above which reads a flat REST array.
            let labels = v["labels"]["nodes"]
                .as_array()
                .map(|arr| {
                    arr.iter()
                        .filter_map(|l| l["name"].as_str().map(|s| s.to_string()))
                        .collect()
                })
                .unwrap_or_default();

            Some(CachedPr {
                number,
                branch,
                linked_issue,
                state,
                review_decision,
                checks_state,
                ci_code_state,
                ci_gate_state,
                ci_checks,
                has_conflicts,
                unresolved_threads,
                linked_issue_state,
                labels,
                title: None,
                is_draft: None,
                author: None,
                requested_reviewers: vec![],
                reviews: vec![],
                additions: None,
                deletions: None,
                created_at: None,
                updated_at: None,
                last_commit_pushed_at: None,
                unresolved_thread_comment_timestamps,
            })
        })
        .collect()
}

/// Derives split CI state from the GraphQL commit statusCheckRollup.
///
/// Walks `commits.nodes[last].commit.statusCheckRollup.contexts.nodes[]`,
/// parses each node as either a `CheckRun` or `StatusContext`, maps its
/// conclusion/state to a normalized value, classifies it via `matcher`, and
/// rolls up both buckets into `ci_code_state` and `ci_gate_state`.
///
/// If `totalCount > 100` (GitHub truncation), a warning is logged.
///
/// Returns `(ci_code_state, ci_gate_state, ci_checks)`.
fn derive_ci_state_graphql(
    pr: &serde_json::Value,
    pr_number: u32,
    matcher: &GateMatcher,
) -> (Option<String>, Option<String>, CiChecks) {
    let rollup = pr["commits"]["nodes"]
        .as_array()
        .and_then(|nodes| nodes.last())
        .map(|n| &n["commit"]["statusCheckRollup"])
        .filter(|r| r.is_object());

    let context_nodes: &[serde_json::Value] =
        match rollup.and_then(|r| r["contexts"]["nodes"].as_array()) {
            Some(arr) => arr,
            None => {
                // No contexts available — fall back to top-level rollup state for
                // legacy checks_state, but ci_code_state/ci_gate_state are None.
                return (None, None, CiChecks::default());
            }
        };

    // Warn if truncated.
    if let Some(total) = rollup.and_then(|r| r["contexts"]["totalCount"].as_u64())
        && total > 100
    {
        LOG.warn(&format!(
            "cache_sources: PR {pr_number} has {total} checks, truncating to first 100"
        ));
    }

    let mut code_checks: Vec<CheckInfo> = Vec::new();
    let mut gate_checks: Vec<CheckInfo> = Vec::new();

    for node in context_nodes {
        let typename = node["__typename"].as_str().unwrap_or("");
        let (name, state_opt, details_url) = match typename {
            "CheckRun" => {
                let name = node["name"].as_str().unwrap_or("").to_string();
                let conclusion = node["conclusion"].as_str();
                let status = node["status"].as_str();
                let state = map_check_run_conclusion(conclusion, status);
                let details_url = node["detailsUrl"].as_str().map(|s| s.to_string());
                (name, state, details_url)
            }
            "StatusContext" => {
                let name = node["context"].as_str().unwrap_or("").to_string();
                let raw_state = node["state"].as_str().unwrap_or("");
                let state = map_status_context_state(raw_state);
                (name, state, None)
            }
            _ => continue,
        };

        let state = match state_opt {
            Some(s) => s,
            None => continue, // SKIPPED/CANCELLED/STALE — omit from rollup
        };

        let check = CheckInfo {
            name: name.clone(),
            state,
            details_url,
        };
        use crate::ci_state::CheckBucket;
        match classify_check(&name, matcher) {
            CheckBucket::Gate => gate_checks.push(check),
            CheckBucket::Code => code_checks.push(check),
        }
    }

    let ci_code_state = rollup_code_state(&code_checks);
    let ci_gate_state = rollup_gate_state(&gate_checks);
    let ci_checks = CiChecks {
        code: code_checks,
        gate: gate_checks,
    };

    (ci_code_state, ci_gate_state, ci_checks)
}

/// Parses `git worktree list --porcelain` and resolves `.git/modules/<name>`
/// submodule paths to their actual working-tree root.
///
/// Delegates the pure parse to `git_parse::parse_worktree_porcelain`. The
/// previous submodule-resolution branch (`git rev-parse --show-toplevel` from
/// inside `.git/modules/<name>`) was ripped as part of #426's thin-shell
/// rip-out: the daemon owns worktree discovery now and submodule reporting
/// flows through it. If a `.git/modules/` path slips through, the caller
/// sees the raw module path and surfaces the issue rather than silently
/// shell-resolving it.
pub fn parse_worktree_porcelain(output: &str) -> Vec<CachedWorktree> {
    crate::git_parse::parse_worktree_porcelain(output)
}

/// The `-F` format string passed to `tmux list-panes -a` for all session
/// enumeration — local, remote (Remmy/BoxdShared), and per-fork (BoxdFork).
///
/// All three callers MUST use this constant so that
/// `parse_tmux_sessions_from_panes` always receives identically-structured
/// lines regardless of the code path that produced them.
pub(crate) const TMUX_SESSION_FORMAT: &str = "#{session_name}\t#{pane_active}\t#{pane_current_path}\t#{session_created}\t#{session_activity}";

/// Parses the output of `tmux list-panes -a -F '#{session_name}\t#{pane_active}\t#{pane_current_path}\t#{session_created}\t#{session_activity}'`
/// into `Vec<CachedTmuxSession>`, one entry per session.
///
/// Each line represents a single pane. This function filters to `pane_active=1` so that
/// `CachedTmuxSession.path` reflects the live cwd of the active pane rather than the
/// frozen `session_path` written at session creation time.
///
/// `panes_fn` and `content_fn` have the same contract as in `parse_tmux_output` —
/// they fetch per-session pane detail and terminal output respectively.
pub fn parse_tmux_sessions_from_panes(
    panes_output: &str,
    host: Option<&str>,
    panes_fn: impl Fn(&str) -> String,
    content_fn: impl Fn(&str) -> Vec<String>,
) -> Vec<CachedTmuxSession> {
    // Collect the active-pane row for each session, keyed by session name.
    // If a session appears multiple times (should not happen in practice for
    // pane_active=1, but guard anyway) only the first matching row wins.
    let mut seen: std::collections::HashMap<String, (String, Option<u64>, Option<u64>)> =
        std::collections::HashMap::new();

    for line in panes_output.trim().lines() {
        if line.is_empty() {
            continue;
        }
        // Format: "{session_name}\t{pane_active}\t{pane_current_path}\t{session_created}\t{session_activity}"
        let parts: Vec<&str> = line.splitn(5, '\t').collect();
        if parts.len() < 3 {
            continue;
        }
        let name = parts[0];
        let pane_active = parts[1];
        if pane_active != "1" {
            continue;
        }
        if seen.contains_key(name) {
            continue;
        }
        let path = parts[2];
        let created_at: Option<u64> = parts.get(3).and_then(|s| s.parse().ok());
        let last_activity_at: Option<u64> = parts.get(4).and_then(|s| s.parse().ok());
        seen.insert(
            name.to_string(),
            (path.to_string(), created_at, last_activity_at),
        );
    }

    // Build sessions in the order they were first seen (stable across runs that
    // preserve tmux list-panes ordering, which is alphabetical by session name).
    let mut ordered: Vec<(String, String, Option<u64>, Option<u64>)> = seen
        .into_iter()
        .map(|(name, (path, created, activity))| (name, path, created, activity))
        .collect();
    ordered.sort_by(|a, b| a.0.cmp(&b.0));

    let mut sessions = Vec::with_capacity(ordered.len());
    for (name, path, created_at, last_activity_at) in ordered {
        let pane_output = panes_fn(&name);
        let parsed = parse_pane_lines(&pane_output);
        let last_output_lines = content_fn(&name);
        sessions.push(CachedTmuxSession {
            name,
            path,
            pane_targets: parsed.targets,
            pane_titles: parsed.titles,
            pane_commands: parsed.commands,
            window_names: parsed.window_names,
            window_active: parsed.window_active,
            window_layouts: parsed.window_layouts,
            pane_paths: parsed.pane_paths,
            pane_active: parsed.pane_active,
            host: host.map(|h| h.to_string()),
            created_at,
            last_activity_at,
            last_output_lines,
            claude_state_raw: None,
        });
    }

    sessions
}

/// Parses the combined output of `tmux list-sessions` and per-session pane
/// information into `Vec<CachedTmuxSession>`.
///
/// `sessions_output` has lines in the format
/// `{session_name}:{session_path}|{session_created}|{session_activity}`.
/// Session path is everything between the first `:` and the first `|`;
/// `session_created` and `session_activity` are Unix timestamps (integers)
/// appended with `|` separators and omitted when the format string does not
/// include them (backward-compatible: lines without `|` produce `None` for both).
/// `panes_fn` is called with each session name and returns lines in the format
/// `{pane_title}:{pane_current_command}`.
/// `content_fn` is called with each session name and returns the last few lines
/// of pane output (used for Claude prompt detection).
pub fn parse_tmux_output(
    sessions_output: &str,
    host: Option<&str>,
    panes_fn: impl Fn(&str) -> String,
    content_fn: impl Fn(&str) -> Vec<String>,
) -> Vec<CachedTmuxSession> {
    let mut sessions = Vec::new();

    for line in sessions_output.trim().lines() {
        if line.is_empty() {
            continue;
        }
        // Format: "{session_name}:{session_path}|{session_created}|{session_activity}"
        // Session path may itself contain colons (e.g. Windows paths or
        // absolute paths on some systems), so we split on the first colon only
        // to get name; then the remainder may contain pipe-separated timestamps.
        let Some((name, rest)) = line.split_once(':') else {
            continue;
        };

        // Split `rest` on `|` to separate path from optional timestamps.
        let parts: Vec<&str> = rest.splitn(3, '|').collect();
        let path = parts[0];
        let created_at: Option<u64> = parts.get(1).and_then(|s| s.parse().ok());
        let last_activity_at: Option<u64> = parts.get(2).and_then(|s| s.parse().ok());

        let pane_output = panes_fn(name);
        let parsed = parse_pane_lines(&pane_output);
        let last_output_lines = content_fn(name);

        sessions.push(CachedTmuxSession {
            name: name.to_string(),
            path: path.to_string(),
            pane_targets: parsed.targets,
            pane_titles: parsed.titles,
            pane_commands: parsed.commands,
            window_names: parsed.window_names,
            window_active: parsed.window_active,
            window_layouts: parsed.window_layouts,
            pane_paths: parsed.pane_paths,
            pane_active: parsed.pane_active,
            host: host.map(|h| h.to_string()),
            created_at,
            last_activity_at,
            last_output_lines,
            claude_state_raw: None, // populated after parsing remote Claude state
        });
    }

    sessions
}

/// Structured output of `parse_pane_lines`.
struct ParsedPaneData {
    /// Tmux window.pane addresses (e.g. "0.1").
    targets: Vec<String>,
    /// Window names per pane row.
    window_names: Vec<String>,
    /// Window active flags per pane row ("1" or "0").
    window_active: Vec<String>,
    /// Window layout strings per pane row (e.g. "even-horizontal").
    window_layouts: Vec<String>,
    /// Current working directory of each pane.
    pane_paths: Vec<String>,
    /// Whether this pane is the active pane in its window ("1" or "0").
    pane_active: Vec<String>,
    /// Pane titles per pane row.
    titles: Vec<String>,
    /// Commands running in each pane.
    commands: Vec<String>,
}

/// Parses pane info lines into structured pane data.
///
/// New (7-field) format:
/// `{window}.{pane}\t{window_name}\t{window_active}\t{window_layout}\t{pane_current_path}\t{pane_active}\t{pane_title}:{pane_current_command}`
///
/// Legacy #184 (4-field) format:
/// `{window}.{pane}\t{window_name}\t{window_active}\t{pane_title}:{pane_current_command}`
/// — `window_layouts`, `pane_paths` default to empty string, `pane_active` defaults to "0".
///
/// Oldest (2-field) format (backward compat):
/// `{window}.{pane}\t{pane_title}:{pane_current_command}`
/// — `window_names`, `window_layouts`, `pane_paths` default to empty string,
///   `window_active` and `pane_active` default to "0".
fn parse_pane_lines(output: &str) -> ParsedPaneData {
    let mut targets = Vec::new();
    let mut window_names = Vec::new();
    let mut window_active = Vec::new();
    let mut window_layouts = Vec::new();
    let mut pane_paths = Vec::new();
    let mut pane_active = Vec::new();
    let mut titles = Vec::new();
    let mut commands = Vec::new();

    for line in output.trim().lines() {
        if line.is_empty() {
            continue;
        }
        // Split into at most 7 fields; match on slice length to pick format generation.
        let parts: Vec<&str> = line.splitn(7, '\t').collect();
        match parts.as_slice() {
            [
                target,
                win_name,
                win_active,
                win_layout,
                pane_path,
                p_active,
                rest,
            ] => {
                // New 7-field format.
                targets.push(target.to_string());
                window_names.push(win_name.to_string());
                window_active.push(win_active.to_string());
                window_layouts.push(win_layout.to_string());
                pane_paths.push(pane_path.to_string());
                pane_active.push(p_active.to_string());
                if let Some((title, cmd)) = rest.split_once(':') {
                    titles.push(title.to_string());
                    commands.push(cmd.to_string());
                } else {
                    titles.push(rest.to_string());
                    commands.push(String::new());
                }
            }
            [target, win_name, win_active, rest] => {
                // Legacy #184 4-field format: no layout/path/pane_active.
                targets.push(target.to_string());
                window_names.push(win_name.to_string());
                window_active.push(win_active.to_string());
                window_layouts.push(String::new());
                pane_paths.push(String::new());
                pane_active.push("0".to_string());
                if let Some((title, cmd)) = rest.split_once(':') {
                    titles.push(title.to_string());
                    commands.push(cmd.to_string());
                } else {
                    titles.push(rest.to_string());
                    commands.push(String::new());
                }
            }
            [target, rest] => {
                // Oldest 2-field format: target, title:command only.
                targets.push(target.to_string());
                window_names.push(String::new());
                window_active.push("0".to_string());
                window_layouts.push(String::new());
                pane_paths.push(String::new());
                pane_active.push("0".to_string());
                if let Some((title, cmd)) = rest.split_once(':') {
                    titles.push(title.to_string());
                    commands.push(cmd.to_string());
                } else {
                    titles.push(rest.to_string());
                    commands.push(String::new());
                }
            }
            _ => {
                // Malformed line — skip.
            }
        }
    }

    ParsedPaneData {
        targets,
        window_names,
        window_active,
        window_layouts,
        pane_paths,
        pane_active,
        titles,
        commands,
    }
}

// ---------------------------------------------------------------------------
// Local command helpers
// ---------------------------------------------------------------------------

fn run_local(program: &str, args: &[&str]) -> anyhow::Result<String> {
    let out = Command::new(program).args(args).output()?;
    if !out.status.success() {
        let stderr = String::from_utf8_lossy(&out.stderr).into_owned();
        return Err(anyhow::anyhow!("{program} failed: {stderr}"));
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}

/// Runs a local command in the given working directory and returns its stdout.
///
/// Used by the ahead/behind carve-out in `tui::App::start_full_refresh` to
/// shell out to `git branch -vv` per project directory.
pub fn run_local_in(program: &str, args: &[&str], cwd: &str) -> anyhow::Result<String> {
    let out = Command::new(program).args(args).current_dir(cwd).output()?;
    if !out.status.success() {
        let stderr = String::from_utf8_lossy(&out.stderr).into_owned();
        return Err(anyhow::anyhow!("{program} failed: {stderr}"));
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}

// ---------------------------------------------------------------------------
// Public refresh functions
// ---------------------------------------------------------------------------

/// Fetches GitHub issues linked to known worktree branches for `config.slug` and
/// writes to the issues cache.
///
/// Extracts issue numbers from:
/// 1. `linked_issue` fields in the PRs cache (from `closingIssuesReferences`)
/// 2. Branch names in the worktrees cache via `extract_issue_number()`
///
/// Then queries only those issues via a per-issue GraphQL query with the
/// `GraphQL-Features: sub_issues` header to populate sub-issues and parent fields.
///
/// On API failure the error is logged and the existing cache is left intact.
pub fn refresh_issues(config: &RepoConfig) -> anyhow::Result<()> {
    let path = cache::cache_path(config.owner(), config.repo_name(), "issues");

    // SWR fast path: skip the gh api call when the issues cache is Fresh.
    let cached_file = cache::read_cache::<CachedIssue>(&path);
    let swr_config = crate::swr::SwrConfig::default_for(crate::swr::SwrKind::GithubIssues);
    if crate::swr::classify_cache_file(&cached_file, chrono::Utc::now(), swr_config)
        == crate::swr::SwrState::Fresh
    {
        LOG.info(&format!(
            "cache_sources: refresh_issues({}): swr=Fresh, skipping gh api",
            config.slug
        ));
        return Ok(());
    }

    // Collect issue numbers from PRs cache (closingIssuesReferences).
    let prs_path = cache::cache_path(config.owner(), config.repo_name(), "prs");
    let prs: Vec<CachedPr> = cache::read_cache::<CachedPr>(&prs_path).entries;
    let mut issue_numbers: std::collections::HashSet<u32> =
        prs.iter().filter_map(|pr| pr.linked_issue).collect();

    // Also collect from worktree branch names.
    let wt_path = cache::cache_path(config.owner(), config.repo_name(), "worktrees");
    let worktrees: Vec<CachedWorktree> = cache::read_cache::<CachedWorktree>(&wt_path).entries;
    for wt in &worktrees {
        if !wt.is_bare
            && let Some(n) = crate::github::extract_issue_number(&wt.branch)
        {
            issue_numbers.insert(n);
        }
    }

    if issue_numbers.is_empty() {
        LOG.info(&format!(
            "cache_sources: refresh_issues({}): no linked issues found, skipping",
            config.slug
        ));
        cache::write_cache_if_nonempty(&path, &Vec::<CachedIssue>::new())?;
        return Ok(());
    }

    let mut numbers: Vec<u32> = issue_numbers.into_iter().collect();
    numbers.sort_unstable();

    let query = issue_graphql_query(config.owner(), config.repo_name(), &numbers);

    let out = run_local(
        "gh",
        &[
            "api",
            "graphql",
            "-H",
            "GraphQL-Features: sub_issues",
            "-f",
            &format!("query={query}"),
        ],
    );

    match out {
        Err(e) => {
            LOG.warn(&format!(
                "cache_sources: refresh_issues({}): {e}",
                config.slug
            ));
            return Ok(());
        }
        Ok(json) => {
            let issues = parse_issues_graphql(&json);
            cache::write_cache_if_nonempty(&path, &issues)?;
            LOG.info(&format!(
                "cache_sources: refresh_issues({}): wrote {} entries",
                config.slug,
                issues.len()
            ));
        }
    }

    Ok(())
}

/// Builds the GraphQL query string for fetching open PRs with per-check context data.
///
/// The query fetches up to 100 statusCheckRollup contexts per PR using inline
/// fragments on both `CheckRun` and `StatusContext` node types.
pub fn pr_graphql_query(owner: &str, name: &str) -> String {
    format!(
        r#"query {{
  repository(owner: "{owner}", name: "{name}") {{
    pullRequests(first: 100, states: OPEN, orderBy: {{field: CREATED_AT, direction: DESC}}) {{
      nodes {{
        number
        headRefName
        baseRefName
        state
        reviewDecision
        mergeable
        # 100 is deliberately high: GitHub rarely surfaces more than ~20 labels
        # on a single PR, and truncation would silently drop a phase label,
        # flipping the computed `phase` projection from its real value to null.
        labels(first: 100) {{
          nodes {{
            name
          }}
        }}
        reviewThreads(first: 100) {{
          nodes {{
            isResolved
            isOutdated
            comments(last: 1) {{
              nodes {{
                createdAt
              }}
            }}
          }}
        }}
        closingIssuesReferences(first: 5) {{
          nodes {{
            number
            state
            stateReason
          }}
        }}
        commits(last: 1) {{
          nodes {{
            commit {{
              statusCheckRollup {{
                state
                contexts(first: 100) {{
                  totalCount
                  nodes {{
                    __typename
                    ... on CheckRun {{
                      name
                      conclusion
                      status
                    }}
                    ... on StatusContext {{
                      context
                      state
                    }}
                  }}
                }}
              }}
            }}
          }}
        }}
      }}
    }}
  }}
}}"#
    )
}

/// Sanitizes a branch name into a valid GraphQL alias.
///
/// Replaces non-alphanumeric characters with `_` and prefixes with `b_` to
/// ensure the alias starts with a letter (GraphQL identifiers must match `[_a-zA-Z][_a-zA-Z0-9]*`).
fn sanitize_branch_alias(branch: &str) -> String {
    let clean: String = branch
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() { c } else { '_' })
        .collect();
    format!("b_{clean}")
}

/// Builds a per-branch GraphQL query using aliases.
///
/// For each branch, adds an aliased `pullRequests(headRefName: "...", first: 1)` field
/// that returns the most recent PR regardless of state (open, merged, closed).
/// All aliases share a `PrFields` fragment for the full field set.
///
/// Returns the complete query string ready for `gh api graphql -f query=...`.
pub fn pr_graphql_query_per_branch(owner: &str, name: &str, branches: &[String]) -> String {
    if branches.is_empty() {
        return format!(r#"query {{ repository(owner: "{owner}", name: "{name}") {{ name }} }}"#);
    }

    let aliases: Vec<String> = branches
        .iter()
        .map(|branch| {
            let alias = sanitize_branch_alias(branch);
            format!(
                r#"    {alias}: pullRequests(headRefName: "{branch}", first: 1, orderBy: {{field: CREATED_AT, direction: DESC}}) {{
      nodes {{ ...PrFields }}
    }}"#
            )
        })
        .collect();

    format!(
        r#"query {{
  repository(owner: "{owner}", name: "{name}") {{
    defaultBranchRef {{
      name
      target {{
        ... on Commit {{
          statusCheckRollup {{
            state
          }}
        }}
      }}
    }}
{aliases}
  }}
}}

fragment PrFields on PullRequest {{
  number
  headRefName
  baseRefName
  title
  state
  isDraft
  author {{ login }}
  reviewDecision
  mergeable
  labels(first: 100) {{ nodes {{ name }} }}
  reviewRequests(first: 20) {{ nodes {{ requestedReviewer {{ ... on User {{ login }} ... on Team {{ name }} }} }} }}
  reviews(last: 20) {{ nodes {{ author {{ login }} state submittedAt }} }}
  reviewThreads(first: 100) {{ nodes {{ isResolved isOutdated comments(last:1){{nodes{{createdAt}}}} }} }}
  closingIssuesReferences(first: 5) {{ nodes {{ number state stateReason }} }}
  additions
  deletions
  createdAt
  updatedAt
  commits(last: 1) {{
    nodes {{
      commit {{
        pushedDate
        statusCheckRollup {{
          state
          contexts(first: 100) {{
            totalCount
            nodes {{
              __typename
              ... on CheckRun {{ name conclusion status detailsUrl }}
              ... on StatusContext {{ context state }}
            }}
          }}
        }}
      }}
    }}
  }}
}}"#,
        aliases = aliases.join("\n")
    )
}

/// Parses a per-branch aliased GraphQL PR response into `Vec<CachedPr>`.
///
/// Expects shape: `{"data":{"repository":{"b_branch_name":{"nodes":[...]}, ...}}}`.
/// Iterates over all keys in `data.repository` that start with `b_`, extracts the
/// first node from each alias, and maps it to a `CachedPr` with all enriched fields.
pub fn parse_prs_graphql_per_branch(json: &str, matcher: &GateMatcher) -> Vec<CachedPr> {
    let root: serde_json::Value = match serde_json::from_str(json) {
        Ok(v) => v,
        Err(e) => {
            LOG.warn(&format!(
                "cache_sources: failed to parse per-branch PRs GraphQL JSON: {e}"
            ));
            return Vec::new();
        }
    };

    let repo = match root["data"]["repository"].as_object() {
        Some(obj) => obj,
        None => {
            LOG.warn("cache_sources: per-branch PRs response missing data.repository");
            return Vec::new();
        }
    };

    let mut prs = Vec::new();

    for (key, val) in repo {
        if !key.starts_with("b_") {
            continue;
        }

        let nodes = match val["nodes"].as_array() {
            Some(n) => n,
            None => continue,
        };

        let Some(v) = nodes.first() else { continue };

        let Some(number) = v["number"].as_u64().map(|n| n as u32) else {
            continue;
        };

        let branch = v["headRefName"].as_str().unwrap_or("").to_string();
        let base = v["baseRefName"].as_str().unwrap_or("");
        let state = v["state"].as_str().unwrap_or("OPEN").to_lowercase();

        if branch == base {
            continue;
        }

        let title = v["title"].as_str().map(|s| s.to_string());
        let is_draft = v["isDraft"].as_bool();
        let author = v["author"]["login"].as_str().map(|s| s.to_string());

        let review_decision = match v["reviewDecision"].as_str().unwrap_or("") {
            "APPROVED" => Some("approved".to_string()),
            "CHANGES_REQUESTED" => Some("changes_requested".to_string()),
            "REVIEW_REQUIRED" => Some("review_required".to_string()),
            _ => None,
        };

        let requested_reviewers: Vec<String> = v["reviewRequests"]["nodes"]
            .as_array()
            .map(|arr| {
                arr.iter()
                    .filter_map(|n| {
                        let reviewer = &n["requestedReviewer"];
                        reviewer["login"]
                            .as_str()
                            .or_else(|| reviewer["name"].as_str())
                            .map(|s| s.to_string())
                    })
                    .collect()
            })
            .unwrap_or_default();

        let reviews: Vec<CachedReview> = v["reviews"]["nodes"]
            .as_array()
            .map(|arr| {
                arr.iter()
                    .filter_map(|r| {
                        let author = r["author"]["login"].as_str()?.to_string();
                        let state = r["state"].as_str().unwrap_or("").to_string();
                        let submitted_at = r["submittedAt"].as_str().map(|s| s.to_string());
                        Some(CachedReview {
                            author,
                            state,
                            submitted_at,
                        })
                    })
                    .collect()
            })
            .unwrap_or_default();

        let additions = v["additions"].as_u64().map(|n| n as u32);
        let deletions = v["deletions"].as_u64().map(|n| n as u32);
        let created_at = v["createdAt"].as_str().map(|s| s.to_string());
        let updated_at = v["updatedAt"].as_str().map(|s| s.to_string());
        let last_commit_pushed_at = v["commits"]["nodes"]
            .as_array()
            .and_then(|arr| arr.last())
            .and_then(|n| n["commit"]["pushedDate"].as_str())
            .map(|s| s.to_string());

        let (ci_code_state, ci_gate_state, ci_checks) = derive_ci_state_graphql(v, number, matcher);
        let checks_state = ci_code_state.clone();

        let has_conflicts = v["mergeable"].as_str().unwrap_or("") == "CONFLICTING";

        let unresolved_threads = count_actionable_threads(&v["reviewThreads"]["nodes"]);
        let unresolved_thread_comment_timestamps =
            collect_thread_comment_timestamps(&v["reviewThreads"]["nodes"]);

        let first_linked = v["closingIssuesReferences"]["nodes"]
            .as_array()
            .and_then(|arr| arr.first());
        let linked_issue = first_linked
            .and_then(|issue| issue["number"].as_u64())
            .map(|n| n as u32);
        let linked_issue_state = first_linked.and_then(|issue| {
            let s = issue["state"].as_str()?;
            let reason = issue["stateReason"].as_str().unwrap_or("");
            let normalised = if s == "OPEN" {
                "open"
            } else if reason == "COMPLETED" {
                "completed"
            } else {
                "closed"
            };
            Some(normalised.to_string())
        });

        let labels = v["labels"]["nodes"]
            .as_array()
            .map(|arr| {
                arr.iter()
                    .filter_map(|l| l["name"].as_str().map(|s| s.to_string()))
                    .collect()
            })
            .unwrap_or_default();

        prs.push(CachedPr {
            number,
            branch,
            linked_issue,
            state,
            review_decision,
            checks_state,
            ci_code_state,
            ci_gate_state,
            ci_checks,
            has_conflicts,
            unresolved_threads,
            linked_issue_state,
            labels,
            title,
            is_draft,
            author,
            requested_reviewers,
            reviews,
            additions,
            deletions,
            created_at,
            updated_at,
            last_commit_pushed_at,
            unresolved_thread_comment_timestamps,
        });
    }

    prs
}

/// Extracts repo-level metadata from a per-branch PR GraphQL response.
///
/// Reads the `repository.defaultBranchRef` field that is included by
/// [`pr_graphql_query_per_branch`] alongside the branch aliases. Returns a
/// [`CachedRepoMeta`] with the default branch name and the CI rollup state of
/// its HEAD commit.
///
/// The same JSON is passed to both this function and
/// [`parse_prs_graphql_per_branch`] so that only one API call is needed for
/// both datasets.
pub fn parse_repo_meta_from_pr_response(json: &str) -> CachedRepoMeta {
    let root: serde_json::Value = match serde_json::from_str(json) {
        Ok(v) => v,
        Err(e) => {
            LOG.warn(&format!(
                "cache_sources: failed to parse repo meta from PR response: {e}"
            ));
            return CachedRepoMeta::default();
        }
    };

    let default_branch_ref = &root["data"]["repository"]["defaultBranchRef"];
    if default_branch_ref.is_null() {
        return CachedRepoMeta::default();
    }

    let default_branch = default_branch_ref["name"].as_str().map(|s| s.to_string());

    let main_ci_state = default_branch_ref["target"]["statusCheckRollup"]["state"]
        .as_str()
        .map(|s| s.to_string());

    CachedRepoMeta {
        default_branch,
        main_ci_state,
    }
}

/// Parses output of `git branch -vv` to extract ahead/behind counts per branch.
///
/// Returns a map of branch name → `(ahead, behind)`. Branches without a
/// tracking remote, or tracking remotes with no divergence count, return
/// `(0, 0)`.
///
/// Input format:
/// ```text
/// * main                abc1234 [origin/main] Latest commit
///   issue42/fix-bug     def5678 [origin/issue42/fix-bug: ahead 3, behind 1] Some commit
///   feat/new-thing      ghi9012 [origin/feat/new-thing: ahead 2] Another commit
/// ```
pub fn parse_git_ahead_behind(output: &str) -> HashMap<String, (u32, u32)> {
    let mut result = HashMap::new();

    for line in output.lines() {
        // Strip the leading "* " or "  " marker and trim.
        let stripped = if line.starts_with("* ") || line.starts_with("  ") {
            &line[2..]
        } else {
            line
        };

        // Branch name is the first whitespace-separated token.
        let mut parts = stripped.splitn(2, ' ');
        let branch = match parts.next() {
            Some(b) if !b.is_empty() => b.to_string(),
            _ => continue,
        };
        let rest = parts.next().unwrap_or("").trim();

        // Extract the "[origin/...: ahead N, behind M]" section if present.
        let (ahead, behind) = if let Some(bracket_start) = rest.find('[') {
            if let Some(bracket_end) = rest.find(']') {
                let bracket_content = &rest[bracket_start + 1..bracket_end];
                let ahead = bracket_content
                    .split("ahead ")
                    .nth(1)
                    .and_then(|s| s.split([',', ']']).next())
                    .and_then(|s| s.trim().parse::<u32>().ok())
                    .unwrap_or(0);
                let behind = bracket_content
                    .split("behind ")
                    .nth(1)
                    .and_then(|s| s.split([',', ']']).next())
                    .and_then(|s| s.trim().parse::<u32>().ok())
                    .unwrap_or(0);
                (ahead, behind)
            } else {
                (0, 0)
            }
        } else {
            (0, 0)
        };

        result.insert(branch, (ahead, behind));
    }

    result
}

/// Parses output of `git for-each-ref --format='%(refname:short) %(committerdate:iso-strict)' refs/heads/`
/// to extract the last commit timestamp per branch.
///
/// Returns a map of branch name → ISO 8601 date string.
///
/// Input format:
/// ```text
/// main 2026-04-10T10:00:00-07:00
/// issue42/fix-bug 2026-04-12T14:30:00-07:00
/// ```
pub fn parse_git_last_commit_dates(output: &str) -> HashMap<String, String> {
    let mut result = HashMap::new();

    for line in output.lines() {
        let trimmed = line.trim();
        if trimmed.is_empty() {
            continue;
        }
        // Split on the LAST space: branch names may contain slashes but not spaces,
        // and the date is always the last token.
        if let Some(pos) = trimmed.rfind(' ') {
            let branch = trimmed[..pos].trim().to_string();
            let date = trimmed[pos + 1..].trim().to_string();
            if !branch.is_empty() && !date.is_empty() {
                result.insert(branch, date);
            }
        }
    }

    result
}

/// Collects the set of branches to look up PRs for, given local and remote
/// worktree cache entries.
///
/// Rules:
/// - Skip bare worktrees (they have no branch checked out).
/// - Skip the repo's default branch (a PR whose head is `main` is
///   structurally not meaningful for worktree work).
/// - Dedupe: a branch checked out in both a local and a remote worktree
///   produces a single query.
///
/// Remote-only worktrees were previously invisible to this collection, so
/// merged PRs for remote-only branches never made it into the cache and the
/// row rendered as blank Coding status forever (see #256).
pub fn collect_pr_query_branches(
    local: &[CachedWorktree],
    remote: &[CachedWorktree],
) -> Vec<String> {
    local
        .iter()
        .chain(remote.iter())
        .filter(|w| !w.is_bare && !crate::derive::is_default_branch(&w.branch))
        .map(|w| w.branch.clone())
        .collect::<std::collections::HashSet<_>>()
        .into_iter()
        .collect()
}

/// Fetches open GitHub PRs for `config.slug` via GraphQL and writes to the PRs cache.
///
/// Uses GraphQL to get closingIssuesReferences (linked issues), reviewThreads,
/// and per-check CI context data (up to 100 checks per PR), which are not
/// available via `gh pr list --json`.
/// On API failure the error is logged and the existing cache is left intact.
pub fn refresh_prs(config: &RepoConfig) -> anyhow::Result<()> {
    let path = cache::cache_path(config.owner(), config.repo_name(), "prs");

    // SWR fast path: skip the gh api call when the PRs cache is Fresh.
    let cached_file = cache::read_cache::<CachedPr>(&path);
    let swr_config = crate::swr::SwrConfig::default_for(crate::swr::SwrKind::GithubPrs);
    if crate::swr::classify_cache_file(&cached_file, chrono::Utc::now(), swr_config)
        == crate::swr::SwrState::Fresh
    {
        LOG.info(&format!(
            "cache_sources: refresh_prs({}): swr=Fresh, skipping gh api",
            config.slug
        ));
        return Ok(());
    }

    // Read both local and remote worktree caches to collect every branch
    // that needs a PR lookup. See `collect_pr_query_branches` for the
    // filtering + dedupe rules.
    let wt_path = cache::cache_path(config.owner(), config.repo_name(), "worktrees");
    let remote_wt_path = cache::cache_path(config.owner(), config.repo_name(), "remote_worktrees");
    let local_wts: Vec<CachedWorktree> = cache::read_cache::<CachedWorktree>(&wt_path).entries;
    let remote_wts: Vec<CachedWorktree> =
        cache::read_cache::<CachedWorktree>(&remote_wt_path).entries;

    let branches = collect_pr_query_branches(&local_wts, &remote_wts);

    if branches.is_empty() {
        LOG.info(&format!(
            "cache_sources: refresh_prs({}): no feature branches, skipping",
            config.slug
        ));
        cache::write_cache_if_nonempty(&path, &Vec::<CachedPr>::new())?;
        return Ok(());
    }

    let query = pr_graphql_query_per_branch(config.owner(), config.repo_name(), &branches);

    // Build gate matcher from global config so classification is consistent
    // with the loaded user preferences.
    let global_cfg = crate::global_config::load_global_config();
    let matcher = GateMatcher::new(&global_cfg.ci_gate_patterns);

    let out = run_local("gh", &["api", "graphql", "-f", &format!("query={query}")]);

    match out {
        Err(e) => {
            LOG.warn(&format!("cache_sources: refresh_prs({}): {e}", config.slug));
            return Ok(());
        }
        Ok(json) => {
            let prs = parse_prs_graphql_per_branch(&json, &matcher);
            cache::write_cache_if_nonempty(&path, &prs)?;
            LOG.info(&format!(
                "cache_sources: refresh_prs({}): wrote {} entries",
                config.slug,
                prs.len()
            ));

            // Parse repo-level meta (default branch, main CI state) from the
            // same response and write to its own cache file.
            let repo_meta = parse_repo_meta_from_pr_response(&json);
            let meta_path = cache::cache_path(config.owner(), config.repo_name(), "repo_meta");
            cache::write_cache(&meta_path, &[repo_meta])?;
        }
    }

    Ok(())
}

/// Fetches git worktrees for `config.path` and writes to the worktrees cache.
///
/// After parsing the worktree list, also runs `git branch -vv` and
/// `git for-each-ref` to populate `ahead`, `behind`, and `last_commit_at`
/// fields on each non-bare worktree entry.
///
/// On failure the error is logged and the existing cache is left intact.
pub fn refresh_worktrees(config: &RepoConfig) -> anyhow::Result<()> {
    let path = cache::cache_path(config.owner(), config.repo_name(), "worktrees");

    let out = run_local_in("git", &["worktree", "list", "--porcelain"], &config.path);

    match out {
        Err(e) => {
            LOG.warn(&format!(
                "cache_sources: refresh_worktrees({}): {e}",
                config.slug
            ));
            return Ok(());
        }
        Ok(porcelain) => {
            let mut worktrees = parse_worktree_porcelain(&porcelain);

            // Enrich with ahead/behind counts from `git branch -vv`.
            let ahead_behind = run_local_in("git", &["branch", "-vv"], &config.path)
                .map(|out| parse_git_ahead_behind(&out))
                .unwrap_or_default();

            // Enrich with last commit timestamps from `git for-each-ref`.
            let commit_dates = run_local_in(
                "git",
                &[
                    "for-each-ref",
                    "--format=%(refname:short) %(committerdate:iso-strict)",
                    "refs/heads/",
                ],
                &config.path,
            )
            .map(|out| parse_git_last_commit_dates(&out))
            .unwrap_or_default();

            for wt in &mut worktrees {
                if !wt.is_bare {
                    if let Some(&(a, b)) = ahead_behind.get(&wt.branch) {
                        wt.ahead = if a > 0 { Some(a) } else { None };
                        wt.behind = if b > 0 { Some(b) } else { None };
                    }
                    wt.last_commit_at = commit_dates.get(&wt.branch).cloned();
                }
            }

            cache::write_cache_if_nonempty(&path, &worktrees)?;
            LOG.info(&format!(
                "cache_sources: refresh_worktrees({}): wrote {} entries",
                config.slug,
                worktrees.len()
            ));
        }
    }

    Ok(())
}

/// Fetches tmux sessions (local or remote) and writes to the tmux sessions cache.
///
/// Pass `None` for local sessions, or `Some("user@host")` for remote sessions.
///
/// On failure the error is logged and the existing cache is left intact.
pub fn refresh_tmux_sessions(host: Option<&str>) -> anyhow::Result<()> {
    let cache_path = cache::tmux_cache_path(host);

    // SWR fast path — remote tmux list over SSH only. Local tmux is always
    // fast enough to re-run; skipping it would delay session state updates
    // with no cost savings.
    if host.is_some() {
        let cached_file = cache::read_cache::<CachedTmuxSession>(&cache_path);
        let swr_config = crate::swr::SwrConfig::default_for(crate::swr::SwrKind::RemoteSessions);
        if crate::swr::classify_cache_file(&cached_file, chrono::Utc::now(), swr_config)
            == crate::swr::SwrState::Fresh
        {
            LOG.info(&format!(
                "cache_sources: refresh_tmux_sessions({}): swr=Fresh, skipping ssh",
                host.unwrap_or("local"),
            ));
            return Ok(());
        }
    }

    let sessions_out = match host {
        None => run_local("tmux", &["list-panes", "-a", "-F", TMUX_SESSION_FORMAT]),
        Some(h) => {
            let cmd = format!("tmux list-panes -a -F '{}'", TMUX_SESSION_FORMAT);
            remote::ssh_exec(h, &cmd)
        }
    };

    let raw_output = match sessions_out {
        Err(e) => {
            LOG.warn(&format!(
                "cache_sources: refresh_tmux_sessions(host={:?}): {e}",
                host
            ));
            return Ok(());
        }
        Ok(s) => s,
    };

    let sessions = parse_tmux_sessions_from_panes(
        raw_output.as_str(),
        host,
        |session_name| {
            // -s lists panes across ALL windows in the session.
            // Format: "window.pane\ttitle:command" for parse_pane_lines.
            let pane_fmt = "#{window_index}.#{pane_index}\t#{window_name}\t#{window_active}\t#{window_layout}\t#{pane_current_path}\t#{pane_active}\t#{pane_title}:#{pane_current_command}";
            match host {
                None => run_local(
                    "tmux",
                    &["list-panes", "-s", "-t", session_name, "-F", pane_fmt],
                )
                .unwrap_or_default(),
                Some(h) => {
                    let cmd = format!(
                        "tmux list-panes -s -t {} -F '{}'",
                        remote::shell_escape(session_name),
                        pane_fmt
                    );
                    remote::ssh_exec(h, &cmd).unwrap_or_default()
                }
            }
        },
        |session_name| {
            let raw = match host {
                None => run_local(
                    "tmux",
                    &["capture-pane", "-p", "-t", session_name, "-S", "-5"],
                )
                .unwrap_or_default(),
                Some(h) => {
                    let cmd = format!(
                        "tmux capture-pane -p -t {} -S -5",
                        remote::shell_escape(session_name)
                    );
                    remote::ssh_exec(h, &cmd).unwrap_or_default()
                }
            };
            raw.lines().map(|l| l.to_string()).collect()
        },
    );

    cache::write_cache_if_nonempty(&cache_path, &sessions)?;
    LOG.info(&format!(
        "cache_sources: refresh_tmux_sessions(host={:?}): wrote {} entries",
        host,
        sessions.len()
    ));

    Ok(())
}

/// Fetches worktrees from a single remote via the appropriate `RemoteAdapter`
/// and writes them to the `remote_worktrees` cache.
///
/// Dispatch is driven by `remote_cfg.kind` (`Remmy`, `BoxdShared`, `BoxdFork`)
/// through `RemoteAdapter::from_config`. The `BoxdFork` adapter produces one
/// flat-layout worktree per live fork VM and tags each with its own fork host
/// (`boxd@<fork>.boxd.sh`), not with `remote_cfg.host`.
///
/// Cache merge semantics: entries from the current remote (identified by
/// exactly `remote_cfg.host`) are replaced; entries from other hosts —
/// including per-fork hosts under a BoxdFork remote — are preserved in the
/// merged output.
///
/// On SSH or parse failure the error is logged and the existing cache is left
/// intact (adapters absorb SSH failures as `Ok(vec![])`, so this function only
/// writes once it has produced at least one entry).
pub fn refresh_remote_worktrees(
    config: &RepoConfig,
    remote_cfg: &crate::global_config::RemoteConfig,
) -> anyhow::Result<()> {
    let cache_path = cache::cache_path(config.owner(), config.repo_name(), "remote_worktrees");

    // SWR fast path: skip the adapter call entirely when the cache is Fresh.
    // Stale → re-fetch (this refresh IS the background revalidation from the
    // caller's point of view, since `refresh_and_build` already runs remotes
    // concurrently off the main thread). Missing / Expired → re-fetch.
    let cached_file = cache::read_cache::<CachedWorktree>(&cache_path);
    let swr_config = crate::swr::SwrConfig::default_for(crate::swr::SwrKind::RemoteWorktrees);
    let state = crate::swr::classify_cache_file(&cached_file, chrono::Utc::now(), swr_config);
    if state == crate::swr::SwrState::Fresh {
        LOG.info(&format!(
            "cache_sources: refresh_remote_worktrees({}, {}): swr=Fresh, skipping adapter",
            config.slug, remote_cfg.host,
        ));
        return Ok(());
    }

    let adapter = crate::remote_adapter::RemoteAdapter::from_config(
        remote_cfg,
        Box::new(crate::remote_adapter::ProcessSshExec),
    );

    let mut worktrees = match adapter.list_worktrees() {
        Ok(wts) => wts,
        Err(e) => {
            LOG.warn(&format!(
                "cache_sources: refresh_remote_worktrees({}, {}): {e}",
                config.slug, remote_cfg.host,
            ));
            return Err(e);
        }
    };

    // Bare-layout adapters (`Remmy`, `BoxdShared`) tag with `remote_cfg.host`;
    // `BoxdFork` tags with per-fork `boxd@<fork>.host`. For merge-by-host to
    // work for the bare-layout case, ensure the host is set (the adapter
    // already does this, but be defensive for empty-list cases).
    for wt in &mut worktrees {
        if wt.host.is_none() {
            wt.host = Some(remote_cfg.host.clone());
        }
    }

    // Collect the set of hosts this refresh is replacing.
    let replaced_hosts: std::collections::HashSet<String> =
        worktrees.iter().filter_map(|w| w.host.clone()).collect();

    // Always replace `remote_cfg.host` so an empty list from a reachable host
    // clears its previous entries (a fork that was live and is now gone would
    // otherwise linger forever).
    let existing: Vec<CachedWorktree> = cache::read_cache::<CachedWorktree>(&cache_path).entries;
    let mut from_other_hosts: Vec<CachedWorktree> = Vec::new();
    for w in existing {
        let Some(host) = &w.host else {
            continue;
        };
        if host == &remote_cfg.host {
            // Replaced: this is the (single-host) remote we just refreshed.
            continue;
        }
        if replaced_hosts.contains(host) {
            // Replaced: still present in the new fetch under the same host.
            continue;
        }
        // If this remote is a BoxdFork, a previously-cached host that did
        // NOT reappear in the new `list --json` is a lost fork VM. Emit a
        // `worktree.remote_lost` event (AC5 feature.feature:306, AC7 #264).
        // Other remote kinds are single-host: their `host == remote_cfg.host`
        // case above already handled them, so this branch doesn't fire.
        if remote_cfg.kind == crate::remote_adapter::RemoteKind::BoxdFork {
            crate::events::log_worktree_remote_lost(
                &config.slug,
                &remote_cfg.name,
                kind_str(remote_cfg.kind),
                host,
                &w.branch,
                &w.path,
            );
            // Lost forks are NOT re-added to the cache.
            continue;
        }
        from_other_hosts.push(w);
    }
    worktrees.extend(from_other_hosts);

    cache::write_cache_if_nonempty(&cache_path, &worktrees)?;
    LOG.info(&format!(
        "cache_sources: refresh_remote_worktrees({}, {}): wrote {} entries via {:?} adapter",
        config.slug,
        remote_cfg.host,
        worktrees.len(),
        remote_cfg.kind,
    ));

    Ok(())
}

/// Maps a `RemoteKind` to its on-the-wire kebab-case string, matching the
/// serde serialization and the `remote_type` field on
/// `worktree.remote_lost` events.
pub(crate) fn kind_str(kind: crate::remote_adapter::RemoteKind) -> &'static str {
    match kind {
        crate::remote_adapter::RemoteKind::Remmy => "remmy",
        crate::remote_adapter::RemoteKind::BoxdShared => "boxd-shared",
        crate::remote_adapter::RemoteKind::BoxdFork => "boxd-fork",
        crate::remote_adapter::RemoteKind::OrchardProxy => "orchard-proxy",
    }
}

/// Returns fork hosts present in `old` but absent from `live`.
///
/// Pure helper — no IO. Extracted so the vanished-fork detection logic
/// is unit-testable without touching the filesystem.
///
/// Result is sorted.
pub fn vanished_hosts(
    old: &std::collections::HashSet<String>,
    live: &std::collections::HashSet<String>,
) -> Vec<String> {
    let mut out: Vec<String> = old.difference(live).cloned().collect();
    out.sort();
    out
}

/// Returns `true` if `host` (bare hostname, no `user@`) is either exactly the
/// `golden` host, or a subdomain of it separated by a `.` boundary.
///
/// This prevents a suffix-only match such as `"myboxd.sh"` passing for
/// golden `"boxd.sh"`.
///
/// # Examples
///
/// ```
/// # use orchard::cache_sources::is_fork_of_golden;
/// assert!(is_fork_of_golden("issue1.boxd.sh", "boxd.sh"));
/// assert!(is_fork_of_golden("boxd.sh", "boxd.sh"));
/// assert!(!is_fork_of_golden("myboxd.sh", "boxd.sh"));
/// assert!(!is_fork_of_golden("xboxd.sh", "boxd.sh"));
/// ```
pub fn is_fork_of_golden(host: &str, golden: &str) -> bool {
    host == golden || host.ends_with(&format!(".{}", golden))
}

/// Reads fork hosts for a `BoxdFork` remote from the `remote_worktrees` cache
/// file under `cache_dir`, applying the [`is_fork_of_golden`] filter.
///
/// Returns the set of full host strings (e.g. `"boxd@issue1.boxd.sh"`) whose
/// bare hostname (the part after `@`) is a subdomain of the golden host.
/// For non-`BoxdFork` remotes, returns an empty set immediately.
///
/// When the cache file does not exist (cold start) or is unreadable, returns
/// an empty set. When `refresh_remote_worktrees` was skipped because its SWR
/// window was still Fresh, the cache reflects the most recent successful fetch
/// — which is accurate enough for tmux vanished-fork detection.
pub(crate) fn fork_hosts_from_cache_in(
    config: &crate::global_config::RepoConfig,
    remote_cfg: &crate::global_config::RemoteConfig,
    cache_dir: &std::path::Path,
) -> std::collections::HashSet<String> {
    use crate::remote_adapter::RemoteKind;

    if remote_cfg.kind != RemoteKind::BoxdFork {
        return std::collections::HashSet::new();
    }

    let cache_path = cache::cache_path_in(
        config.owner(),
        config.repo_name(),
        "remote_worktrees",
        cache_dir,
    );
    let entries: Vec<CachedWorktree> = cache::read_cache::<CachedWorktree>(&cache_path).entries;

    let golden = &remote_cfg.host;
    entries
        .into_iter()
        .filter_map(|w| w.host)
        .filter(|h| {
            h.split('@')
                .nth(1)
                .map(|after_at| is_fork_of_golden(after_at, golden.as_str()))
                .unwrap_or(false)
        })
        .collect()
}

/// Reads fork hosts for a `BoxdFork` remote from the production
/// `remote_worktrees` cache file, applying the [`is_fork_of_golden`] boundary
/// check.
///
/// See [`fork_hosts_from_cache_in`] for full semantics.
pub(crate) fn fork_hosts_from_cache(
    config: &crate::global_config::RepoConfig,
    remote_cfg: &crate::global_config::RemoteConfig,
) -> std::collections::HashSet<String> {
    fork_hosts_from_cache_in(config, remote_cfg, &cache::cache_dir())
}

/// Snapshots the fork hosts for a `BoxdFork` remote by reading the current
/// `remote_worktrees` cache — before any mutation by `refresh_remote_worktrees`.
///
/// Returns the set of host strings (e.g. `"boxd@issue1.boxd.sh"`) that were
/// cached from prior refreshes of this remote. For non-`BoxdFork` remotes,
/// returns an empty set — single-host remotes do not need per-fork tracking.
///
/// Callers must invoke this **before** `refresh_remote_worktrees` so the
/// snapshot reflects the pre-mutation state used to detect vanished forks.
pub fn snapshot_fork_hosts_for_remote(
    config: &crate::global_config::RepoConfig,
    remote_cfg: &crate::global_config::RemoteConfig,
) -> std::collections::HashSet<String> {
    snapshot_fork_hosts_for_remote_in(config, remote_cfg, &cache::cache_dir())
}

/// Like [`snapshot_fork_hosts_for_remote`] but takes an explicit `cache_dir`
/// so tests can redirect file I/O to a `TempDir` without mutating `HOME`.
pub(crate) fn snapshot_fork_hosts_for_remote_in(
    config: &crate::global_config::RepoConfig,
    remote_cfg: &crate::global_config::RemoteConfig,
    cache_dir: &std::path::Path,
) -> std::collections::HashSet<String> {
    fork_hosts_from_cache_in(config, remote_cfg, cache_dir)
}

/// Deletes tmux caches for `vanished_hosts` and emits `session.remote_lost`
/// events with the real host strings (not derived from filenames).
///
/// For each host in `vanished_hosts`, removes the file at
/// `cache::tmux_cache_path(Some(host))` and emits the event. File-not-found
/// errors are silently ignored (idempotent).
pub fn delete_vanished_fork_caches(
    vanished: &[String],
    slug: &str,
    remote_name: &str,
    remote_kind: crate::remote_adapter::RemoteKind,
) {
    delete_vanished_fork_caches_in(
        vanished,
        slug,
        remote_name,
        remote_kind,
        &cache::cache_dir(),
    );
}

/// Like [`delete_vanished_fork_caches`] but takes an explicit `cache_dir` so
/// tests can redirect file I/O to a `TempDir` without mutating `HOME`.
pub(crate) fn delete_vanished_fork_caches_in(
    vanished: &[String],
    slug: &str,
    remote_name: &str,
    remote_kind: crate::remote_adapter::RemoteKind,
    cache_dir: &std::path::Path,
) {
    for host in vanished {
        let path = cache::tmux_cache_path_in(Some(host), cache_dir);
        match std::fs::remove_file(&path) {
            Ok(()) => {
                LOG.info(&format!(
                    "cache_sources: delete_vanished_fork_caches: \
                     removed stale tmux cache for vanished fork: {host}"
                ));
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                // Already gone — idempotent.
            }
            Err(e) => {
                LOG.warn(&format!(
                    "cache_sources: delete_vanished_fork_caches: \
                     failed to remove stale cache for {host}: {e}"
                ));
            }
        }
        crate::events::log_session_remote_lost(slug, remote_name, kind_str(remote_kind), host);
    }
}

/// Refreshes tmux session caches for a single remote, dispatching per-fork
/// for `BoxdFork` remotes and falling back to a single-host call for
/// `Remmy` / `BoxdShared`.
///
/// For `BoxdFork`:
/// 1. Uses `old_hosts` (a pre-mutation snapshot from the caller, taken before
///    `refresh_remote_worktrees` was called) as the authoritative "previously
///    known" fork set. This avoids lossy filename reverse-engineering and the
///    cross-golden suffix ambiguity of the filesystem-scan approach.
/// 2. Calls the adapter to enumerate live fork hosts. Returns `Err` if the
///    golden host is unreachable (prevents false mass-deletion of caches on
///    a transient outage).
/// 3. Calls `refresh_tmux_sessions(Some(&fork_host))` per live fork. Per-fork
///    failures are logged and skipped — one unreachable fork doesn't abort the
///    whole remote.
/// 4. Detects forks in `old_hosts` that are absent from the live set; unlinks
///    the stale cache and emits `session.remote_lost` with the real host string.
///
/// For `Remmy` / `BoxdShared`: thin wrapper around
/// `refresh_tmux_sessions(Some(&remote_cfg.host))`, propagating its result.
/// The `old_hosts` parameter is ignored for these kinds.
pub fn refresh_remote_tmux_sessions(
    config: &crate::global_config::RepoConfig,
    remote_cfg: &crate::global_config::RemoteConfig,
    old_hosts: &std::collections::HashSet<String>,
) -> anyhow::Result<()> {
    use crate::remote_adapter::RemoteKind;

    match remote_cfg.kind {
        RemoteKind::Remmy | RemoteKind::BoxdShared => refresh_tmux_sessions(Some(&remote_cfg.host)),
        RemoteKind::BoxdFork => refresh_boxd_fork_tmux_sessions(config, remote_cfg, old_hosts),
        // OrchardProxy sessions are sourced from `orchard-tui --json`; tmux discovery
        // uses the fallback adapter path which is handled by RemoteAdapter dispatch.
        RemoteKind::OrchardProxy => Ok(()),
    }
}

/// Inner implementation for `BoxdFork` tmux session refresh.
///
/// Separated from `refresh_remote_tmux_sessions` to keep the match arm thin
/// and keep this logic testable.
///
/// Uses `old_hosts` (pre-mutation snapshot) rather than a filesystem scan, so
/// the authoritative "previously known" set is derived from real host strings
/// rather than reverse-engineered filenames. This eliminates the cross-golden
/// suffix ambiguity described in the PR #315 review.
///
/// Derives `live_hosts` from the `remote_worktrees` cache written by the
/// preceding `refresh_remote_worktrees` call — no second SSH round-trip.
/// If `refresh_remote_worktrees` was skipped because its SWR window was still
/// Fresh, the cache reflects the most recent successful fetch, which is equally
/// authoritative for tmux cleanup purposes.
fn refresh_boxd_fork_tmux_sessions(
    config: &crate::global_config::RepoConfig,
    remote_cfg: &crate::global_config::RemoteConfig,
    old_hosts: &std::collections::HashSet<String>,
) -> anyhow::Result<()> {
    // Derive live fork hosts from the remote_worktrees cache that was just
    // written (or left in place when SWR Fresh) by refresh_remote_worktrees.
    // This avoids a second SSH round-trip to the golden host.
    let live_hosts = fork_hosts_from_cache(config, remote_cfg);

    // Refresh tmux sessions for each live fork. Individual failures are
    // logged + skipped — a single unreachable fork shouldn't kill the rest.
    for fork_host in &live_hosts {
        if let Err(e) = refresh_tmux_sessions(Some(fork_host)) {
            LOG.warn(&format!(
                "cache_sources: refresh_boxd_fork_tmux_sessions: fork {fork_host}: {e}"
            ));
        }
    }

    // Delete caches for forks that were known before this cycle but have
    // now vanished from the live set. `old_hosts` is the pre-mutation
    // snapshot passed by the caller (taken before `refresh_remote_worktrees`
    // overwrote the cache), preserving the real host strings.
    let gone = vanished_hosts(old_hosts, &live_hosts);
    delete_vanished_fork_caches(&gone, &config.slug, &remote_cfg.name, remote_cfg.kind);

    Ok(())
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::collections::HashSet;

    // -- collect_pr_query_branches -----------------------------------------

    fn wt(path: &str, branch: &str, is_bare: bool, host: Option<&str>) -> CachedWorktree {
        CachedWorktree {
            path: path.to_string(),
            branch: branch.to_string(),
            is_bare,
            is_locked: false,
            host: host.map(str::to_string),
            ahead: None,
            behind: None,
            last_commit_at: None,
            layout: crate::cache::WorktreeLayout::Bare,
        }
    }

    #[test]
    fn collect_pr_query_branches_includes_remote_only_branches() {
        // Regression for #256: remote worktree's branch must appear in the
        // query list so merged PRs for remote-only worktrees get fetched.
        let local = vec![wt("/local/wt", "feat/local", false, None)];
        let remote = vec![wt(
            "/remote/wt",
            "issue247/session-elapsed-time",
            false,
            Some("boxd"),
        )];
        let branches = collect_pr_query_branches(&local, &remote);
        assert!(branches.contains(&"feat/local".to_string()));
        assert!(branches.contains(&"issue247/session-elapsed-time".to_string()));
    }

    #[test]
    fn collect_pr_query_branches_dedupes_branch_present_in_both_caches() {
        let local = vec![wt("/local/wt", "feat/shared", false, None)];
        let remote = vec![wt("/remote/wt", "feat/shared", false, Some("boxd"))];
        let branches = collect_pr_query_branches(&local, &remote);
        assert_eq!(branches.len(), 1);
        assert_eq!(branches[0], "feat/shared");
    }

    #[test]
    fn collect_pr_query_branches_skips_bare_worktrees_from_either_cache() {
        let local = vec![wt("/local/bare", "", true, None)];
        let remote = vec![wt("/remote/bare", "", true, Some("boxd"))];
        let branches = collect_pr_query_branches(&local, &remote);
        assert!(branches.is_empty());
    }

    #[test]
    fn collect_pr_query_branches_skips_default_branch_from_either_cache() {
        let local = vec![wt("/local/main", "main", false, None)];
        let remote = vec![wt("/remote/main", "main", false, Some("boxd"))];
        let branches = collect_pr_query_branches(&local, &remote);
        assert!(branches.is_empty());
    }

    #[test]
    fn collect_pr_query_branches_empty_inputs_returns_empty() {
        let branches = collect_pr_query_branches(&[], &[]);
        assert!(branches.is_empty());
    }

    // -- parse_issues_json --------------------------------------------------

    #[test]
    fn parse_issues_json_produces_correct_entries() {
        let json = serde_json::to_string(&json!([
            {
                "number": 10,
                "title": "Fix the thing",
                "state": "OPEN",
                "labels": [{"name": "bug"}, {"name": "priority:high"}]
            },
            {
                "number": 11,
                "title": "Add a feature",
                "state": "OPEN",
                "labels": []
            }
        ]))
        .unwrap();

        let issues = parse_issues_json(&json);
        assert_eq!(issues.len(), 2);

        assert_eq!(issues[0].number, 10);
        assert_eq!(issues[0].title, "Fix the thing");
        assert_eq!(issues[0].state, "open");
        assert_eq!(issues[0].labels, vec!["bug", "priority:high"]);

        assert_eq!(issues[1].number, 11);
        assert_eq!(issues[1].labels, Vec::<String>::new());
    }

    #[test]
    fn parse_issues_json_extracts_label_names_not_objects() {
        let json = serde_json::to_string(&json!([{
            "number": 1,
            "title": "Test",
            "state": "OPEN",
            "labels": [{"id": "LA_x1", "name": "bug", "color": "red"}]
        }]))
        .unwrap();

        let issues = parse_issues_json(&json);
        assert_eq!(issues[0].labels, vec!["bug"]);
    }

    #[test]
    fn parse_issues_json_invalid_json_returns_empty() {
        let issues = parse_issues_json("not json");
        assert!(issues.is_empty());
    }

    #[test]
    fn parse_issues_json_handles_closed_state() {
        let json = r#"[
            {"number": 1, "title": "Old issue", "state": "CLOSED", "labels": []},
            {"number": 2, "title": "Active issue", "state": "OPEN", "labels": [{"name": "bug"}]}
        ]"#;
        let issues = parse_issues_json(json);
        assert_eq!(issues.len(), 2);
        assert_eq!(issues[0].number, 1);
        assert_eq!(issues[0].state, "closed");
        assert_eq!(issues[1].number, 2);
        assert_eq!(issues[1].state, "open");
    }

    #[test]
    fn parse_issues_json_mixed_states_preserves_all() {
        let json = r#"[
            {"number": 1, "title": "Closed", "state": "CLOSED", "labels": []},
            {"number": 13, "title": "Open 1", "state": "OPEN", "labels": []},
            {"number": 14, "title": "Open 2", "state": "OPEN", "labels": []},
            {"number": 16, "title": "Open 3", "state": "OPEN", "labels": []},
            {"number": 18, "title": "Open 4", "state": "OPEN", "labels": []}
        ]"#;
        let issues = parse_issues_json(json);
        assert_eq!(
            issues.len(),
            5,
            "all issues should be preserved regardless of state"
        );
        let numbers: Vec<u32> = issues.iter().map(|i| i.number).collect();
        assert_eq!(numbers, vec![1, 13, 14, 16, 18]);
    }

    // -- parse_prs_graphql ---------------------------------------------------

    /// Returns a `GateMatcher` with no patterns (all checks → code bucket).
    ///
    /// Used by tests that don't need gate classification.
    fn empty_matcher() -> GateMatcher {
        GateMatcher::new(&[])
    }

    /// Helper to wrap PR nodes in the GraphQL response envelope.
    fn graphql_prs(nodes: serde_json::Value) -> String {
        serde_json::to_string(&json!({
            "data": {
                "repository": {
                    "pullRequests": {
                        "nodes": nodes
                    }
                }
            }
        }))
        .unwrap()
    }

    /// Helper to build a single PR node in GraphQL format.
    ///
    /// `check_state` is the top-level rollup state. When provided, a single
    /// synthetic `CheckRun` context is generated that maps to the same state,
    /// so `ci_code_state` and legacy `checks_state` are consistent.
    fn gql_pr_node(
        number: u32,
        branch: &str,
        review_decision: Option<&str>,
        mergeable: &str,
        check_state: Option<&str>,
        linked_issues: Vec<u32>,
        unresolved_threads: u32,
    ) -> serde_json::Value {
        gql_pr_node_with_contexts(
            number,
            branch,
            review_decision,
            mergeable,
            check_state,
            linked_issues,
            unresolved_threads,
            vec![],
        )
    }

    /// Helper to build a PR node with explicit context nodes.
    ///
    /// `check_state` generates a synthetic "ci" CheckRun when `Some`. Additional
    /// explicit context nodes from `contexts` are appended after the synthetic one.
    /// Each context tuple is `(typename, name_or_context, conclusion_or_state)`.
    #[allow(clippy::too_many_arguments)]
    fn gql_pr_node_with_contexts(
        number: u32,
        branch: &str,
        review_decision: Option<&str>,
        mergeable: &str,
        check_state: Option<&str>,
        linked_issues: Vec<u32>,
        unresolved_threads: u32,
        extra_contexts: Vec<serde_json::Value>,
    ) -> serde_json::Value {
        let threads: Vec<serde_json::Value> = (0..unresolved_threads)
            .map(|_| json!({"isResolved": false}))
            .collect();
        let issues: Vec<serde_json::Value> = linked_issues
            .into_iter()
            .map(|n| json!({"number": n, "state": "OPEN", "stateReason": null}))
            .collect();

        // Build context nodes from check_state shorthand.
        let mut context_nodes: Vec<serde_json::Value> = Vec::new();
        if let Some(s) = check_state {
            let synthetic = match s {
                "SUCCESS" | "EXPECTED" => {
                    json!({"__typename": "CheckRun", "name": "ci", "conclusion": "SUCCESS", "status": "COMPLETED"})
                }
                "FAILURE" | "ERROR" => {
                    json!({"__typename": "CheckRun", "name": "ci", "conclusion": "FAILURE", "status": "COMPLETED"})
                }
                "PENDING" => {
                    json!({"__typename": "CheckRun", "name": "ci", "conclusion": null, "status": "IN_PROGRESS"})
                }
                other => {
                    json!({"__typename": "CheckRun", "name": "ci", "conclusion": other, "status": "COMPLETED"})
                }
            };
            context_nodes.push(synthetic);
        }
        context_nodes.extend(extra_contexts);

        let total_count = context_nodes.len() as u64;
        let rollup = if total_count > 0 || check_state.is_some() {
            json!({
                "state": check_state.unwrap_or("SUCCESS"),
                "contexts": {
                    "totalCount": total_count,
                    "nodes": context_nodes
                }
            })
        } else {
            serde_json::Value::Null
        };

        let commits = json!([{"commit": {"statusCheckRollup": rollup}}]);

        json!({
            "number": number,
            "headRefName": branch,
            "state": "OPEN",
            "reviewDecision": review_decision,
            "mergeable": mergeable,
            "reviewThreads": {"nodes": threads},
            "closingIssuesReferences": {"nodes": issues},
            "commits": {"nodes": commits}
        })
    }

    #[test]
    fn parse_prs_graphql_produces_correct_entries() {
        let json = graphql_prs(json!([gql_pr_node(
            55,
            "feat/task-centric",
            Some("APPROVED"),
            "MERGEABLE",
            Some("SUCCESS"),
            vec![47],
            0
        )]));

        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs.len(), 1);

        let pr = &prs[0];
        assert_eq!(pr.number, 55);
        assert_eq!(pr.branch, "feat/task-centric");
        assert_eq!(pr.linked_issue, Some(47));
        assert_eq!(pr.linked_issue_state.as_deref(), Some("open"));
        assert_eq!(pr.state, "open");
        assert_eq!(pr.review_decision.as_deref(), Some("approved"));
        assert_eq!(pr.checks_state.as_deref(), Some("passing"));
        assert!(!pr.has_conflicts);
        assert_eq!(pr.unresolved_threads, 0);
    }

    #[test]
    fn parse_prs_graphql_changes_requested() {
        let json = graphql_prs(json!([gql_pr_node(
            10,
            "fix/something",
            Some("CHANGES_REQUESTED"),
            "MERGEABLE",
            None,
            vec![],
            0
        )]));

        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs[0].review_decision.as_deref(), Some("changes_requested"));
    }

    #[test]
    fn parse_prs_graphql_has_conflicts_when_conflicting() {
        let json = graphql_prs(json!([gql_pr_node(
            10,
            "fix/conflict",
            None,
            "CONFLICTING",
            None,
            vec![],
            0
        )]));

        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert!(prs[0].has_conflicts);
    }

    #[test]
    fn parse_prs_graphql_counts_unresolved_threads() {
        let json = graphql_prs(json!([gql_pr_node(
            10,
            "fix/threads",
            None,
            "MERGEABLE",
            None,
            vec![],
            3
        )]));

        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs[0].unresolved_threads, 3);
    }

    #[test]
    fn parse_prs_graphql_no_linked_issue_when_empty() {
        let json = graphql_prs(json!([gql_pr_node(
            99,
            "chore/no-issue",
            None,
            "MERGEABLE",
            None,
            vec![],
            0
        )]));

        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs[0].linked_issue, None);
        assert_eq!(prs[0].linked_issue_state, None);
    }

    #[test]
    fn parse_prs_graphql_linked_issue_closed_completed() {
        let issues = vec![json!({"number": 42, "state": "CLOSED", "stateReason": "COMPLETED"})];
        let pr_node = json!({
            "number": 10,
            "headRefName": "fix/done",
            "state": "OPEN",
            "reviewDecision": null,
            "mergeable": "MERGEABLE",
            "reviewThreads": {"nodes": []},
            "closingIssuesReferences": {"nodes": issues},
            "commits": {"nodes": [{"commit": {"statusCheckRollup": null}}]}
        });
        let json = graphql_prs(json!([pr_node]));

        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs[0].linked_issue, Some(42));
        assert_eq!(prs[0].linked_issue_state.as_deref(), Some("completed"));
    }

    #[test]
    fn parse_prs_graphql_linked_issue_closed_not_completed() {
        let issues = vec![json!({"number": 42, "state": "CLOSED", "stateReason": "NOT_PLANNED"})];
        let pr_node = json!({
            "number": 10,
            "headRefName": "fix/wontfix",
            "state": "OPEN",
            "reviewDecision": null,
            "mergeable": "MERGEABLE",
            "reviewThreads": {"nodes": []},
            "closingIssuesReferences": {"nodes": issues},
            "commits": {"nodes": [{"commit": {"statusCheckRollup": null}}]}
        });
        let json = graphql_prs(json!([pr_node]));

        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs[0].linked_issue, Some(42));
        assert_eq!(prs[0].linked_issue_state.as_deref(), Some("closed"));
    }

    #[test]
    fn parse_prs_graphql_checks_state_failing() {
        let json = graphql_prs(json!([gql_pr_node(
            10,
            "feat/ci-fail",
            None,
            "MERGEABLE",
            Some("FAILURE"),
            vec![],
            0
        )]));

        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs[0].checks_state.as_deref(), Some("failing"));
    }

    #[test]
    fn parse_prs_graphql_checks_state_pending() {
        let json = graphql_prs(json!([gql_pr_node(
            10,
            "feat/ci-pending",
            None,
            "MERGEABLE",
            Some("PENDING"),
            vec![],
            0
        )]));

        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs[0].checks_state.as_deref(), Some("pending"));
    }

    #[test]
    fn parse_prs_graphql_invalid_json_returns_empty() {
        assert!(parse_prs_graphql("{bad}", &empty_matcher()).is_empty());
    }

    // -- Task #8: GraphQL query contains required context fields ---------------

    /// Scenario: GraphQL query fetches up to 100 per-check contexts with inline
    /// fragments on CheckRun and StatusContext.
    #[test]
    fn graphql_query_contains_contexts_with_inline_fragments() {
        let query = pr_graphql_query("acme", "my-project");

        assert!(
            query.contains("statusCheckRollup"),
            "query must select statusCheckRollup"
        );
        assert!(
            query.contains("contexts(first: 100)"),
            "query must select contexts(first: 100)"
        );
        assert!(
            query.contains("... on CheckRun"),
            "query must have inline fragment on CheckRun"
        );
        assert!(
            query.contains("... on StatusContext"),
            "query must have inline fragment on StatusContext"
        );
        // CheckRun fields
        assert!(query.contains("name"), "query must select CheckRun.name");
        assert!(
            query.contains("conclusion"),
            "query must select CheckRun.conclusion"
        );
        assert!(
            query.contains("status"),
            "query must select CheckRun.status"
        );
        // StatusContext fields
        assert!(
            query.contains("context"),
            "query must select StatusContext.context"
        );
        // "state" appears in both CheckRun status field and StatusContext
        assert!(
            query.contains("state"),
            "query must select StatusContext.state"
        );
    }

    // -- Task #9: Parse CheckRun and StatusContext into uniform CheckInfo -------

    /// Scenario: Parsing normalizes CheckRun and StatusContext into uniform CheckInfo.
    #[test]
    fn parse_check_run_and_status_context_into_check_info() {
        let check_run_ctx = json!({"__typename": "CheckRun", "name": "test-unit", "conclusion": "SUCCESS", "status": "COMPLETED"});
        let status_ctx =
            json!({"__typename": "StatusContext", "context": "travis-ci", "state": "SUCCESS"});

        let pr_node = gql_pr_node_with_contexts(
            1,
            "feat/test-branch",
            None,
            "MERGEABLE",
            None,
            vec![],
            0,
            vec![check_run_ctx, status_ctx],
        );
        let json = graphql_prs(json!([pr_node]));
        let prs = parse_prs_graphql(&json, &empty_matcher());

        assert_eq!(prs.len(), 1);
        let pr = &prs[0];

        // Both checks land in the code bucket (empty matcher, no gate patterns).
        assert_eq!(pr.ci_checks.code.len(), 2, "expected 2 code checks");
        assert!(pr.ci_checks.gate.is_empty(), "expected 0 gate checks");

        // CheckRun uses .name field.
        let check_run_info = pr
            .ci_checks
            .code
            .iter()
            .find(|c| c.name == "test-unit")
            .expect("CheckRun should use .name field as check name");
        assert_eq!(
            check_run_info.state, "passing",
            "SUCCESS conclusion maps to passing"
        );

        // StatusContext uses .context field (not .name).
        let status_info = pr
            .ci_checks
            .code
            .iter()
            .find(|c| c.name == "travis-ci")
            .expect("StatusContext should use .context field as check name");
        assert_eq!(
            status_info.state, "passing",
            "SUCCESS state maps to passing"
        );

        assert_eq!(
            pr.ci_code_state.as_deref(),
            Some("passing"),
            "ci_code_state should be passing when all code checks pass"
        );
        // Legacy checks_state mirrors ci_code_state.
        assert_eq!(
            pr.checks_state.as_deref(),
            Some("passing"),
            "legacy checks_state mirrors ci_code_state"
        );
    }

    // -- Task #10: >100 checks truncated ---------------------------------------

    /// Scenario: PRs with more than 100 checks are truncated and a warning is logged.
    #[test]
    fn parse_prs_graphql_truncates_at_100_contexts() {
        // Build exactly 100 context nodes (what GitHub returns for first:100).
        // Set totalCount to 120 to simulate truncation.
        let context_nodes: Vec<serde_json::Value> = (0..100)
            .map(|i| {
                json!({
                    "__typename": "CheckRun",
                    "name": format!("check-{i}"),
                    "conclusion": "SUCCESS",
                    "status": "COMPLETED"
                })
            })
            .collect();

        let pr_node = json!({
            "number": 42,
            "headRefName": "feat/many-checks",
            "baseRefName": "main",
            "state": "OPEN",
            "reviewDecision": null,
            "mergeable": "MERGEABLE",
            "reviewThreads": {"nodes": []},
            "closingIssuesReferences": {"nodes": []},
            "commits": {"nodes": [{
                "commit": {
                    "statusCheckRollup": {
                        "state": "SUCCESS",
                        "contexts": {
                            "totalCount": 120,
                            "nodes": context_nodes
                        }
                    }
                }
            }]}
        });

        let json = graphql_prs(json!([pr_node]));
        let prs = parse_prs_graphql(&json, &empty_matcher());

        assert_eq!(prs.len(), 1);
        let pr = &prs[0];

        // All 100 returned checks are parsed (not 120 which weren't returned).
        let total_parsed = pr.ci_checks.code.len() + pr.ci_checks.gate.len();
        assert_eq!(
            total_parsed, 100,
            "should parse exactly 100 checks (the first 100 returned by GraphQL)"
        );
        assert_eq!(
            pr.ci_code_state.as_deref(),
            Some("passing"),
            "rollup of 100 passing checks should be passing"
        );
        // The warning is logged internally — we verify behavior (100 entries),
        // not the log call (no test-infra log capture in this module).
    }

    // -- parse_prs_graphql labels extraction --------------------------------

    /// Builds a PR node with an explicit `labels` array of name strings.
    fn gql_pr_node_with_labels(number: u32, label_names: &[&str]) -> serde_json::Value {
        let mut node = gql_pr_node(number, "test/branch", None, "MERGEABLE", None, vec![], 0);
        let label_nodes: Vec<serde_json::Value> =
            label_names.iter().map(|n| json!({"name": n})).collect();
        node["labels"] = json!({ "nodes": label_nodes });
        node
    }

    #[test]
    fn parse_prs_graphql_extracts_labels_when_present() {
        let json = graphql_prs(json!([gql_pr_node_with_labels(
            55,
            &["in-progress", "bug"],
        )]));
        let prs = parse_prs_graphql(&json, &GateMatcher::new(&[]));
        assert_eq!(prs[0].labels, vec!["in-progress", "bug"]);
    }

    #[test]
    fn parse_prs_graphql_labels_empty_when_nodes_missing() {
        // Existing `gql_pr_node` helper emits no `labels` key at all.
        let json = graphql_prs(json!([gql_pr_node(
            55,
            "test/branch",
            None,
            "MERGEABLE",
            None,
            vec![],
            0,
        )]));
        let prs = parse_prs_graphql(&json, &GateMatcher::new(&[]));
        assert!(prs[0].labels.is_empty());
    }

    #[test]
    fn parse_prs_graphql_labels_empty_when_nodes_array_is_empty() {
        let json = graphql_prs(json!([gql_pr_node_with_labels(55, &[])]));
        let prs = parse_prs_graphql(&json, &GateMatcher::new(&[]));
        assert!(prs[0].labels.is_empty());
    }

    #[test]
    fn parse_prs_graphql_skips_label_nodes_without_name() {
        // Defensive: if a node in the labels array is missing a `name` field,
        // skip it rather than panicking or emitting a placeholder.
        let mut node = gql_pr_node(55, "test/branch", None, "MERGEABLE", None, vec![], 0);
        node["labels"] = json!({
            "nodes": [
                {"name": "keep-me"},
                {"other": "no-name-here"},
                {"name": "also-keep"},
            ]
        });
        let json = graphql_prs(json!([node]));
        let prs = parse_prs_graphql(&json, &GateMatcher::new(&[]));
        assert_eq!(prs[0].labels, vec!["keep-me", "also-keep"]);
    }

    // -- parse_worktree_porcelain -------------------------------------------

    #[test]
    fn parse_worktree_porcelain_main_worktree() {
        let input = "worktree /home/user/repo\nHEAD abc123\nbranch refs/heads/main\n";
        let wts = parse_worktree_porcelain(input);
        assert_eq!(wts.len(), 1);
        assert_eq!(wts[0].path, "/home/user/repo");
        assert_eq!(wts[0].branch, "main");
        assert!(!wts[0].is_bare);
        assert!(!wts[0].is_locked);
    }

    #[test]
    fn parse_worktree_porcelain_strips_refs_heads() {
        let input = "worktree /home/user/repo-feat\nHEAD abc\nbranch refs/heads/feat/my-work\n";
        let wts = parse_worktree_porcelain(input);
        assert_eq!(wts[0].branch, "feat/my-work");
    }

    #[test]
    fn parse_worktree_porcelain_bare_worktree() {
        let input = "worktree /home/user/repo.git\nHEAD 000\nbare\n";
        let wts = parse_worktree_porcelain(input);
        assert!(wts[0].is_bare);
    }

    #[test]
    fn parse_worktree_porcelain_locked_worktree() {
        let input =
            "worktree /home/user/repo-locked\nHEAD abc\nbranch refs/heads/main\nlocked reason\n";
        let wts = parse_worktree_porcelain(input);
        assert!(wts[0].is_locked);
    }

    #[test]
    fn parse_worktree_porcelain_multiple_blocks() {
        let input = "worktree /a\nHEAD aaa\nbranch refs/heads/main\n\nworktree /b\nHEAD bbb\nbranch refs/heads/feat\n";
        let wts = parse_worktree_porcelain(input);
        assert_eq!(wts.len(), 2);
        assert_eq!(wts[0].path, "/a");
        assert_eq!(wts[1].path, "/b");
    }

    #[test]
    fn parse_worktree_porcelain_empty_input() {
        assert!(parse_worktree_porcelain("").is_empty());
    }

    // -- parse_tmux_output --------------------------------------------------

    #[test]
    fn parse_tmux_output_local_session() {
        let sessions = "my-session:/home/user/repo\n";
        let result = parse_tmux_output(
            sessions,
            None,
            |name| {
                assert_eq!(name, "my-session");
                "0.0\tzsh:zsh\n0.1\tbash:bash\n".to_string()
            },
            |_| vec![],
        );
        assert_eq!(result.len(), 1);
        assert_eq!(result[0].name, "my-session");
        assert_eq!(result[0].path, "/home/user/repo");
        assert_eq!(result[0].pane_targets, vec!["0.0", "0.1"]);
        assert_eq!(result[0].pane_titles, vec!["zsh", "bash"]);
        assert_eq!(result[0].pane_commands, vec!["zsh", "bash"]);
        assert!(result[0].host.is_none());
    }

    #[test]
    fn parse_tmux_output_remote_session() {
        let sessions = "remote-session:/home/ubuntu/workspace\n";
        let result = parse_tmux_output(
            sessions,
            Some("ubuntu@10.0.0.1"),
            |_| "".to_string(),
            |_| vec![],
        );
        assert_eq!(result[0].host.as_deref(), Some("ubuntu@10.0.0.1"));
    }

    #[test]
    fn parse_tmux_output_empty_input() {
        let result = parse_tmux_output("", None, |_| "".to_string(), |_| vec![]);
        assert!(result.is_empty());
    }

    #[test]
    fn parse_tmux_output_skips_malformed_lines() {
        // Line without colon separator is skipped.
        let sessions = "bad-line-no-colon\ngood-session:/path\n";
        let result = parse_tmux_output(sessions, None, |_| "".to_string(), |_| vec![]);
        assert_eq!(result.len(), 1);
        assert_eq!(result[0].name, "good-session");
    }

    #[test]
    fn parse_tmux_output_multiple_sessions() {
        let sessions = "session-a:/path/a\nsession-b:/path/b\n";
        let result = parse_tmux_output(sessions, None, |_| "".to_string(), |_| vec![]);
        assert_eq!(result.len(), 2);
        assert_eq!(result[0].name, "session-a");
        assert_eq!(result[1].name, "session-b");
    }

    #[test]
    fn parse_tmux_output_captures_last_output_lines() {
        let sessions = "my-session:/path\n";
        let result = parse_tmux_output(
            sessions,
            None,
            |_| "".to_string(),
            |_| vec!["line1".to_string(), "line2".to_string()],
        );
        assert_eq!(result[0].last_output_lines, vec!["line1", "line2"]);
    }

    #[test]
    fn local_and_remote_parse_identically_except_for_host() {
        // AC #5: local and remote fetch paths feed identical raw output shape
        // into parse_tmux_sessions_from_panes; the only per-row difference
        // should be the `host` tag.
        let raw = "repo_main\t1\t/work/repo\t1700000000\t1700001000\n\
                   repo_main\t0\t/inactive\t1700000000\t1700001000\n\
                   feat_sess\t1\t/work/repo-feat\t1700000100\t1700002000\n";

        let local = parse_tmux_sessions_from_panes(raw, None, |_| String::new(), |_| vec![]);
        let remote =
            parse_tmux_sessions_from_panes(raw, Some("user@host"), |_| String::new(), |_| vec![]);

        assert_eq!(local.len(), remote.len());
        for (l, r) in local.iter().zip(remote.iter()) {
            assert_eq!(l.name, r.name);
            assert_eq!(l.path, r.path);
            assert_eq!(l.created_at, r.created_at);
            assert_eq!(l.last_activity_at, r.last_activity_at);
        }
        assert!(local.iter().all(|s| s.host.is_none()));
        assert!(
            remote
                .iter()
                .all(|s| s.host.as_deref() == Some("user@host"))
        );
    }

    #[test]
    fn parse_tmux_output_attaches_no_claude_state_for_local_sessions() {
        let sessions_str = "my-session:/path\n";
        let result = parse_tmux_output(sessions_str, None, |_| "".to_string(), |_| vec![]);
        assert!(result[0].claude_state_raw.is_none());
    }

    // -- parse_pane_lines ---------------------------------------------------

    #[test]
    fn parse_pane_lines_new_format_extracts_window_metadata() {
        let output =
            "0.0\tmain\t1\tbash:bash\n0.1\tmain\t1\tclaude:claude\n1.0\teditor\t0\tnvim:nvim\n";
        let parsed = parse_pane_lines(output);
        assert_eq!(parsed.targets, vec!["0.0", "0.1", "1.0"]);
        assert_eq!(parsed.window_names, vec!["main", "main", "editor"]);
        assert_eq!(parsed.window_active, vec!["1", "1", "0"]);
        assert_eq!(parsed.titles, vec!["bash", "claude", "nvim"]);
        assert_eq!(parsed.commands, vec!["bash", "claude", "nvim"]);
    }

    #[test]
    fn parse_pane_lines_old_format_backward_compat() {
        // Old format: only two tab-separated fields — target and title:command.
        let output = "0.0\tbash:bash\n0.1\tvim:vim\n";
        let parsed = parse_pane_lines(output);
        assert_eq!(parsed.targets, vec!["0.0", "0.1"]);
        // Old format: window_names empty, window_active defaults to "0".
        assert_eq!(parsed.window_names, vec!["", ""]);
        assert_eq!(parsed.window_active, vec!["0", "0"]);
        assert_eq!(parsed.titles, vec!["bash", "vim"]);
        assert_eq!(parsed.commands, vec!["bash", "vim"]);
    }

    #[test]
    fn parse_pane_lines_empty_input() {
        let parsed = parse_pane_lines("");
        assert!(parsed.targets.is_empty());
        assert!(parsed.window_names.is_empty());
        assert!(parsed.window_active.is_empty());
        assert!(parsed.window_layouts.is_empty());
        assert!(parsed.pane_paths.is_empty());
        assert!(parsed.pane_active.is_empty());
        assert!(parsed.titles.is_empty());
        assert!(parsed.commands.is_empty());
    }

    #[test]
    fn parse_pane_lines_extended_format_extracts_layout_path_active() {
        // 7-field format: target, window_name, window_active, window_layout,
        // pane_current_path, pane_active, title:command
        let output = concat!(
            "0.0\tmain\t1\teven-horizontal\t/home/user/proj\t1\tbash:bash\n",
            "0.1\tmain\t1\teven-horizontal\t/home/user/proj\t0\tclaude:claude\n",
            "1.0\teditor\t0\tmain-vertical\t/tmp\t1\tnvim:nvim\n",
        );
        let parsed = parse_pane_lines(output);
        assert_eq!(parsed.targets, vec!["0.0", "0.1", "1.0"]);
        assert_eq!(parsed.window_names, vec!["main", "main", "editor"]);
        assert_eq!(parsed.window_active, vec!["1", "1", "0"]);
        assert_eq!(
            parsed.window_layouts,
            vec!["even-horizontal", "even-horizontal", "main-vertical"]
        );
        assert_eq!(
            parsed.pane_paths,
            vec!["/home/user/proj", "/home/user/proj", "/tmp"]
        );
        assert_eq!(parsed.pane_active, vec!["1", "0", "1"]);
        assert_eq!(parsed.titles, vec!["bash", "claude", "nvim"]);
        assert_eq!(parsed.commands, vec!["bash", "claude", "nvim"]);
    }

    #[test]
    fn parse_pane_lines_legacy_four_field_format_fills_defaults() {
        // Legacy #184 4-field format: new fields default to empty/"0".
        let output = "0.0\tmain\t1\tbash:bash\n0.1\tmain\t1\tclaude:claude\n";
        let parsed = parse_pane_lines(output);
        assert_eq!(parsed.targets, vec!["0.0", "0.1"]);
        assert_eq!(parsed.window_names, vec!["main", "main"]);
        assert_eq!(parsed.window_active, vec!["1", "1"]);
        // New fields default to empty string for layout/path, "0" for pane_active.
        assert_eq!(parsed.window_layouts, vec!["", ""]);
        assert_eq!(parsed.pane_paths, vec!["", ""]);
        assert_eq!(parsed.pane_active, vec!["0", "0"]);
        assert_eq!(parsed.titles, vec!["bash", "claude"]);
        assert_eq!(parsed.commands, vec!["bash", "claude"]);
    }

    #[test]
    fn parse_pane_lines_title_with_colon_new_format() {
        let output = "0.0\tmain\t1\tClaude: my-project:node\n";
        let parsed = parse_pane_lines(output);
        assert_eq!(parsed.targets, vec!["0.0"]);
        assert_eq!(parsed.window_names, vec!["main"]);
        assert_eq!(parsed.window_active, vec!["1"]);
        assert_eq!(parsed.titles, vec!["Claude"]);
        assert_eq!(parsed.commands, vec![" my-project:node"]);
    }

    // -- issue_graphql_query ---------------------------------------------------

    #[test]
    fn issue_graphql_query_empty_returns_stub() {
        let q = issue_graphql_query("acme", "myrepo", &[]);
        assert!(q.contains("repository(owner: \"acme\", name: \"myrepo\")"));
        assert!(!q.contains("i_"), "empty query must have no issue aliases");
    }

    #[test]
    fn issue_graphql_query_generates_aliases_and_fragment() {
        let q = issue_graphql_query("acme", "myrepo", &[42, 99]);

        // Each issue gets an alias.
        assert!(q.contains("i_42: issue(number: 42)"), "missing i_42 alias");
        assert!(q.contains("i_99: issue(number: 99)"), "missing i_99 alias");

        // Fragment is defined and referenced.
        assert!(
            q.contains("...IssueFields"),
            "aliases must use ...IssueFields spread"
        );
        assert!(
            q.contains("fragment IssueFields on Issue"),
            "IssueFields fragment must be defined"
        );

        // Fragment contains required fields.
        assert!(
            q.contains("subIssues(first: 50)"),
            "fragment must select subIssues"
        );
        assert!(
            q.contains("parent { number }"),
            "fragment must select parent.number"
        );
        assert!(
            q.contains("assignees(first: 20)"),
            "fragment must select assignees"
        );
        assert!(q.contains("createdAt"), "fragment must select createdAt");
        assert!(q.contains("body"), "fragment must select body");
        assert!(
            q.contains("labels(first: 100)"),
            "fragment must select labels(first: 100)"
        );
    }

    #[test]
    fn issue_graphql_query_single_issue() {
        let q = issue_graphql_query("owner", "repo", &[1]);
        assert!(q.contains("i_1: issue(number: 1)"));
    }

    // -- extract_blocked_by (blocking regex) ------------------------------------

    #[test]
    fn blocking_regex_matches_blocked_by() {
        let result = extract_blocked_by("This is blocked by #42");
        assert_eq!(result, vec![42]);
    }

    #[test]
    fn blocking_regex_matches_depends_on() {
        let result = extract_blocked_by("Depends on #99");
        assert_eq!(result, vec![99]);
    }

    #[test]
    fn blocking_regex_matches_waiting_on() {
        let result = extract_blocked_by("waiting on #7");
        assert_eq!(result, vec![7]);
    }

    #[test]
    fn blocking_regex_case_insensitive() {
        let result = extract_blocked_by("BLOCKED BY #100 and Depends On #200");
        assert!(result.contains(&100), "should match BLOCKED BY #100");
        assert!(result.contains(&200), "should match Depends On #200");
    }

    #[test]
    fn blocking_regex_does_not_match_fixes() {
        let result = extract_blocked_by("fixes #42");
        assert!(result.is_empty(), "should not match 'fixes #N'");
    }

    #[test]
    fn blocking_regex_does_not_match_closes() {
        let result = extract_blocked_by("closes #42");
        assert!(result.is_empty(), "should not match 'closes #N'");
    }

    #[test]
    fn blocking_regex_does_not_match_bare_hash() {
        let result = extract_blocked_by("see #42 for context");
        assert!(result.is_empty(), "should not match bare '#N'");
    }

    #[test]
    fn blocking_regex_multiple_blockers() {
        let result = extract_blocked_by("blocked by #10\ndepends on #20\nwaiting on #30");
        assert_eq!(result.len(), 3);
        assert!(result.contains(&10));
        assert!(result.contains(&20));
        assert!(result.contains(&30));
    }

    // -- parse_issues_graphql --------------------------------------------------

    fn graphql_issues_response(aliases: serde_json::Value) -> String {
        serde_json::to_string(&json!({
            "data": {
                "repository": aliases
            }
        }))
        .unwrap()
    }

    #[test]
    fn parse_issues_graphql_basic_fields() {
        let json = graphql_issues_response(json!({
            "i_42": {
                "number": 42,
                "title": "Fix the thing",
                "state": "OPEN",
                "labels": { "nodes": [{"name": "bug"}] },
                "assignees": { "nodes": [{"login": "alice"}] },
                "createdAt": "2026-01-15T10:00:00Z",
                "body": "",
                "subIssues": { "nodes": [] },
                "parent": null
            }
        }));

        let issues = parse_issues_graphql(&json);
        assert_eq!(issues.len(), 1);
        let issue = &issues[0];
        assert_eq!(issue.number, 42);
        assert_eq!(issue.title, "Fix the thing");
        assert_eq!(issue.state, "open", "state should be lowercased");
        assert_eq!(issue.labels, vec!["bug"]);
        assert_eq!(issue.assignees, vec!["alice"]);
        assert_eq!(issue.created_at.as_deref(), Some("2026-01-15T10:00:00Z"));
        assert!(issue.blocked_by.is_empty());
        assert!(issue.sub_issues.is_empty());
        assert!(issue.parent.is_none());
    }

    #[test]
    fn parse_issues_graphql_closed_state_normalized() {
        let json = graphql_issues_response(json!({
            "i_10": {
                "number": 10,
                "title": "Old issue",
                "state": "CLOSED",
                "labels": { "nodes": [] },
                "assignees": { "nodes": [] },
                "createdAt": null,
                "body": "",
                "subIssues": { "nodes": [] },
                "parent": null
            }
        }));

        let issues = parse_issues_graphql(&json);
        assert_eq!(issues[0].state, "closed");
    }

    #[test]
    fn parse_issues_graphql_sub_issues_and_parent() {
        let json = graphql_issues_response(json!({
            "i_5": {
                "number": 5,
                "title": "Parent issue",
                "state": "OPEN",
                "labels": { "nodes": [] },
                "assignees": { "nodes": [] },
                "createdAt": null,
                "body": "",
                "subIssues": {
                    "nodes": [
                        {"number": 6, "title": "Child one", "state": "OPEN"},
                        {"number": 7, "title": "Child two", "state": "CLOSED"}
                    ]
                },
                "parent": null
            },
            "i_6": {
                "number": 6,
                "title": "Child one",
                "state": "OPEN",
                "labels": { "nodes": [] },
                "assignees": { "nodes": [] },
                "createdAt": null,
                "body": "",
                "subIssues": { "nodes": [] },
                "parent": {"number": 5}
            }
        }));

        let issues = parse_issues_graphql(&json);
        let parent = issues.iter().find(|i| i.number == 5).unwrap();
        assert_eq!(parent.sub_issues.len(), 2);
        assert_eq!(parent.sub_issues[0].number, 6);
        assert_eq!(parent.sub_issues[0].title, "Child one");
        assert_eq!(parent.sub_issues[0].state, "open");
        assert_eq!(parent.sub_issues[1].number, 7);
        assert_eq!(parent.sub_issues[1].state, "closed");
        assert!(parent.parent.is_none());

        let child = issues.iter().find(|i| i.number == 6).unwrap();
        assert_eq!(child.parent, Some(5));
        assert!(child.sub_issues.is_empty());
    }

    #[test]
    fn parse_issues_graphql_blocking_references_from_body() {
        let json = graphql_issues_response(json!({
            "i_20": {
                "number": 20,
                "title": "Blocked issue",
                "state": "OPEN",
                "labels": { "nodes": [] },
                "assignees": { "nodes": [] },
                "createdAt": null,
                "body": "This issue is blocked by #18 and depends on #19",
                "subIssues": { "nodes": [] },
                "parent": null
            }
        }));

        let issues = parse_issues_graphql(&json);
        assert_eq!(issues.len(), 1);
        let issue = &issues[0];
        assert!(issue.blocked_by.contains(&18), "should contain 18");
        assert!(issue.blocked_by.contains(&19), "should contain 19");
    }

    #[test]
    fn parse_issues_graphql_skips_non_i_prefixed_keys() {
        // 'name' key from the stub query must not be parsed as an issue.
        let json = graphql_issues_response(json!({
            "name": "myrepo",
            "i_1": {
                "number": 1,
                "title": "Real issue",
                "state": "OPEN",
                "labels": { "nodes": [] },
                "assignees": { "nodes": [] },
                "createdAt": null,
                "body": "",
                "subIssues": { "nodes": [] },
                "parent": null
            }
        }));

        let issues = parse_issues_graphql(&json);
        assert_eq!(issues.len(), 1);
        assert_eq!(issues[0].number, 1);
    }

    #[test]
    fn parse_issues_graphql_null_issue_skipped() {
        // GitHub returns null for an issue alias if the issue doesn't exist.
        let json = graphql_issues_response(json!({
            "i_999": null,
            "i_1": {
                "number": 1,
                "title": "Exists",
                "state": "OPEN",
                "labels": { "nodes": [] },
                "assignees": { "nodes": [] },
                "createdAt": null,
                "body": "",
                "subIssues": { "nodes": [] },
                "parent": null
            }
        }));

        let issues = parse_issues_graphql(&json);
        assert_eq!(issues.len(), 1, "null issue alias must be skipped");
        assert_eq!(issues[0].number, 1);
    }

    #[test]
    fn parse_issues_graphql_invalid_json_returns_empty() {
        let issues = parse_issues_graphql("not json");
        assert!(issues.is_empty());
    }

    #[test]
    fn parse_issues_graphql_multiple_assignees() {
        let json = graphql_issues_response(json!({
            "i_3": {
                "number": 3,
                "title": "Team issue",
                "state": "OPEN",
                "labels": { "nodes": [] },
                "assignees": { "nodes": [{"login": "alice"}, {"login": "bob"}] },
                "createdAt": null,
                "body": "",
                "subIssues": { "nodes": [] },
                "parent": null
            }
        }));

        let issues = parse_issues_graphql(&json);
        assert_eq!(issues[0].assignees, vec!["alice", "bob"]);
    }

    // -- parse_git_ahead_behind -----------------------------------------------

    #[test]
    fn ahead_behind_branch_with_both() {
        let output = "  issue42/fix-bug     def5678 [origin/issue42/fix-bug: ahead 3, behind 1] Some commit\n";
        let map = parse_git_ahead_behind(output);
        assert_eq!(map.get("issue42/fix-bug"), Some(&(3, 1)));
    }

    #[test]
    fn ahead_behind_branch_ahead_only() {
        let output =
            "  feat/new-thing      ghi9012 [origin/feat/new-thing: ahead 2] Another commit\n";
        let map = parse_git_ahead_behind(output);
        assert_eq!(map.get("feat/new-thing"), Some(&(2, 0)));
    }

    #[test]
    fn ahead_behind_branch_behind_only() {
        let output = "  stale/branch        abc1234 [origin/stale/branch: behind 5] Old commit\n";
        let map = parse_git_ahead_behind(output);
        assert_eq!(map.get("stale/branch"), Some(&(0, 5)));
    }

    #[test]
    fn ahead_behind_branch_no_divergence() {
        // Tracking remote but no ahead/behind counts — branch is in sync.
        let output = "* main                abc1234 [origin/main] Latest commit\n";
        let map = parse_git_ahead_behind(output);
        assert_eq!(map.get("main"), Some(&(0, 0)));
    }

    #[test]
    fn ahead_behind_branch_no_tracking() {
        // No bracket section — untracked local branch.
        let output = "  local-only          abc1234 Untracked commit\n";
        let map = parse_git_ahead_behind(output);
        assert_eq!(map.get("local-only"), Some(&(0, 0)));
    }

    #[test]
    fn ahead_behind_multiple_branches() {
        let output = "\
* main                abc1234 [origin/main] Latest commit
  issue42/fix-bug     def5678 [origin/issue42/fix-bug: ahead 3, behind 1] Some commit
  feat/new-thing      ghi9012 [origin/feat/new-thing: ahead 2] Another commit
";
        let map = parse_git_ahead_behind(output);
        assert_eq!(map.len(), 3);
        assert_eq!(map.get("main"), Some(&(0, 0)));
        assert_eq!(map.get("issue42/fix-bug"), Some(&(3, 1)));
        assert_eq!(map.get("feat/new-thing"), Some(&(2, 0)));
    }

    #[test]
    fn ahead_behind_empty_output() {
        let map = parse_git_ahead_behind("");
        assert!(map.is_empty());
    }

    // -- parse_git_last_commit_dates ------------------------------------------

    #[test]
    fn commit_dates_multiple_branches() {
        let output = "\
main 2026-04-10T10:00:00-07:00
issue42/fix-bug 2026-04-12T14:30:00-07:00
";
        let map = parse_git_last_commit_dates(output);
        assert_eq!(map.len(), 2);
        assert_eq!(
            map.get("main"),
            Some(&"2026-04-10T10:00:00-07:00".to_string())
        );
        assert_eq!(
            map.get("issue42/fix-bug"),
            Some(&"2026-04-12T14:30:00-07:00".to_string())
        );
    }

    #[test]
    fn commit_dates_empty_output() {
        let map = parse_git_last_commit_dates("");
        assert!(map.is_empty());
    }

    #[test]
    fn commit_dates_single_branch() {
        let output = "feat/my-feature 2026-03-01T09:00:00+00:00\n";
        let map = parse_git_last_commit_dates(output);
        assert_eq!(
            map.get("feat/my-feature"),
            Some(&"2026-03-01T09:00:00+00:00".to_string())
        );
    }

    // -- parse_tmux_output with session timestamps ----------------------------

    #[test]
    fn parse_tmux_output_with_session_timestamps() {
        let sessions = "my-session:/home/user/repo|1700000000|1700001000\n";
        let result = parse_tmux_output(sessions, None, |_| "".to_string(), |_| vec![]);
        assert_eq!(result.len(), 1);
        assert_eq!(result[0].name, "my-session");
        assert_eq!(result[0].path, "/home/user/repo");
        assert_eq!(result[0].created_at, Some(1700000000u64));
        assert_eq!(result[0].last_activity_at, Some(1700001000u64));
    }

    #[test]
    fn parse_tmux_output_without_timestamps_defaults_to_none() {
        // Old format without timestamps — backward-compatible.
        let sessions = "my-session:/home/user/repo\n";
        let result = parse_tmux_output(sessions, None, |_| "".to_string(), |_| vec![]);
        assert_eq!(result.len(), 1);
        assert_eq!(result[0].created_at, None);
        assert_eq!(result[0].last_activity_at, None);
    }

    #[test]
    fn parse_tmux_output_with_only_created_timestamp() {
        let sessions = "my-session:/home/user/repo|1700000000\n";
        let result = parse_tmux_output(sessions, None, |_| "".to_string(), |_| vec![]);
        assert_eq!(result.len(), 1);
        assert_eq!(result[0].created_at, Some(1700000000u64));
        assert_eq!(result[0].last_activity_at, None);
    }

    // -- parse_repo_meta_from_pr_response -------------------------------------

    #[test]
    fn parse_repo_meta_extracts_default_branch_and_ci_state() {
        let json = serde_json::to_string(&json!({
            "data": {
                "repository": {
                    "defaultBranchRef": {
                        "name": "main",
                        "target": {
                            "statusCheckRollup": {
                                "state": "SUCCESS"
                            }
                        }
                    },
                    "b_feat_branch": {
                        "nodes": []
                    }
                }
            }
        }))
        .unwrap();

        let meta = parse_repo_meta_from_pr_response(&json);
        assert_eq!(meta.default_branch.as_deref(), Some("main"));
        assert_eq!(meta.main_ci_state.as_deref(), Some("SUCCESS"));
    }

    #[test]
    fn parse_repo_meta_null_default_branch_returns_default() {
        let json = serde_json::to_string(&json!({
            "data": {
                "repository": {
                    "defaultBranchRef": null
                }
            }
        }))
        .unwrap();

        let meta = parse_repo_meta_from_pr_response(&json);
        assert!(meta.default_branch.is_none());
        assert!(meta.main_ci_state.is_none());
    }

    #[test]
    fn parse_repo_meta_missing_ci_state_returns_none() {
        let json = serde_json::to_string(&json!({
            "data": {
                "repository": {
                    "defaultBranchRef": {
                        "name": "main",
                        "target": {}
                    }
                }
            }
        }))
        .unwrap();

        let meta = parse_repo_meta_from_pr_response(&json);
        assert_eq!(meta.default_branch.as_deref(), Some("main"));
        assert!(meta.main_ci_state.is_none());
    }

    #[test]
    fn parse_repo_meta_invalid_json_returns_default() {
        let meta = parse_repo_meta_from_pr_response("not json");
        assert!(meta.default_branch.is_none());
        assert!(meta.main_ci_state.is_none());
    }

    // -- pr_graphql_query_per_branch includes defaultBranchRef ----------------

    #[test]
    fn per_branch_query_includes_default_branch_ref() {
        let query = pr_graphql_query_per_branch("acme", "my-project", &["feat/branch".to_string()]);
        assert!(
            query.contains("defaultBranchRef"),
            "query must select defaultBranchRef"
        );
        assert!(
            query.contains("statusCheckRollup"),
            "query must select statusCheckRollup on defaultBranchRef target"
        );
    }

    #[test]
    fn per_branch_parser_populates_details_url_on_check_info() {
        let json = r#"{
            "data": {
                "repository": {
                    "defaultBranchRef": null,
                    "b_feat_branch": {
                        "nodes": [{
                            "number": 1,
                            "headRefName": "feat/branch",
                            "baseRefName": "main",
                            "title": "Test",
                            "state": "OPEN",
                            "isDraft": false,
                            "author": { "login": "dev" },
                            "reviewDecision": null,
                            "mergeable": "MERGEABLE",
                            "labels": { "nodes": [] },
                            "reviewRequests": { "nodes": [] },
                            "reviews": { "nodes": [] },
                            "reviewThreads": { "nodes": [] },
                            "closingIssuesReferences": { "nodes": [] },
                            "additions": 10,
                            "deletions": 5,
                            "createdAt": "2026-01-01T00:00:00Z",
                            "updatedAt": "2026-01-02T00:00:00Z",
                            "commits": {
                                "nodes": [{
                                    "commit": {
                                        "pushedDate": "2026-01-01T12:00:00Z",
                                        "statusCheckRollup": {
                                            "state": "SUCCESS",
                                            "contexts": {
                                                "totalCount": 1,
                                                "nodes": [{
                                                    "__typename": "CheckRun",
                                                    "name": "test-unit",
                                                    "conclusion": "SUCCESS",
                                                    "status": "COMPLETED",
                                                    "detailsUrl": "https://github.com/owner/repo/actions/runs/123"
                                                }]
                                            }
                                        }
                                    }
                                }]
                            }
                        }]
                    }
                }
            }
        }"#;

        let matcher = GateMatcher::new(&[]);
        let prs = parse_prs_graphql_per_branch(json, &matcher);
        assert_eq!(prs.len(), 1);
        assert_eq!(prs[0].ci_checks.code.len(), 1);
        assert_eq!(
            prs[0].ci_checks.code[0].details_url.as_deref(),
            Some("https://github.com/owner/repo/actions/runs/123")
        );
    }

    // -- parse_tmux_output new cached fields (Task #190-2) --------------------

    #[test]
    fn parse_tmux_output_with_extended_panes_populates_new_cached_fields() {
        // 7-field tab-delimited pane line:
        // {window}.{pane}\t{window_name}\t{window_active}\t{window_layout}\t{pane_path}\t{pane_active}\t{title}:{command}
        let layout = "bb62,80x24,0,0{40x24,0,0,1,39x24,41,0,2}";
        let pane_line = format!("0.0\tmain\t1\t{layout}\t/home/user/repo\t1\tbash:bash");
        let sessions = "my-session:/home/user/repo\n";

        let result = parse_tmux_output(sessions, None, |_| pane_line.clone(), |_| vec![]);

        assert_eq!(result.len(), 1);
        let session = &result[0];

        assert_eq!(session.window_layouts, vec![layout]);
        assert_eq!(session.pane_paths, vec!["/home/user/repo"]);
        assert_eq!(session.pane_active, vec!["1"]);
    }

    // -- unresolved_threads counting: isResolved gates it, isOutdated does not (#252, #607) --

    /// Builds a per-branch GraphQL response envelope with the given thread nodes.
    fn per_branch_prs_with_threads(threads: serde_json::Value) -> String {
        serde_json::to_string(&json!({
            "data": {
                "repository": {
                    "b_feat_branch": {
                        "nodes": [{
                            "number": 1,
                            "headRefName": "feat/branch",
                            "baseRefName": "main",
                            "title": "Test",
                            "state": "OPEN",
                            "isDraft": false,
                            "author": { "login": "dev" },
                            "reviewDecision": null,
                            "mergeable": "MERGEABLE",
                            "labels": { "nodes": [] },
                            "reviewRequests": { "nodes": [] },
                            "reviews": { "nodes": [] },
                            "reviewThreads": { "nodes": threads },
                            "closingIssuesReferences": { "nodes": [] },
                            "additions": 0,
                            "deletions": 0,
                            "createdAt": "2026-01-01T00:00:00Z",
                            "updatedAt": "2026-01-02T00:00:00Z",
                            "commits": { "nodes": [{ "commit": { "pushedDate": null, "statusCheckRollup": null } }] }
                        }]
                    }
                }
            }
        }))
        .unwrap()
    }

    #[test]
    fn unresolved_threads_counts_unresolved_regardless_of_outdated() {
        // GitHub's merge gate blocks on isResolved alone (#607) — the
        // outdated-but-unresolved thread below counts alongside the two
        // ordinary unresolved ones.
        let threads = json!([
            {"isResolved": false, "isOutdated": false},
            {"isResolved": false, "isOutdated": false},
            {"isResolved": true,  "isOutdated": false},
            {"isResolved": false, "isOutdated": true},
            {"isResolved": true,  "isOutdated": true},
        ]);

        // parse_prs_graphql path
        let json = graphql_prs(json!([{
            "number": 1,
            "headRefName": "feat/branch",
            "state": "OPEN",
            "reviewDecision": null,
            "mergeable": "MERGEABLE",
            "reviewThreads": { "nodes": threads.clone() },
            "closingIssuesReferences": { "nodes": [] },
            "commits": { "nodes": [{ "commit": { "statusCheckRollup": null } }] }
        }]));
        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs[0].unresolved_threads, 3);

        // parse_prs_graphql_per_branch path
        let per_branch_json = per_branch_prs_with_threads(threads);
        let prs_pb = parse_prs_graphql_per_branch(&per_branch_json, &empty_matcher());
        assert_eq!(prs_pb[0].unresolved_threads, 3);
    }

    #[test]
    fn unresolved_threads_zero_when_all_resolved() {
        // Resolved excludes a thread regardless of its outdated state; an
        // outdated-but-UNresolved thread is a different fixture (see
        // `unresolved_threads_counts_outdated_unresolved_as_blocking`).
        let threads = json!([
            {"isResolved": true, "isOutdated": false},
            {"isResolved": true, "isOutdated": true},
            {"isResolved": true, "isOutdated": true},
        ]);

        let json = graphql_prs(json!([{
            "number": 1,
            "headRefName": "feat/branch",
            "state": "OPEN",
            "reviewDecision": null,
            "mergeable": "MERGEABLE",
            "reviewThreads": { "nodes": threads.clone() },
            "closingIssuesReferences": { "nodes": [] },
            "commits": { "nodes": [{ "commit": { "statusCheckRollup": null } }] }
        }]));
        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs[0].unresolved_threads, 0);

        let per_branch_json = per_branch_prs_with_threads(threads);
        let prs_pb = parse_prs_graphql_per_branch(&per_branch_json, &empty_matcher());
        assert_eq!(prs_pb[0].unresolved_threads, 0);
    }

    #[test]
    fn unresolved_threads_counts_outdated_unresolved_as_blocking() {
        // PR #764 review finding: an outdated-but-unresolved thread must still
        // count. GitHub's "Require conversation resolution before merging" gate
        // blocks on isResolved alone (#607) — isOutdated has no bearing on it.
        let threads = json!([
            {"isResolved": true,  "isOutdated": false}, // resolved -> excluded
            {"isResolved": false, "isOutdated": false}, // unresolved -> counted
            {"isResolved": false, "isOutdated": true},  // outdated, unresolved -> counted
            {"isResolved": true,  "isOutdated": true},  // outdated, resolved -> excluded
        ]);

        let json = graphql_prs(json!([{
            "number": 1,
            "headRefName": "feat/branch",
            "state": "OPEN",
            "reviewDecision": null,
            "mergeable": "MERGEABLE",
            "reviewThreads": { "nodes": threads.clone() },
            "closingIssuesReferences": { "nodes": [] },
            "commits": { "nodes": [{ "commit": { "statusCheckRollup": null } }] }
        }]));
        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs[0].unresolved_threads, 2);

        let per_branch_json = per_branch_prs_with_threads(threads);
        let prs_pb = parse_prs_graphql_per_branch(&per_branch_json, &empty_matcher());
        assert_eq!(prs_pb[0].unresolved_threads, 2);
    }

    #[test]
    fn parser_counts_thread_missing_is_outdated_as_unresolved() {
        // Thread with only isResolved (no isOutdated key at all) — isOutdated is
        // never read by count_actionable_threads, so its absence cannot matter.
        let threads = json!([{"isResolved": false}]);

        let json = graphql_prs(json!([{
            "number": 1,
            "headRefName": "feat/branch",
            "state": "OPEN",
            "reviewDecision": null,
            "mergeable": "MERGEABLE",
            "reviewThreads": { "nodes": threads.clone() },
            "closingIssuesReferences": { "nodes": [] },
            "commits": { "nodes": [{ "commit": { "statusCheckRollup": null } }] }
        }]));
        let prs = parse_prs_graphql(&json, &empty_matcher());
        assert_eq!(prs[0].unresolved_threads, 1);

        let per_branch_json = per_branch_prs_with_threads(threads);
        let prs_pb = parse_prs_graphql_per_branch(&per_branch_json, &empty_matcher());
        assert_eq!(prs_pb[0].unresolved_threads, 1);
    }

    #[test]
    fn pr_graphql_query_includes_is_outdated_in_review_threads() {
        let q = pr_graphql_query("owner", "repo");
        // Strip all whitespace to be robust against formatting changes.
        let compact: String = q.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(
            compact.contains("isResolved"),
            "expected reviewThreads to select isResolved, got: {q}"
        );
        assert!(
            compact.contains("isOutdated"),
            "expected reviewThreads to select isOutdated, got: {q}"
        );
    }

    // -- parse_tmux_sessions_from_panes (issue #275) --------------------------

    /// Builds a panes-format line: "{name}\t{pane_active}\t{cwd}\t{created}\t{activity}".
    fn pane_line(name: &str, pane_active: &str, cwd: &str, created: u64, activity: u64) -> String {
        format!("{name}\t{pane_active}\t{cwd}\t{created}\t{activity}")
    }

    #[test]
    fn parse_tmux_output_new_format_uses_active_pane_cwd() {
        // Single session, one active pane — path must come from pane_current_path,
        // not a frozen session_path.
        let created = 1_700_000_000u64;
        let activity = 1_700_001_000u64;
        let input = pane_line("myrepo", "1", "/work/repo-feat", created, activity);

        let sessions = parse_tmux_sessions_from_panes(&input, None, |_| String::new(), |_| vec![]);

        assert_eq!(sessions.len(), 1);
        let s = &sessions[0];
        assert_eq!(s.name, "myrepo");
        assert_eq!(s.path, "/work/repo-feat", "path must be pane_current_path");
        assert_eq!(s.created_at, Some(created));
        assert_eq!(s.last_activity_at, Some(activity));
        assert!(s.host.is_none());
    }

    #[test]
    fn parse_tmux_output_new_format_skips_inactive_panes() {
        // Two panes for the same session: inactive (cwd=/work/repo) then active (cwd=/work/repo-feat).
        // The session path must reflect the ACTIVE pane only.
        let input = format!(
            "{}\n{}",
            pane_line("myrepo", "0", "/work/repo", 1_700_000_000, 1_700_001_000),
            pane_line(
                "myrepo",
                "1",
                "/work/repo-feat",
                1_700_000_000,
                1_700_001_000
            ),
        );

        let sessions = parse_tmux_sessions_from_panes(&input, None, |_| String::new(), |_| vec![]);

        assert_eq!(sessions.len(), 1, "should deduplicate to one session");
        assert_eq!(
            sessions[0].path, "/work/repo-feat",
            "path must be the active pane cwd, not the inactive pane cwd"
        );
    }

    #[test]
    fn pr_graphql_query_per_branch_includes_is_outdated() {
        let q = pr_graphql_query_per_branch("owner", "repo", &["branch-1".to_string()]);
        let compact: String = q.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(
            compact.contains("isOutdated"),
            "expected PrFields fragment to select isOutdated, got: {q}"
        );
        assert!(
            compact.contains("isResolved"),
            "expected PrFields fragment to select isResolved, got: {q}"
        );
    }

    #[test]
    fn pr_graphql_query_per_branch_uses_last_20_reviews() {
        let q = pr_graphql_query_per_branch("owner", "repo", &["branch-1".to_string()]);
        let compact: String = q.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(
            compact.contains("reviews(last:20)"),
            "expected reviews to use last:20, got: {q}"
        );
    }

    // -- AC #5: per-thread latest-comment timestamps in both GraphQL queries ---

    /// Both `pr_graphql_query` and `pr_graphql_query_per_branch` must select
    /// `comments(last: 1) { nodes { createdAt } }` on each reviewThread node.
    /// Only `createdAt` is selected — `body` and `author` must NOT appear inside
    /// the comments selection (minimal cost selection per AC #5).
    #[test]
    fn pr_graphql_query_review_threads_select_per_thread_comment_timestamps() {
        let q = pr_graphql_query("owner", "repo");
        let compact: String = q.chars().filter(|c| !c.is_whitespace()).collect();

        assert!(
            compact.contains("comments(last:1){nodes{createdAt}}"),
            "pr_graphql_query: reviewThreads must select comments(last:1){{nodes{{createdAt}}}}, got: {q}"
        );
        // Minimal selection — no body or author inside the comments subselection.
        // We check that these strings don't appear as fields inside `comments{nodes{...}}`.
        // The compact form would contain them if they were in the fragment, so this is
        // conservative but sufficient for the constraint.
        assert!(
            !compact.contains("comments(last:1){nodes{createdAtbody"),
            "pr_graphql_query: comments must not select 'body', got: {q}"
        );
        assert!(
            !compact.contains("comments(last:1){nodes{createdAtauthor"),
            "pr_graphql_query: comments must not select 'author', got: {q}"
        );
    }

    #[test]
    fn pr_graphql_query_per_branch_review_threads_select_per_thread_comment_timestamps() {
        let q = pr_graphql_query_per_branch("owner", "repo", &["branch-1".to_string()]);
        let compact: String = q.chars().filter(|c| !c.is_whitespace()).collect();

        assert!(
            compact.contains("comments(last:1){nodes{createdAt}}"),
            "pr_graphql_query_per_branch: PrFields fragment must select comments(last:1){{nodes{{createdAt}}}}, got: {q}"
        );
        assert!(
            !compact.contains("comments(last:1){nodes{createdAtbody"),
            "pr_graphql_query_per_branch: comments must not select 'body', got: {q}"
        );
        assert!(
            !compact.contains("comments(last:1){nodes{createdAtauthor"),
            "pr_graphql_query_per_branch: comments must not select 'author', got: {q}"
        );
    }

    // -- AC #5: parser collects only unresolved-and-not-outdated timestamps ----

    /// Given a fixture with mixed resolved/outdated/unresolved threads with per-thread
    /// `comments(last:1)` timestamps, the parser returns only timestamps for threads
    /// where `isResolved=false AND isOutdated!=true`.
    #[test]
    fn parse_prs_graphql_collects_thread_timestamps_for_unresolved_not_outdated_only() {
        // Thread fixture:
        //   T1: unresolved, not outdated, latest comment 2026-04-15T10:00:00Z  → included
        //   T2: unresolved, not outdated, latest comment 2026-04-18T09:30:00Z  → included (max)
        //   T3: resolved,   not outdated, latest comment 2026-04-19T11:00:00Z  → excluded (resolved)
        //   T4: unresolved, outdated,     latest comment 2026-04-19T12:00:00Z  → excluded (outdated)
        let threads = json!([
            { "isResolved": false, "isOutdated": false, "comments": { "nodes": [{ "createdAt": "2026-04-15T10:00:00Z" }] } },
            { "isResolved": false, "isOutdated": false, "comments": { "nodes": [{ "createdAt": "2026-04-18T09:30:00Z" }] } },
            { "isResolved": true,  "isOutdated": false, "comments": { "nodes": [{ "createdAt": "2026-04-19T11:00:00Z" }] } },
            { "isResolved": false, "isOutdated": true,  "comments": { "nodes": [{ "createdAt": "2026-04-19T12:00:00Z" }] } },
        ]);

        let mut timestamps = collect_thread_comment_timestamps(&threads);
        timestamps.sort_unstable();

        // Epoch for 2026-04-15T10:00:00Z
        let ts1 = ::chrono::DateTime::parse_from_rfc3339("2026-04-15T10:00:00Z")
            .unwrap()
            .timestamp();
        // Epoch for 2026-04-18T09:30:00Z
        let ts2 = ::chrono::DateTime::parse_from_rfc3339("2026-04-18T09:30:00Z")
            .unwrap()
            .timestamp();

        assert_eq!(
            timestamps,
            vec![ts1, ts2],
            "should only include timestamps for isResolved=false AND isOutdated!=true threads"
        );
        // Sanity: ts2 is the max — this is the value `since_epoch` will use.
        assert!(ts2 > ts1);
    }

    #[test]
    fn parse_prs_graphql_thread_timestamps_skips_threads_without_comments() {
        // Thread with no comments array should contribute no timestamp, not panic.
        let threads = json!([
            { "isResolved": false, "isOutdated": false, "comments": { "nodes": [] } },
            { "isResolved": false, "isOutdated": false },
        ]);
        let timestamps = collect_thread_comment_timestamps(&threads);
        assert!(
            timestamps.is_empty(),
            "threads with empty or missing comments should produce no timestamps"
        );
    }

    // -- AC #5: old CachedPr JSON (missing new field) deserializes cleanly -----

    /// Deserializing a `CachedPr` JSON written by a previous orchard version
    /// (which lacks the `unresolved_thread_comment_timestamps` field) must succeed
    /// and produce an empty vec for that field (Serde `default`).
    #[test]
    fn cached_pr_old_json_without_thread_timestamps_deserializes_cleanly() {
        // Minimal old-shape CachedPr JSON — all fields that existed before this
        // change. The new `unresolved_thread_comment_timestamps` field is absent.
        let old_json = r#"{
            "number": 42,
            "branch": "fix/old-cache",
            "linked_issue": null,
            "state": "open",
            "review_decision": null,
            "has_conflicts": false,
            "unresolved_threads": 2
        }"#;

        let pr: crate::cache::CachedPr = serde_json::from_str(old_json)
            .expect("old CachedPr JSON must deserialize without errors");

        assert_eq!(pr.number, 42);
        assert_eq!(pr.unresolved_threads, 2);
        assert!(
            pr.unresolved_thread_comment_timestamps.is_empty(),
            "missing field must default to empty vec, not error"
        );
    }

    #[test]
    fn parse_tmux_output_new_format_multiple_sessions() {
        // Three sessions each with one active pane at a distinct cwd.
        let input = format!(
            "{}\n{}\n{}",
            pane_line("alpha", "1", "/work/alpha", 1_700_000_001, 1_700_001_001),
            pane_line("beta", "1", "/work/beta", 1_700_000_002, 1_700_001_002),
            pane_line("gamma", "1", "/tmp/scratch", 1_700_000_003, 1_700_001_003),
        );

        let sessions = parse_tmux_sessions_from_panes(&input, None, |_| String::new(), |_| vec![]);

        assert_eq!(sessions.len(), 3);
        // Results are sorted by session name (alphabetical).
        let by_name: std::collections::HashMap<&str, &CachedTmuxSession> =
            sessions.iter().map(|s| (s.name.as_str(), s)).collect();

        assert_eq!(by_name["alpha"].path, "/work/alpha");
        assert_eq!(by_name["beta"].path, "/work/beta");
        assert_eq!(by_name["gamma"].path, "/tmp/scratch");
    }

    #[test]
    fn parse_tmux_output_new_format_handles_tabs_in_none_of_the_fields() {
        // Sanity: tmux session names and paths never contain tabs, so tab is a
        // reliable separator. A well-formed line with no embedded tabs parses cleanly.
        let input = "repo_main\t1\t/workspace/orchardist\t1700000000\t1700001000";

        let sessions = parse_tmux_sessions_from_panes(input, None, |_| String::new(), |_| vec![]);

        assert_eq!(sessions.len(), 1);
        assert_eq!(sessions[0].name, "repo_main");
        assert_eq!(sessions[0].path, "/workspace/orchardist");
    }

    #[test]
    fn parse_tmux_output_new_format_empty_input_returns_empty() {
        let sessions = parse_tmux_sessions_from_panes("", None, |_| String::new(), |_| vec![]);
        assert!(sessions.is_empty());
    }

    // -- vanished_hosts -------------------------------------------------------

    #[test]
    fn vanished_hosts_returns_hosts_in_old_but_not_live() {
        let old: HashSet<String> = ["A", "B", "C"].iter().map(|s| s.to_string()).collect();
        let live: HashSet<String> = ["A", "C"].iter().map(|s| s.to_string()).collect();
        // vanished_hosts guarantees a sorted result — no manual sort needed.
        let result = vanished_hosts(&old, &live);
        assert_eq!(result, vec!["B"]);
    }

    #[test]
    fn vanished_hosts_empty_when_all_live() {
        let old: HashSet<String> = ["A", "B"].iter().map(|s| s.to_string()).collect();
        let live: HashSet<String> = ["A", "B"].iter().map(|s| s.to_string()).collect();
        assert!(vanished_hosts(&old, &live).is_empty());
    }

    #[test]
    fn vanished_hosts_all_vanished_when_live_empty() {
        let old: HashSet<String> = ["A", "B"].iter().map(|s| s.to_string()).collect();
        let live: HashSet<String> = HashSet::new();
        // vanished_hosts guarantees a sorted result — no manual sort needed.
        let result = vanished_hosts(&old, &live);
        assert_eq!(result, vec!["A", "B"]);
    }

    #[test]
    fn vanished_hosts_empty_when_both_empty() {
        let old: HashSet<String> = HashSet::new();
        let live: HashSet<String> = HashSet::new();
        assert!(vanished_hosts(&old, &live).is_empty());
    }

    // -- tmux_cache_path for fork hosts ---------------------------------------

    #[test]
    fn tmux_cache_path_for_boxd_fork_host() {
        let path = cache::tmux_cache_path(Some("boxd@issue1.boxd.sh"));
        let name = path.file_name().unwrap().to_str().unwrap();
        // '@' and '.' replaced with '_'
        assert_eq!(name, "boxd_issue1_boxd_sh_tmux_sessions.json");
    }

    // -- snapshot_fork_hosts_for_remote ---------------------------------------

    #[test]
    fn snapshot_fork_hosts_for_remote_returns_empty_for_non_boxd_fork() {
        use crate::global_config::RemoteConfig;
        use crate::remote_adapter::RemoteKind;

        let config = crate::global_config::RepoConfig {
            slug: "acme/myrepo".to_string(),
            path: "/workspace/myrepo".to_string(),
            remotes: vec![],
        };
        let remote = RemoteConfig {
            name: "remmy".to_string(),
            host: "ubuntu@myhost".to_string(),
            path: "/workspace".to_string(),
            shell: "ssh".to_string(),
            kind: RemoteKind::Remmy,
            allow_transitive: false,
        };

        let result = snapshot_fork_hosts_for_remote(&config, &remote);
        assert!(result.is_empty(), "non-BoxdFork remotes return empty set");
    }

    // -- is_fork_of_golden ----------------------------------------------------

    #[test]
    fn is_fork_of_golden_accepts_direct_subdomain() {
        assert!(
            is_fork_of_golden("issue1.boxd.sh", "boxd.sh"),
            "issue1.boxd.sh is a subdomain of boxd.sh separated by a dot"
        );
    }

    #[test]
    fn is_fork_of_golden_rejects_suffix_without_dot_boundary() {
        assert!(
            !is_fork_of_golden("xboxd.sh", "boxd.sh"),
            "xboxd.sh must not match golden boxd.sh — no dot boundary"
        );
        assert!(
            !is_fork_of_golden("myboxd.sh", "boxd.sh"),
            "myboxd.sh must not match golden boxd.sh — no dot boundary"
        );
    }

    #[test]
    fn snapshot_fork_hosts_filters_by_golden_host() {
        use crate::global_config::RemoteConfig;
        use crate::remote_adapter::RemoteKind;

        let dir = tempfile::tempdir().unwrap();
        let cache_dir = dir.path();
        std::fs::create_dir_all(cache_dir).unwrap();

        let config = crate::global_config::RepoConfig {
            slug: "acme/myrepo".to_string(),
            path: "/workspace/myrepo".to_string(),
            remotes: vec![],
        };
        let remote = RemoteConfig {
            name: "boxd".to_string(),
            host: "boxd.sh".to_string(),
            path: "/workspace".to_string(),
            shell: "ssh".to_string(),
            kind: RemoteKind::BoxdFork,
            allow_transitive: false,
        };

        // Write a remote_worktrees cache containing a mix of:
        //   - a fork of THIS golden (boxd@issue1.boxd.sh)          → included
        //   - a fork of a DIFFERENT golden (boxd@issue1.other.sh)  → excluded
        //   - a local entry (no host)                              → excluded
        //   - a suffix-match without dot boundary (boxd@myboxd.sh) → excluded
        let cache_path = cache::cache_path_in(
            config.owner(),
            config.repo_name(),
            "remote_worktrees",
            cache_dir,
        );
        let entries = vec![
            wt(
                "/fork/issue1",
                "issue1/branch",
                false,
                Some("boxd@issue1.boxd.sh"),
            ),
            wt(
                "/fork/other",
                "other/branch",
                false,
                Some("boxd@issue1.other.sh"),
            ),
            wt("/local/wt", "local/branch", false, None),
            wt(
                "/fork/suffix",
                "suffix/branch",
                false,
                Some("boxd@myboxd.sh"),
            ),
        ];
        cache::write_cache(&cache_path, &entries).unwrap();

        let result = snapshot_fork_hosts_for_remote_in(&config, &remote, cache_dir);

        assert_eq!(
            result,
            std::collections::HashSet::from(["boxd@issue1.boxd.sh".to_string()]),
            "only the fork of this golden host must appear in the snapshot"
        );
    }

    // -- delete_vanished_fork_caches ------------------------------------------

    /// Proves that a stale fork cache is deleted and a live fork's cache is
    /// preserved.
    ///
    /// This is the canonical AC1 regression test: the snapshot approach
    /// correctly identifies vanished forks via real host strings and removes
    /// their caches without any filename reverse-engineering.
    #[test]
    fn e2e_vanished_fork_cache_deleted_live_fork_preserved() {
        use crate::remote_adapter::RemoteKind;

        let dir = tempfile::tempdir().unwrap();
        let cache_dir = dir.path();
        std::fs::create_dir_all(cache_dir).unwrap();

        let fork1_host = "boxd@issue1.boxd.sh";
        let fork2_host = "boxd@issue2.boxd.sh";

        // Pre-populate cache files for both forks (state before this cycle).
        let fork1_path = cache::tmux_cache_path_in(Some(fork1_host), cache_dir);
        let fork2_path = cache::tmux_cache_path_in(Some(fork2_host), cache_dir);
        std::fs::write(
            &fork1_path,
            r#"{"last_refreshed":"2026-01-01T00:00:00Z","entries":[]}"#,
        )
        .unwrap();
        std::fs::write(
            &fork2_path,
            r#"{"last_refreshed":"2026-01-01T00:00:00Z","entries":[]}"#,
        )
        .unwrap();

        // Snapshot: both forks were known before mutation.
        let old_hosts: HashSet<String> = [fork1_host.to_string(), fork2_host.to_string()].into();
        // Live: only fork2 remains.
        let live_hosts: HashSet<String> = [fork2_host.to_string()].into();

        let gone = vanished_hosts(&old_hosts, &live_hosts);
        delete_vanished_fork_caches_in(
            &gone,
            "acme/myrepo",
            "boxd-fork-remote",
            RemoteKind::BoxdFork,
            cache_dir,
        );

        assert!(
            !fork1_path.exists(),
            "issue1 cache must be deleted — it vanished from the live set"
        );
        assert!(
            fork2_path.exists(),
            "issue2 cache must be preserved — it is still live"
        );
    }

    /// Proves all stale caches are deleted when the live set is empty.
    #[test]
    fn delete_vanished_fork_caches_deletes_all_when_live_empty() {
        use crate::remote_adapter::RemoteKind;

        let dir = tempfile::tempdir().unwrap();
        let cache_dir = dir.path();
        std::fs::create_dir_all(cache_dir).unwrap();

        let fork1_host = "boxd@issue1.boxd.sh";
        let fork2_host = "boxd@issue2.boxd.sh";
        let fork1_path = cache::tmux_cache_path_in(Some(fork1_host), cache_dir);
        let fork2_path = cache::tmux_cache_path_in(Some(fork2_host), cache_dir);
        std::fs::write(&fork1_path, "{}").unwrap();
        std::fs::write(&fork2_path, "{}").unwrap();

        let old_hosts: HashSet<String> = [fork1_host.to_string(), fork2_host.to_string()].into();
        let live_hosts: HashSet<String> = HashSet::new();
        let gone = vanished_hosts(&old_hosts, &live_hosts);

        delete_vanished_fork_caches_in(
            &gone,
            "acme/myrepo",
            "boxd",
            RemoteKind::BoxdFork,
            cache_dir,
        );

        assert!(!fork1_path.exists(), "fork1 must be deleted");
        assert!(!fork2_path.exists(), "fork2 must be deleted");
    }

    /// Proves no caches are deleted when all previously-known forks are still live.
    #[test]
    fn delete_vanished_fork_caches_keeps_all_when_all_live() {
        use crate::remote_adapter::RemoteKind;

        let dir = tempfile::tempdir().unwrap();
        let cache_dir = dir.path();
        std::fs::create_dir_all(cache_dir).unwrap();

        let fork1_host = "boxd@issue1.boxd.sh";
        let fork1_path = cache::tmux_cache_path_in(Some(fork1_host), cache_dir);
        std::fs::write(&fork1_path, "{}").unwrap();

        let old_hosts: HashSet<String> = [fork1_host.to_string()].into();
        let live_hosts: HashSet<String> = [fork1_host.to_string()].into();
        let gone = vanished_hosts(&old_hosts, &live_hosts);

        delete_vanished_fork_caches_in(
            &gone,
            "acme/myrepo",
            "boxd",
            RemoteKind::BoxdFork,
            cache_dir,
        );

        assert!(
            fork1_path.exists(),
            "fork1 must NOT be deleted — it is live"
        );
    }
}
