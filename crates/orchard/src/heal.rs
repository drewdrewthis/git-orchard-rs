//! Heal command: audits and repairs drifted Orchard state.
//!
//! The heal command inspects tmux sessions, worktrees, Claude state files, and
//! cache files to find inconsistencies and stale data. It can operate in dry-run
//! mode (default) or apply fixes with `--fix`.
//!
//! Architecture follows the functional core / imperative shell pattern:
//! - `diagnose()` is a pure function that computes a `HealReport` from its inputs.
//! - `apply_fixes()` performs the actual I/O side effects, through the
//!   injectable [`FixExecutor`] port so tests stay hermetic.
//! - `format_report()` formats a human-readable text output.
use std::path::Path;

use serde::Serialize;

use crate::cache;
use crate::types::TmuxSession;

pub mod fix_executor;

pub use fix_executor::{FixCall, FixExecutor, ProcessFixExecutor, RecordingFixExecutor};

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

/// The category of a heal finding.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum HealCategory {
    /// A tmux session with no matching worktree path on disk.
    OrphanedSession,
    /// A tmux session whose working directory no longer exists on disk.
    DeadSessionDirectory,
    /// A `~/.cache/orchard/` file whose repo slug is not in the active config.
    StaleCache,
    /// A worktree whose associated PR has been merged.
    MergedPrWorktree,
    /// A worktree whose associated PR has been closed without merging.
    ClosedPrWorktree,
    /// A worktree whose linked GitHub issue is closed.
    ClosedIssueWorktree,
    /// A tmux session whose name does not match the Orchard naming convention.
    SessionNamingMismatch,
    /// Multiple tmux sessions pointing to the same worktree path.
    MultipleSessionsPerWorktree,
}

/// Severity level for a heal finding.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Severity {
    /// Everything looks healthy — no action needed.
    Ok,
    /// Something looks off but is not immediately broken.
    Warning,
    /// A clear problem that should be fixed.
    Error,
}

/// The action associated with a heal finding.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case", tag = "type", content = "value")]
pub enum HealAction {
    /// Kill the named tmux session.
    KillSession(String),
    /// Delete the file at the given path.
    DeleteFile(String),
    /// Flag the item for manual cleanup; no automatic action.
    FlagForCleanup(String),
    /// Informational only — report but take no action.
    ReportOnly(String),
    /// No action needed; item is healthy.
    None,
}

/// A single finding from the heal diagnosis.
#[derive(Debug, Clone, Serialize)]
pub struct HealFinding {
    /// Category of the problem.
    pub category: HealCategory,
    /// Severity of the problem.
    pub severity: Severity,
    /// Human-readable description of the finding.
    pub message: String,
    /// The action to take, if any.
    pub action: HealAction,
    /// True when this finding targets the tmux session that invoked `orchard heal`.
    ///
    /// When `is_self` is true, `apply_fixes` must skip the action rather than
    /// killing the session — a self-kill would terminate the running process.
    ///
    /// **Invariant**: `is_self` is set by matching the finding's session name
    /// against `current_session` passed to [`diagnose`]. This assumes both
    /// names come from the same tmux server (tmux session names are unique
    /// within a server but NOT across servers). Callers diagnosing remote or
    /// cross-socket sessions MUST NOT pass a `current_session` value sourced
    /// from a different server, or the self-protection invariant breaks.
    ///
    /// **JSON consumers**: a finding with `is_self: true` and `severity:
    /// error` is the "do-not-act" marker — `apply_fixes` will skip it and
    /// `orchard heal --fix` aborts with exit 1 before reaching any action.
    /// External tooling reading `--json` output must honour this signal.
    pub is_self: bool,
}

/// The complete output of a heal diagnosis run.
#[derive(Debug, Serialize)]
pub struct HealReport {
    /// All findings, both healthy and problematic.
    pub findings: Vec<HealFinding>,
}

impl HealReport {
    /// Returns true when every finding is healthy (severity == Ok).
    pub fn is_all_ok(&self) -> bool {
        self.findings.iter().all(|f| f.severity == Severity::Ok)
    }

    /// Returns only the findings that have an actionable fix.
    pub fn actionable(&self) -> impl Iterator<Item = &HealFinding> {
        self.findings
            .iter()
            .filter(|f| !matches!(f.action, HealAction::None))
    }
}

/// Returns the first finding (if any) that is both is_self and Severity::Error.
///
/// When such a finding exists, the heal CLI must abort before applying any
/// fixes — the invoking session is in an unknown-bad state, and silent
/// repairs are unsafe. See AC #2 of issue #361.
pub fn detect_self_error(report: &HealReport) -> Option<&HealFinding> {
    report
        .findings
        .iter()
        .find(|f| f.is_self && f.severity == Severity::Error)
}

/// Typed outcome of a single heal action.
///
/// `message` (on `FixResult`) stays for human display, but tests and
/// programmatic consumers should branch on this enum — string-prefix
/// matching on the message is brittle (a localized or reformatted
/// message silently breaks the kill-vs-skip distinction).
///
/// Resolves issue #371.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum FixOutcome {
    /// A KillSession action killed the named tmux session.
    Killed,
    /// A KillSession action targeted the invoking session and was skipped
    /// per the self-protection invariant (see `is_self` on HealFinding).
    SkippedSelf,
    /// A DeleteFile action removed the file.
    Deleted,
    /// A FlagForCleanup action surfaced the item to the operator.
    Flagged,
    /// A DeleteFile action refused the path (outside cache/tmp).
    Skipped,
    /// The underlying syscall (`tmux kill-session`, `fs::remove_file`,
    /// etc.) returned an error. The error string is in
    /// [`FixResult::error`]; the action shape is in the originating
    /// HealFinding.
    Failed,
}

/// Result of applying a single heal action.
#[derive(Debug, Serialize)]
pub struct FixResult {
    /// The typed outcome — branch on this in tests and programmatic
    /// consumers. The `message` field below is for human display only.
    pub outcome: FixOutcome,
    /// The finding that was addressed.
    pub message: String,
    /// Whether the fix succeeded.
    pub success: bool,
    /// Error detail when `success` is false.
    pub error: Option<String>,
}

// ---------------------------------------------------------------------------
// Inputs for diagnose()
// ---------------------------------------------------------------------------

/// Describes a worktree to the healer (lightweight, no PR/issue enrichment needed at this layer).
#[derive(Debug, Clone)]
pub struct HealWorktree {
    /// Absolute filesystem path to the worktree root.
    pub path: String,
    /// The branch checked out in this worktree.
    pub branch: String,
    /// The expected Orchard session name derived from this worktree.
    pub expected_session_name: Option<String>,
    /// PR state for this worktree's branch ("open", "merged", "closed"), if any.
    pub pr_state: Option<String>,
    /// PR number, if any.
    pub pr_number: Option<u32>,
    /// Linked GitHub issue state ("open", "closed"), if any.
    pub issue_state: Option<String>,
}

/// Bundled inputs for `diagnose`.
///
/// All fields are borrowed slices/references — `HealInput` is `Copy` and zero-cost
/// to construct. The `Default` impl produces an all-empty input, which lets test
/// sites that only need a subset of fields use struct-update syntax:
///
/// ```ignore
/// let report = diagnose(&HealInput {
///     sessions: &sessions,
///     worktrees: &worktrees,
///     ..Default::default()
/// });
/// ```
///
/// # Invariant
///
/// All fields must remain borrowed (`&'a [T]` or `Option<&'a T>`). Adding an
/// owned `Vec<T>` field would break both the `Copy` derive and the zero-copy
/// contract — drop the derive and revisit the design instead.
#[derive(Debug, Clone, Copy, Default)]
pub struct HealInput<'a> {
    /// Live tmux sessions from the daemon (or any equivalent source).
    pub sessions: &'a [TmuxSession],
    /// Enriched worktrees to check.
    pub worktrees: &'a [HealWorktree],
    /// Cache file names from `~/.cache/orchard/`.
    pub cache_files: &'a [String],
    /// Repo slugs from global config (e.g. `["owner/repo"]`).
    pub known_repo_slugs: &'a [String],
    /// Name of the tmux session invoking heal, if any. `KillSession` findings
    /// whose target matches this name will have `is_self` set to `true`.
    pub current_session: Option<&'a str>,
}

// ---------------------------------------------------------------------------
// Core diagnostic function (pure)
// ---------------------------------------------------------------------------

/// Diagnoses the health of the Orchard environment.
///
/// This is a pure function: all I/O is performed by callers who gather the
/// inputs via `HealInput`. The function only analyzes and classifies findings.
///
/// `current_session` (via `HealInput`) names the tmux session invoking heal, if
/// any. `KillSession` findings whose target matches this name will have `is_self`
/// set to `true`, allowing callers to skip self-destructive kills.
pub fn diagnose(input: &HealInput<'_>) -> HealReport {
    let HealInput {
        sessions,
        worktrees,
        cache_files,
        known_repo_slugs,
        current_session,
    } = *input;

    let mut findings = Vec::new();

    check_sessions(sessions, worktrees, &mut findings, current_session);
    check_cache_files(cache_files, known_repo_slugs, &mut findings);
    check_worktree_pr_states(worktrees, &mut findings);
    check_worktree_issue_states(worktrees, &mut findings);
    check_session_naming(sessions, worktrees, &mut findings);
    check_multiple_sessions_per_worktree(sessions, worktrees, &mut findings);

    HealReport { findings }
}

// ---------------------------------------------------------------------------
// Individual checks
// ---------------------------------------------------------------------------

fn check_sessions(
    sessions: &[TmuxSession],
    worktrees: &[HealWorktree],
    findings: &mut Vec<HealFinding>,
    current_session: Option<&str>,
) {
    let worktree_paths: Vec<&str> = worktrees.iter().map(|w| w.path.as_str()).collect();

    for session in sessions {
        // Prefer the live active-pane cwd over the frozen session path. Users
        // `cd` inside sessions, so the active pane reflects where they actually
        // are — the frozen path may be stale but the session is still healthy.
        let effective_path = session.active_pane_cwd.as_deref().unwrap_or(&session.path);
        let path_exists = Path::new(effective_path).exists();
        let path_has_worktree = worktree_paths.contains(&effective_path);
        let is_self = current_session.map(|s| s == session.name).unwrap_or(false);

        if !path_exists {
            findings.push(HealFinding {
                category: HealCategory::DeadSessionDirectory,
                severity: Severity::Error,
                message: format!(
                    "Session \"{}\" points to non-existent path \"{}\"",
                    session.name, effective_path
                ),
                action: HealAction::KillSession(session.name.clone()),
                is_self,
            });
        } else if !path_has_worktree {
            findings.push(HealFinding {
                category: HealCategory::OrphanedSession,
                severity: Severity::Warning,
                message: format!(
                    "Session \"{}\" has no matching worktree (path: {})",
                    session.name, effective_path
                ),
                action: HealAction::KillSession(session.name.clone()),
                is_self,
            });
        }
    }
}

fn check_cache_files(
    cache_files: &[String],
    known_repo_slugs: &[String],
    findings: &mut Vec<HealFinding>,
) {
    for filename in cache_files {
        // Cache files follow the pattern: {owner}_{repo}_{source}.json
        // Skip files that don't follow the per-repo naming convention.
        if let Some(repo_slug) = extract_repo_slug_from_cache_filename(filename)
            && !known_repo_slugs.contains(&repo_slug)
        {
            let cache_path = cache::cache_dir().join(filename);
            findings.push(HealFinding {
                category: HealCategory::StaleCache,
                severity: Severity::Warning,
                message: format!(
                    "Stale cache file \"{}\" for unknown repo \"{}\"",
                    filename, repo_slug
                ),
                action: HealAction::DeleteFile(cache_path.to_string_lossy().to_string()),
                is_self: false,
            });
        }
    }
}

fn check_worktree_pr_states(worktrees: &[HealWorktree], findings: &mut Vec<HealFinding>) {
    for wt in worktrees {
        let Some(pr_state) = &wt.pr_state else {
            continue;
        };
        let pr_label = wt
            .pr_number
            .map(|n| format!("PR #{n}"))
            .unwrap_or_else(|| "PR".to_string());

        if pr_state.eq_ignore_ascii_case("merged") {
            findings.push(HealFinding {
                category: HealCategory::MergedPrWorktree,
                severity: Severity::Warning,
                message: format!("Worktree \"{}\" is stale: {} merged", wt.path, pr_label),
                action: HealAction::FlagForCleanup(wt.path.clone()),
                is_self: false,
            });
        } else if pr_state.eq_ignore_ascii_case("closed") {
            findings.push(HealFinding {
                category: HealCategory::ClosedPrWorktree,
                severity: Severity::Warning,
                message: format!(
                    "Worktree \"{}\" is stale: {} closed without merge",
                    wt.path, pr_label
                ),
                action: HealAction::FlagForCleanup(wt.path.clone()),
                is_self: false,
            });
        }
    }
}

fn check_worktree_issue_states(worktrees: &[HealWorktree], findings: &mut Vec<HealFinding>) {
    for wt in worktrees {
        let Some(issue_state) = &wt.issue_state else {
            continue;
        };
        // Only flag when the issue is closed and there's no PR (otherwise the PR check covers it).
        if issue_state.eq_ignore_ascii_case("closed") && wt.pr_state.is_none() {
            findings.push(HealFinding {
                category: HealCategory::ClosedIssueWorktree,
                severity: Severity::Warning,
                message: format!("Worktree \"{}\" is stale: linked issue is closed", wt.path),
                action: HealAction::FlagForCleanup(wt.path.clone()),
                is_self: false,
            });
        }
    }
}

fn check_session_naming(
    sessions: &[TmuxSession],
    worktrees: &[HealWorktree],
    findings: &mut Vec<HealFinding>,
) {
    for session in sessions {
        // Match by live active-pane cwd first; fall back to frozen session path.
        let effective_path = session.active_pane_cwd.as_deref().unwrap_or(&session.path);
        let Some(wt) = worktrees.iter().find(|w| w.path == effective_path) else {
            continue;
        };
        let Some(expected) = &wt.expected_session_name else {
            continue;
        };
        if &session.name != expected {
            findings.push(HealFinding {
                category: HealCategory::SessionNamingMismatch,
                severity: Severity::Warning,
                message: format!(
                    "Session naming mismatch: expected \"{}\" but found \"{}\"",
                    expected, session.name
                ),
                action: HealAction::ReportOnly(format!(
                    "Rename session \"{}\" to \"{}\"",
                    session.name, expected
                )),
                is_self: false,
            });
        }
    }
}

fn check_multiple_sessions_per_worktree(
    sessions: &[TmuxSession],
    worktrees: &[HealWorktree],
    findings: &mut Vec<HealFinding>,
) {
    for wt in worktrees {
        // Match by live active-pane cwd when available; fall back to frozen path.
        let matching: Vec<&TmuxSession> = sessions
            .iter()
            .filter(|s| {
                let effective = s.active_pane_cwd.as_deref().unwrap_or(&s.path);
                effective == wt.path
            })
            .collect();

        if matching.len() > 1 {
            let names: Vec<&str> = matching.iter().map(|s| s.name.as_str()).collect();
            findings.push(HealFinding {
                category: HealCategory::MultipleSessionsPerWorktree,
                severity: Severity::Warning,
                message: format!(
                    "Multiple sessions for worktree \"{}\": {}",
                    wt.path,
                    names.join(", ")
                ),
                action: HealAction::ReportOnly(format!(
                    "Worktree \"{}\" has {} sessions",
                    wt.path,
                    matching.len()
                )),
                is_self: false,
            });
        }
    }
}

// ---------------------------------------------------------------------------
// Fix application (imperative shell)
// ---------------------------------------------------------------------------

/// Applies the actions from a set of findings.
///
/// Kills sessions and deletes files as directed. Worktrees flagged with
/// `FlagForCleanup` are never deleted automatically — they require manual action.
///
/// Thin wrapper over [`apply_fixes_with`] bound to [`ProcessFixExecutor`].
pub fn apply_fixes(findings: &[HealFinding]) -> Vec<FixResult> {
    apply_fixes_with(findings, &ProcessFixExecutor)
}

/// Applies the actions from a set of findings using an injected [`FixExecutor`].
///
/// Same behaviour as [`apply_fixes`], with the tmux and filesystem effects
/// supplied by the caller. Tests pass a [`RecordingFixExecutor`] and assert on
/// the recorded calls instead of killing sessions or deleting files.
///
/// Resolves issue #372.
pub fn apply_fixes_with(findings: &[HealFinding], exec: &dyn FixExecutor) -> Vec<FixResult> {
    let mut results = Vec::new();

    for finding in findings {
        match &finding.action {
            HealAction::KillSession(name) => {
                if finding.is_self {
                    results.push(FixResult {
                        outcome: FixOutcome::SkippedSelf,
                        message: format!("Skipped session \"{}\" — refusing to kill self", name),
                        success: true,
                        error: None,
                    });
                    continue;
                }
                // The `is_self` guard above makes the cross-cutting
                // wrapper redundant here, but use it anyway so the code
                // path matches every other in-process kill site.
                let current = exec.current_session();
                let result = exec.kill_session(name, current.as_deref());
                let success = result.is_ok();
                results.push(FixResult {
                    outcome: if success {
                        FixOutcome::Killed
                    } else {
                        FixOutcome::Failed
                    },
                    message: format!("Killed session \"{}\"", name),
                    success,
                    error: result.err(),
                });
            }
            HealAction::DeleteFile(path) => {
                let p = Path::new(path);
                let in_cache = p.starts_with(cache::cache_dir());
                let in_tmp = p.starts_with("/tmp")
                    && p.file_name()
                        .is_some_and(|n| n.to_string_lossy().starts_with("orchard-claude-"));
                if !in_cache && !in_tmp {
                    results.push(FixResult {
                        outcome: FixOutcome::Skipped,
                        message: format!("Skipped file outside expected directories: \"{}\"", path),
                        success: false,
                        error: Some("path not in ~/.cache/orchard/ or /tmp/orchard-*".to_string()),
                    });
                    continue;
                }
                let result = exec.remove_file(path);
                let success = result.is_ok();
                results.push(FixResult {
                    outcome: if success {
                        FixOutcome::Deleted
                    } else {
                        FixOutcome::Failed
                    },
                    message: format!("Deleted file \"{}\"", path),
                    success,
                    error: result.err(),
                });
            }
            HealAction::FlagForCleanup(desc) => {
                results.push(FixResult {
                    outcome: FixOutcome::Flagged,
                    message: format!("Flagged for manual cleanup: {}", desc),
                    success: true,
                    error: None,
                });
            }
            HealAction::ReportOnly(_) | HealAction::None => {
                // No action to apply.
            }
        }
    }

    results
}

// ---------------------------------------------------------------------------
// Text report formatting
// ---------------------------------------------------------------------------

/// Formats a heal report as a human-readable string with icons.
///
/// Healthy items use a checkmark (`✓`), warnings use `⚠`, and errors use `✗`.
/// When there are actionable findings, a suggestion to run `--fix` is appended.
pub fn format_report(report: &HealReport, fix_results: Option<&[FixResult]>) -> String {
    let mut lines: Vec<String> = Vec::new();

    let ok_sessions = report
        .findings
        .iter()
        .filter(|f| {
            matches!(
                f.category,
                HealCategory::OrphanedSession | HealCategory::DeadSessionDirectory
            ) && f.severity == Severity::Ok
        })
        .count();

    let bad_sessions = report
        .findings
        .iter()
        .filter(|f| {
            matches!(
                f.category,
                HealCategory::OrphanedSession | HealCategory::DeadSessionDirectory
            ) && f.severity != Severity::Ok
        })
        .count();

    // Summary lines.
    let total_session_issues = bad_sessions;
    let session_names: Vec<String> = report
        .findings
        .iter()
        .filter(|f| {
            matches!(
                f.category,
                HealCategory::OrphanedSession | HealCategory::DeadSessionDirectory
            )
        })
        .map(|f| f.message.clone())
        .collect();

    // Build a summary of session health.
    let total_sessions_checked = ok_sessions + bad_sessions;
    if (total_sessions_checked > 0 || session_names.is_empty()) && total_session_issues == 0 {
        lines.push(format!(
            "\u{2713} {} tmux session{} OK",
            total_sessions_checked,
            if total_sessions_checked == 1 { "" } else { "s" }
        ));
    }

    // Print all session-related findings.
    for finding in report.findings.iter().filter(|f| {
        matches!(
            f.category,
            HealCategory::OrphanedSession | HealCategory::DeadSessionDirectory
        )
    }) {
        let icon = severity_icon(&finding.severity);
        let suffix = if finding.is_self {
            " \u{2014} skipped (self)"
        } else {
            ""
        };
        lines.push(format!("{} {}{}", icon, finding.message, suffix));
    }

    // Print all non-session findings.
    for finding in report.findings.iter().filter(|f| {
        !matches!(
            f.category,
            HealCategory::OrphanedSession | HealCategory::DeadSessionDirectory
        )
    }) {
        let icon = severity_icon(&finding.severity);
        let suffix = if finding.is_self {
            " \u{2014} skipped (self)"
        } else {
            ""
        };
        lines.push(format!("{} {}{}", icon, finding.message, suffix));
    }

    // Fix results section.
    if let Some(results) = fix_results
        && !results.is_empty()
    {
        lines.push(String::new());
        lines.push("Applied fixes:".to_string());
        for r in results {
            let icon = if r.success { "\u{2713}" } else { "\u{2716}" };
            if let Some(err) = &r.error {
                lines.push(format!("  {} {} ({})", icon, r.message, err));
            } else {
                lines.push(format!("  {} {}", icon, r.message));
            }
        }
    }

    // Suggest --fix when there are actionable findings and we're not in fix mode.
    let has_actionable = report.actionable().any(|f| {
        matches!(
            f.action,
            HealAction::KillSession(_) | HealAction::DeleteFile(_)
        )
    });
    if fix_results.is_none() && has_actionable {
        lines.push(String::new());
        lines.push("Run `orchard heal --fix` to repair.".to_string());
    }

    lines.join("\n")
}

fn severity_icon(severity: &Severity) -> &'static str {
    match severity {
        Severity::Ok => "\u{2713}",      // ✓
        Severity::Warning => "\u{26a0}", // ⚠
        Severity::Error => "\u{2716}",   // ✗
    }
}

// ---------------------------------------------------------------------------
// I/O helpers for gathering heal inputs
// ---------------------------------------------------------------------------

/// Reads all files from the orchard cache directory and returns their filenames.
pub fn gather_cache_files() -> Vec<String> {
    let dir = cache::cache_dir();
    let Ok(entries) = std::fs::read_dir(&dir) else {
        return Vec::new();
    };

    entries
        .flatten()
        .filter_map(|e| {
            let name = e.file_name().to_string_lossy().to_string();
            if name.ends_with(".json") {
                Some(name)
            } else {
                None
            }
        })
        .collect()
}

// ---------------------------------------------------------------------------
// Cache filename parsing
// ---------------------------------------------------------------------------

/// Extracts a `owner/repo` slug from a cache filename like `owner_repo_issues.json`.
///
/// Returns `None` for filenames that don't follow the per-repo naming pattern
/// (e.g. `tmux_sessions.json`, `config.json`).
fn extract_repo_slug_from_cache_filename(filename: &str) -> Option<String> {
    // Per-repo files follow: {owner}_{repo}_{source}.json
    // We need at least 3 underscore-separated parts before the extension.
    let without_ext = filename.strip_suffix(".json")?;

    // Known non-repo files.
    let non_repo_prefixes = [
        "tmux_sessions",
        "config",
        "session_manifest",
        "last_selection",
    ];
    if non_repo_prefixes.iter().any(|p| without_ext.starts_with(p)) {
        return None;
    }

    // Must have at least 3 parts: owner_repo_source
    let parts: Vec<&str> = without_ext.splitn(3, '_').collect();
    if parts.len() < 3 {
        return None;
    }

    Some(format!("{}/{}", parts[0], parts[1]))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::TmuxSession;

    fn make_session(name: &str, path: &str) -> TmuxSession {
        TmuxSession {
            name: name.to_string(),
            path: path.to_string(),
            attached: false,
            pane_title: None,
            active_pane_cwd: None,
        }
    }

    fn make_worktree(path: &str, branch: &str) -> HealWorktree {
        HealWorktree {
            path: path.to_string(),
            branch: branch.to_string(),
            expected_session_name: None,
            pr_state: None,
            pr_number: None,
            issue_state: None,
        }
    }

    // -----------------------------------------------------------------------
    // Orphaned session detection
    // -----------------------------------------------------------------------

    #[test]
    fn detect_orphaned_session_no_matching_worktree() {
        let sessions = vec![make_session(
            "myrepo_old-feature",
            "/tmp/nonexistent-worktree",
        )];
        let worktrees = vec![make_worktree("/workspace/main", "main")];
        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &worktrees,
            ..Default::default()
        });

        // Path doesn't exist → DeadSessionDirectory, not orphaned
        let finding = &report.findings[0];
        assert_eq!(finding.category, HealCategory::DeadSessionDirectory);
        assert_eq!(finding.severity, Severity::Error);
    }

    #[test]
    fn detect_orphaned_session_path_exists_but_no_worktree() {
        // Use a real path that exists but is not a worktree.
        let sessions = vec![make_session("myrepo_old-feature", "/tmp")];
        let worktrees = vec![make_worktree("/workspace/main", "main")];
        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &worktrees,
            ..Default::default()
        });

        let finding = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::OrphanedSession);
        assert!(finding.is_some(), "should detect orphaned session");
        assert_eq!(finding.unwrap().severity, Severity::Warning);
        assert!(
            matches!(&finding.unwrap().action, HealAction::KillSession(name) if name == "myrepo_old-feature")
        );
    }

    // -----------------------------------------------------------------------
    // Dead session directory
    // -----------------------------------------------------------------------

    #[test]
    fn detect_dead_session_directory() {
        let sessions = vec![make_session("myrepo_gone", "/tmp/nonexistent-path-xyz")];
        let worktrees = vec![];
        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &worktrees,
            ..Default::default()
        });

        let finding = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::DeadSessionDirectory);
        assert!(finding.is_some());
        assert!(
            matches!(&finding.unwrap().action, HealAction::KillSession(n) if n == "myrepo_gone")
        );
    }

    // -----------------------------------------------------------------------
    // Stale cache files
    // -----------------------------------------------------------------------

    #[test]
    fn detect_stale_cache_file_for_unknown_repo() {
        let cache_files = vec!["ghost_repo_issues.json".to_string()];
        let known_slugs = vec!["owner/my-project".to_string()];
        let report = diagnose(&HealInput {
            cache_files: &cache_files,
            known_repo_slugs: &known_slugs,
            ..Default::default()
        });

        let finding = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::StaleCache);
        assert!(finding.is_some(), "should detect stale cache file");
        assert!(matches!(finding.unwrap().action, HealAction::DeleteFile(_)));
    }

    #[test]
    fn no_stale_cache_finding_for_known_repo() {
        let cache_files = vec!["owner_myproject_issues.json".to_string()];
        let known_slugs = vec!["owner/myproject".to_string()];
        let report = diagnose(&HealInput {
            cache_files: &cache_files,
            known_repo_slugs: &known_slugs,
            ..Default::default()
        });

        let stale = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::StaleCache);
        assert!(stale.is_none());
    }

    #[test]
    fn tmux_sessions_cache_file_ignored() {
        let cache_files = vec!["tmux_sessions.json".to_string()];
        let known_slugs: Vec<String> = vec![];
        let report = diagnose(&HealInput {
            cache_files: &cache_files,
            known_repo_slugs: &known_slugs,
            ..Default::default()
        });

        let stale = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::StaleCache);
        assert!(stale.is_none(), "tmux_sessions.json is not a repo cache");
    }

    // -----------------------------------------------------------------------
    // Merged/closed PR worktrees
    // -----------------------------------------------------------------------

    #[test]
    fn flag_worktree_with_merged_pr() {
        let mut wt = make_worktree(".worktrees/issue3-tests", "issue3/tests");
        wt.pr_state = Some("merged".to_string());
        wt.pr_number = Some(12);
        let report = diagnose(&HealInput {
            worktrees: &[wt],
            ..Default::default()
        });

        let finding = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::MergedPrWorktree);
        assert!(finding.is_some());
        assert!(finding.unwrap().message.contains("PR #12 merged"));
        // Must not delete automatically.
        assert!(matches!(
            finding.unwrap().action,
            HealAction::FlagForCleanup(_)
        ));
    }

    #[test]
    fn flag_worktree_with_closed_pr() {
        let mut wt = make_worktree(".worktrees/issue5-fix", "issue5/fix");
        wt.pr_state = Some("closed".to_string());
        wt.pr_number = Some(15);
        let report = diagnose(&HealInput {
            worktrees: &[wt],
            ..Default::default()
        });

        let finding = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::ClosedPrWorktree);
        assert!(finding.is_some());
    }

    #[test]
    fn merged_pr_worktree_is_not_deleted_by_fix() {
        let mut wt = make_worktree(".worktrees/issue3-tests", "issue3/tests");
        wt.pr_state = Some("merged".to_string());
        wt.pr_number = Some(12);
        let report = diagnose(&HealInput {
            worktrees: &[wt],
            ..Default::default()
        });

        let exec = RecordingFixExecutor::new();
        let results = apply_fixes_with(&report.findings, &exec);
        // FlagForCleanup produces a result but does not kill/delete anything.
        for r in &results {
            assert_eq!(r.outcome, FixOutcome::Flagged);
        }
        assert!(
            exec.calls().is_empty(),
            "flagging must not issue any effect; got: {:?}",
            exec.calls()
        );
    }

    // -----------------------------------------------------------------------
    // Closed issue worktrees
    // -----------------------------------------------------------------------

    #[test]
    fn flag_worktree_with_closed_issue_no_pr() {
        let mut wt = make_worktree(".worktrees/issue8-refactor", "issue8/refactor");
        wt.issue_state = Some("closed".to_string());
        let report = diagnose(&HealInput {
            worktrees: &[wt],
            ..Default::default()
        });

        let finding = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::ClosedIssueWorktree);
        assert!(finding.is_some());
    }

    #[test]
    fn closed_issue_not_flagged_when_pr_exists() {
        // When there's a PR, the PR check takes precedence; issue check is skipped.
        let mut wt = make_worktree(".worktrees/issue8-refactor", "issue8/refactor");
        wt.issue_state = Some("closed".to_string());
        wt.pr_state = Some("open".to_string());
        let report = diagnose(&HealInput {
            worktrees: &[wt],
            ..Default::default()
        });

        let issue_finding = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::ClosedIssueWorktree);
        assert!(
            issue_finding.is_none(),
            "issue check skipped when PR exists"
        );
    }

    // -----------------------------------------------------------------------
    // Session naming mismatch
    // -----------------------------------------------------------------------

    #[test]
    fn detect_session_naming_mismatch() {
        let sessions = vec![make_session("wrong-name", "/workspace/feature-login")];
        let mut wt = make_worktree("/workspace/feature-login", "feature/login");
        wt.expected_session_name = Some("myrepo_feature-login".to_string());
        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &[wt],
            ..Default::default()
        });

        let finding = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::SessionNamingMismatch);
        assert!(finding.is_some());
        assert!(finding.unwrap().message.contains("myrepo_feature-login"));
        assert!(finding.unwrap().message.contains("wrong-name"));
    }

    #[test]
    fn no_mismatch_when_session_name_matches() {
        let sessions = vec![make_session(
            "myrepo_feature-login",
            "/workspace/feature-login",
        )];
        let mut wt = make_worktree("/workspace/feature-login", "feature/login");
        wt.expected_session_name = Some("myrepo_feature-login".to_string());
        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &[wt],
            ..Default::default()
        });

        let mismatch = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::SessionNamingMismatch);
        assert!(mismatch.is_none());
    }

    // -----------------------------------------------------------------------
    // Multiple sessions per worktree
    // -----------------------------------------------------------------------

    #[test]
    fn detect_multiple_sessions_for_same_worktree() {
        let sessions = vec![
            make_session("myrepo_issue10-api", "/workspace/issue10-api"),
            make_session("extra-session", "/workspace/issue10-api"),
        ];
        let wt = make_worktree("/workspace/issue10-api", "issue10/api");
        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &[wt],
            ..Default::default()
        });

        let finding = report
            .findings
            .iter()
            .find(|f| f.category == HealCategory::MultipleSessionsPerWorktree);
        assert!(finding.is_some(), "should detect multiple sessions");
        assert!(finding.unwrap().message.contains("myrepo_issue10-api"));
        assert!(finding.unwrap().message.contains("extra-session"));
    }

    // -----------------------------------------------------------------------
    // All-healthy
    // -----------------------------------------------------------------------

    #[test]
    fn all_ok_when_everything_matches() {
        // Use a real path that exists on disk so the directory check passes.
        let tmp = std::env::temp_dir();
        let path = tmp.to_string_lossy().to_string();
        let sessions = vec![make_session("myrepo_main", &path)];
        let worktrees = vec![make_worktree(&path, "main")];
        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &worktrees,
            ..Default::default()
        });

        assert!(report.is_all_ok(), "should report all-ok");
        assert_eq!(report.findings.len(), 0);
    }

    // -----------------------------------------------------------------------
    // format_report
    // -----------------------------------------------------------------------

    #[test]
    fn format_report_suggests_fix_when_actionable() {
        let sessions = vec![make_session("orphan", "/tmp")];
        let worktrees = vec![];
        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &worktrees,
            ..Default::default()
        });
        let text = format_report(&report, None);

        assert!(
            text.contains("orchard heal --fix"),
            "should suggest --fix: {text}"
        );
    }

    #[test]
    fn format_report_no_fix_suggestion_when_all_ok() {
        // Use a real path so the directory check passes.
        let tmp = std::env::temp_dir();
        let path = tmp.to_string_lossy().to_string();
        let sessions = vec![make_session("myrepo_main", &path)];
        let worktrees = vec![make_worktree(&path, "main")];
        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &worktrees,
            ..Default::default()
        });
        let text = format_report(&report, None);

        assert!(
            !text.contains("orchard heal --fix"),
            "should not suggest --fix when healthy: {text}"
        );
    }

    #[test]
    fn format_report_shows_fix_results() {
        let fix_results = vec![FixResult {
            outcome: FixOutcome::Killed,
            message: "Killed session \"orphan\"".to_string(),
            success: true,
            error: None,
        }];
        let report = HealReport { findings: vec![] };
        let text = format_report(&report, Some(&fix_results));

        assert!(text.contains("Killed session"), "{text}");
    }

    // -----------------------------------------------------------------------
    // Cache filename parsing
    // -----------------------------------------------------------------------

    #[test]
    fn extract_repo_slug_from_standard_filename() {
        let slug = extract_repo_slug_from_cache_filename("owner_myrepo_issues.json");
        assert_eq!(slug, Some("owner/myrepo".to_string()));
    }

    #[test]
    fn extract_repo_slug_returns_none_for_tmux_cache() {
        let slug = extract_repo_slug_from_cache_filename("tmux_sessions.json");
        assert!(slug.is_none());
    }

    #[test]
    fn extract_repo_slug_returns_none_for_too_few_parts() {
        let slug = extract_repo_slug_from_cache_filename("something.json");
        assert!(slug.is_none());
    }

    // -----------------------------------------------------------------------
    // Live active-pane cwd rescues session with stale frozen path (issue #297)
    // -----------------------------------------------------------------------

    /// Regression test: a session whose frozen `session.path` does not exist on
    /// disk but whose live active-pane cwd IS a known worktree path must be
    /// classified as healthy — not DeadSessionDirectory or OrphanedSession.
    ///
    /// This test MUST FAIL until `check_sessions` (or `TmuxSession`) is updated
    /// to consult `active_pane_cwd` before `path` when classifying sessions.
    #[test]
    fn live_active_pane_cwd_rescues_session_with_stale_frozen_path() {
        let worktree_path = std::env::temp_dir()
            .join("orchard-test-worktree-297")
            .to_string_lossy()
            .to_string();
        // Ensure the directory actually exists so path_exists check passes.
        std::fs::create_dir_all(&worktree_path).unwrap();

        // Frozen session.path is stale/non-existent.
        let stale_path = "/tmp/orchard-nonexistent-stale-session-path-xyz".to_string();
        assert!(
            !std::path::Path::new(&stale_path).exists(),
            "stale path must not exist for this test to be valid"
        );

        let session = TmuxSession {
            name: "myrepo_issue297".to_string(),
            path: stale_path,
            attached: false,
            pane_title: None,
            // The live active-pane cwd points to the real worktree.
            active_pane_cwd: Some(worktree_path.clone()),
        };

        let worktrees = vec![make_worktree(&worktree_path, "issue297/fix")];
        let report = diagnose(&HealInput {
            sessions: &[session],
            worktrees: &worktrees,
            ..Default::default()
        });

        let bad = report.findings.iter().find(|f| {
            matches!(
                f.category,
                HealCategory::DeadSessionDirectory | HealCategory::OrphanedSession
            )
        });
        assert!(
            bad.is_none(),
            "session with live active-pane cwd matching a worktree must be healthy, \
             but got finding: {:?}",
            bad
        );
    }

    // -----------------------------------------------------------------------
    // HealReport helpers
    // -----------------------------------------------------------------------

    #[test]
    fn heal_report_is_all_ok_with_no_findings() {
        let report = HealReport { findings: vec![] };
        assert!(report.is_all_ok());
    }

    #[test]
    fn heal_report_is_not_all_ok_with_warning_finding() {
        let report = HealReport {
            findings: vec![HealFinding {
                category: HealCategory::OrphanedSession,
                severity: Severity::Warning,
                message: "orphaned".to_string(),
                action: HealAction::KillSession("orphan".to_string()),
                is_self: false,
            }],
        };
        assert!(!report.is_all_ok());
    }

    // -----------------------------------------------------------------------
    // Self-protection: is_self flag (#361)
    // -----------------------------------------------------------------------

    #[test]
    fn diagnose_marks_kill_session_finding_as_is_self_when_name_matches_current_session() {
        // Session "orchardist" at /tmp — path exists but no matching worktree
        // → OrphanedSession with KillSession("orchardist").
        let sessions = vec![make_session("orchardist", "/tmp")];
        let worktrees: Vec<HealWorktree> = vec![];

        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &worktrees,
            current_session: Some("orchardist"),
            ..Default::default()
        });

        let finding = report
            .findings
            .iter()
            .find(|f| matches!(&f.action, HealAction::KillSession(n) if n == "orchardist"));
        assert!(
            finding.is_some(),
            "should produce a KillSession(\"orchardist\") finding"
        );
        assert!(
            finding.unwrap().is_self,
            "KillSession finding for current session must have is_self == true"
        );
    }

    #[test]
    fn diagnose_does_not_mark_non_matching_kill_session_findings_as_is_self() {
        // Two sessions at /tmp, no worktrees → two OrphanedSession findings.
        let sessions = vec![
            make_session("orchardist", "/tmp"),
            make_session("another", "/tmp"),
        ];
        let worktrees: Vec<HealWorktree> = vec![];

        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &worktrees,
            current_session: Some("orchardist"),
            ..Default::default()
        });

        let orchardist_finding = report
            .findings
            .iter()
            .find(|f| matches!(&f.action, HealAction::KillSession(n) if n == "orchardist"));
        let another_finding = report
            .findings
            .iter()
            .find(|f| matches!(&f.action, HealAction::KillSession(n) if n == "another"));

        assert!(
            orchardist_finding.is_some(),
            "should have finding for orchardist"
        );
        assert!(another_finding.is_some(), "should have finding for another");

        assert!(
            orchardist_finding.unwrap().is_self,
            "orchardist finding must have is_self == true"
        );
        assert!(
            !another_finding.unwrap().is_self,
            "another finding must have is_self == false"
        );
    }

    #[test]
    fn diagnose_with_current_session_none_never_sets_is_self_true() {
        // Session "orchardist" at /tmp, no worktrees → KillSession, current_session = None.
        let sessions = vec![make_session("orchardist", "/tmp")];
        let worktrees: Vec<HealWorktree> = vec![];

        let report = diagnose(&HealInput {
            sessions: &sessions,
            worktrees: &worktrees,
            ..Default::default()
        });

        for finding in &report.findings {
            assert!(
                !finding.is_self,
                "is_self must be false when current_session is None, but found is_self=true on: {:?}",
                finding
            );
        }
    }

    /// Builds a KillSession finding for `name`.
    fn kill_finding(name: &str, is_self: bool) -> HealFinding {
        HealFinding {
            category: HealCategory::OrphanedSession,
            severity: Severity::Warning,
            message: format!("Session \"{name}\" has no matching worktree"),
            action: HealAction::KillSession(name.to_string()),
            is_self,
        }
    }

    #[test]
    fn apply_fixes_skips_kill_session_for_is_self_finding() {
        let exec = RecordingFixExecutor::new().with_current_session("orchardist");

        let results = apply_fixes_with(&[kill_finding("orchardist", true)], &exec);

        assert_eq!(results.len(), 1);
        assert!(results[0].success, "skip result must be success=true");
        assert_eq!(
            results[0].outcome,
            FixOutcome::SkippedSelf,
            "outcome must be SkippedSelf when is_self gate fires; got message: {}",
            results[0].message
        );
        assert!(results[0].error.is_none(), "skip result must have no error");
        assert!(
            exec.calls().is_empty(),
            "the is_self gate must fire before any effect reaches the executor; got: {:?}",
            exec.calls()
        );
    }

    /// Issue #372: the kill is observed through the injected executor rather
    /// than by shelling out to a real `tmux kill-session`.
    #[test]
    fn apply_fixes_runs_kill_session_when_is_self_is_false() {
        let exec = RecordingFixExecutor::new().with_current_session("host-session");

        let results = apply_fixes_with(&[kill_finding("orphan-session", false)], &exec);

        assert_eq!(results.len(), 1);
        assert_eq!(
            results[0].outcome,
            FixOutcome::Killed,
            "non-self finding must take the Killed path; got message: {}",
            results[0].message
        );
        assert_eq!(
            exec.calls(),
            vec![FixCall::KillSession {
                name: "orphan-session".to_string(),
                current_session: Some("host-session".to_string()),
            }],
            "the kill must be issued once, with the host session passed to the \
             #369 self-kill guard"
        );
    }

    /// The `Failed` outcome is only reachable through the seam: a real
    /// `tmux kill-session` against a missing session still exits `Ok`.
    #[test]
    fn apply_fixes_reports_failed_when_the_kill_errors() {
        let exec = RecordingFixExecutor::new().failing_kill("tmux: no server running");

        let results = apply_fixes_with(&[kill_finding("orphan-session", false)], &exec);

        assert_eq!(results.len(), 1);
        assert_eq!(results[0].outcome, FixOutcome::Failed);
        assert!(!results[0].success);
        assert_eq!(
            results[0].error.as_deref(),
            Some("tmux: no server running"),
            "the executor's error must be surfaced verbatim on FixResult::error"
        );
        assert_eq!(exec.killed_sessions(), vec!["orphan-session"]);
    }

    #[test]
    fn apply_fixes_deletes_files_inside_the_cache_dir_via_the_executor() {
        let path = cache::cache_dir().join("stale.json");
        let path_str = path.to_string_lossy().to_string();
        let exec = RecordingFixExecutor::new();

        let results = apply_fixes_with(
            &[HealFinding {
                category: HealCategory::StaleCache,
                severity: Severity::Warning,
                message: "stale cache".to_string(),
                action: HealAction::DeleteFile(path_str.clone()),
                is_self: false,
            }],
            &exec,
        );

        assert_eq!(results.len(), 1);
        assert_eq!(results[0].outcome, FixOutcome::Deleted);
        assert_eq!(exec.removed_files(), vec![path_str]);
    }

    /// The path guard must reject before the executor is reached — nothing
    /// outside `~/.cache/orchard/` or `/tmp/orchard-*` is ever handed to it.
    #[test]
    fn apply_fixes_does_not_reach_the_executor_for_paths_outside_allowed_dirs() {
        let exec = RecordingFixExecutor::new();

        let results = apply_fixes_with(
            &[HealFinding {
                category: HealCategory::StaleCache,
                severity: Severity::Warning,
                message: "stale cache".to_string(),
                action: HealAction::DeleteFile("/etc/passwd".to_string()),
                is_self: false,
            }],
            &exec,
        );

        assert_eq!(results.len(), 1);
        assert_eq!(results[0].outcome, FixOutcome::Skipped);
        assert!(!results[0].success);
        assert!(
            exec.calls().is_empty(),
            "the path guard must fire before any effect reaches the executor; got: {:?}",
            exec.calls()
        );
    }

    #[test]
    fn format_report_annotates_is_self_findings_with_skipped_self() {
        let report = HealReport {
            findings: vec![HealFinding {
                category: HealCategory::OrphanedSession,
                severity: Severity::Warning,
                message: "Session \"orchardist\" has no matching worktree (path: /tmp)".to_string(),
                action: HealAction::KillSession("orchardist".to_string()),
                is_self: true,
            }],
        };
        let text = format_report(&report, None);
        assert!(
            text.contains("skipped (self)"),
            "format_report must annotate is_self findings with 'skipped (self)': {text}"
        );
    }

    #[test]
    fn format_report_leaves_non_is_self_kill_session_findings_unchanged() {
        let report = HealReport {
            findings: vec![HealFinding {
                category: HealCategory::OrphanedSession,
                severity: Severity::Warning,
                message: "Session \"orchardist\" has no matching worktree (path: /tmp)".to_string(),
                action: HealAction::KillSession("orchardist".to_string()),
                is_self: false,
            }],
        };
        let text = format_report(&report, None);
        assert!(
            !text.contains("skipped (self)"),
            "format_report must not annotate non-is_self findings: {text}"
        );
    }

    // -----------------------------------------------------------------------
    // detect_self_error (#361)
    // -----------------------------------------------------------------------

    #[test]
    fn detect_self_error_returns_finding_for_dead_session_directory_self() {
        // One DeadSessionDirectory finding with is_self=true, severity Error.
        let finding = HealFinding {
            category: HealCategory::DeadSessionDirectory,
            severity: Severity::Error,
            message: "Session \"orchardist\" points to non-existent path".to_string(),
            action: HealAction::KillSession("orchardist".to_string()),
            is_self: true,
        };
        let report = HealReport {
            findings: vec![finding.clone()],
        };

        let result = detect_self_error(&report);
        assert!(
            result.is_some(),
            "should return Some for self + Error finding"
        );
        // Confirm it is the same finding (by matching the message).
        assert_eq!(result.unwrap().message, finding.message);
    }

    #[test]
    fn detect_self_error_returns_none_when_self_finding_is_warning_severity() {
        // is_self=true but severity is Warning — must return None.
        let finding = HealFinding {
            category: HealCategory::OrphanedSession,
            severity: Severity::Warning,
            message: "Session has no matching worktree".to_string(),
            action: HealAction::KillSession("orchardist".to_string()),
            is_self: true,
        };
        let report = HealReport {
            findings: vec![finding],
        };

        let result = detect_self_error(&report);
        assert!(
            result.is_none(),
            "Warning-severity self finding must not trigger detect_self_error"
        );
    }

    #[test]
    fn detect_self_error_returns_none_when_no_self_findings() {
        // Findings exist but none has is_self=true.
        let finding = HealFinding {
            category: HealCategory::DeadSessionDirectory,
            severity: Severity::Error,
            message: "Session points to non-existent path".to_string(),
            action: HealAction::KillSession("other-session".to_string()),
            is_self: false,
        };
        let report = HealReport {
            findings: vec![finding],
        };

        let result = detect_self_error(&report);
        assert!(
            result.is_none(),
            "no is_self findings → detect_self_error must return None"
        );
    }

    #[test]
    fn detect_self_error_skips_non_self_error_findings() {
        // Severity::Error but is_self=false — must return None.
        let finding = HealFinding {
            category: HealCategory::DeadSessionDirectory,
            severity: Severity::Error,
            message: "Other session dead".to_string(),
            action: HealAction::KillSession("other".to_string()),
            is_self: false,
        };
        let report = HealReport {
            findings: vec![finding],
        };

        let result = detect_self_error(&report);
        assert!(
            result.is_none(),
            "Error severity without is_self must not trigger detect_self_error"
        );
    }
}
