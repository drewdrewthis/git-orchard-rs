/// Integration tests for the `orchard heal` command.
///
/// These tests exercise the `heal::diagnose` pure function with realistic inputs,
/// corresponding to acceptance criteria from `specs/features/orchard-heal.feature`.
use orchard::heal::{
    FixOutcome, HealAction, HealCategory, HealInput, HealWorktree, RecordingFixExecutor, Severity,
    apply_fixes_with, detect_self_error, diagnose, format_report,
};
use orchard::types::TmuxSession;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

fn session(name: &str, path: &str) -> TmuxSession {
    TmuxSession {
        name: name.to_string(),
        path: path.to_string(),
        attached: false,
        pane_title: None,
        active_pane_cwd: None,
    }
}

fn worktree(path: &str, branch: &str) -> HealWorktree {
    HealWorktree {
        path: path.to_string(),
        branch: branch.to_string(),
        expected_session_name: None,
        pr_state: None,
        pr_number: None,
        issue_state: None,
    }
}

// ---------------------------------------------------------------------------
// Dry-run: reports what it would do without making changes
// ---------------------------------------------------------------------------

/// Scenario: Dry run reports orphaned session as a warning. No sessions are
/// killed or files deleted.
#[test]
fn dry_run_reports_findings_without_applying_fixes() {
    let sessions = vec![session("myrepo_old-feature", "/tmp")]; // /tmp exists but no matching wt
    let worktrees = vec![worktree("/workspace/main", "main")];

    let report = diagnose(&HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        ..Default::default()
    });

    // Orphaned session finding.
    let orphan = report
        .findings
        .iter()
        .find(|f| f.category == HealCategory::OrphanedSession);
    assert!(orphan.is_some(), "should find orphaned session");
    assert_eq!(orphan.unwrap().severity, Severity::Warning);

    // Suggest fix in report.
    let text = format_report(&report, None);
    assert!(
        text.contains("orchard heal --fix"),
        "dry run should suggest --fix: {text}"
    );
}

// ---------------------------------------------------------------------------
// All-healthy scenario
// ---------------------------------------------------------------------------

/// Scenario: When everything matches, no findings are generated and no --fix is suggested.
#[test]
fn all_healthy_when_sessions_match_worktrees() {
    // Use a real path that exists on disk so the directory-existence check passes.
    let tmp = std::env::temp_dir().to_string_lossy().to_string();
    let sessions = vec![session("myrepo_main", &tmp)];
    let worktrees = vec![worktree(&tmp, "main")];

    let report = diagnose(&HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        ..Default::default()
    });

    assert_eq!(report.findings.len(), 0, "should have no findings");
    assert!(report.is_all_ok());

    let text = format_report(&report, None);
    assert!(
        !text.contains("orchard heal --fix"),
        "should not suggest --fix when healthy"
    );
}

// ---------------------------------------------------------------------------
// Orphaned tmux sessions
// ---------------------------------------------------------------------------

/// Scenario: tmux session with no matching worktree path is flagged.
#[test]
fn orphaned_session_detected_when_path_exists_but_no_worktree_matches() {
    // /tmp always exists but is not a worktree.
    let sessions = vec![session("myrepo_old-feature", "/tmp")];
    let worktrees = vec![worktree("/workspace/other", "main")];

    let report = diagnose(&HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        ..Default::default()
    });

    let finding = report
        .findings
        .iter()
        .find(|f| f.category == HealCategory::OrphanedSession);
    assert!(
        finding.is_some(),
        "should detect orphaned session: {:?}",
        report.findings
    );
    assert_eq!(finding.unwrap().severity, Severity::Warning);
    assert!(
        matches!(&finding.unwrap().action, HealAction::KillSession(n) if n == "myrepo_old-feature")
    );
}

// ---------------------------------------------------------------------------
// Dead session directory
// ---------------------------------------------------------------------------

/// Scenario: Session pointing to a non-existent path gets an Error-severity finding.
#[test]
fn dead_session_directory_detected_for_nonexistent_path() {
    let sessions = vec![session(
        "myrepo_gone",
        "/tmp/nonexistent-path-xyz-heal-test",
    )];
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
    assert!(finding.is_some(), "should detect dead session directory");
    assert_eq!(finding.unwrap().severity, Severity::Error);
    assert!(matches!(&finding.unwrap().action, HealAction::KillSession(n) if n == "myrepo_gone"));
}

// ---------------------------------------------------------------------------
// Stale cache files
// ---------------------------------------------------------------------------

/// Scenario: Cache file for unknown repo slug is flagged.
#[test]
fn stale_cache_file_flagged_for_unknown_repo() {
    let cache_files = vec!["ghost_repo_issues.json".to_string()];
    let known_slugs = vec!["owner/known-project".to_string()];

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
    assert!(finding.unwrap().message.contains("ghost_repo_issues.json"));
}

/// Scenario: Cache file for a known repo slug is NOT flagged.
#[test]
fn no_stale_cache_for_known_repo() {
    let cache_files = vec!["owner_myrepo_issues.json".to_string()];
    let known_slugs = vec!["owner/myrepo".to_string()];

    let report = diagnose(&HealInput {
        cache_files: &cache_files,
        known_repo_slugs: &known_slugs,
        ..Default::default()
    });

    let stale = report
        .findings
        .iter()
        .find(|f| f.category == HealCategory::StaleCache);
    assert!(stale.is_none(), "known repo cache should not be flagged");
}

// ---------------------------------------------------------------------------
// Merged/closed PR worktrees
// ---------------------------------------------------------------------------

/// Scenario: Worktree whose PR is merged is flagged (not auto-deleted).
#[test]
fn merged_pr_worktree_flagged_for_manual_cleanup() {
    let mut wt = worktree(".worktrees/issue3-tests", "issue3/tests");
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
    assert!(finding.is_some(), "should flag merged PR worktree");
    assert!(finding.unwrap().message.contains("PR #12 merged"));
    assert!(matches!(
        finding.unwrap().action,
        HealAction::FlagForCleanup(_)
    ));
}

/// Scenario: Worktree with closed PR is flagged.
#[test]
fn closed_pr_worktree_flagged() {
    let mut wt = worktree(".worktrees/issue5-fix", "issue5/fix");
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
    assert!(finding.is_some(), "should flag closed PR worktree");
}

/// Scenario: --fix does NOT delete worktrees for merged PRs.
#[test]
fn fix_does_not_auto_delete_merged_pr_worktrees() {
    let mut wt = worktree(".worktrees/issue3-tests", "issue3/tests");
    wt.pr_state = Some("merged".to_string());
    wt.pr_number = Some(12);

    let report = diagnose(&HealInput {
        worktrees: &[wt],
        ..Default::default()
    });
    let exec = RecordingFixExecutor::new();
    let fix_results = apply_fixes_with(&report.findings, &exec);

    // All results should be FlagForCleanup (not KillSession or DeleteFile).
    for r in &fix_results {
        assert_eq!(
            r.outcome,
            FixOutcome::Flagged,
            "merged PR worktree should only be flagged, not deleted: {}",
            r.message
        );
    }
    assert!(
        exec.calls().is_empty(),
        "no tmux kill or file delete may be issued for a merged-PR worktree; got: {:?}",
        exec.calls()
    );
}

// ---------------------------------------------------------------------------
// Closed issue worktrees
// ---------------------------------------------------------------------------

/// Scenario: Worktree whose linked issue is closed (and no PR) is flagged.
#[test]
fn closed_issue_worktree_flagged_when_no_pr() {
    let mut wt = worktree(".worktrees/issue8-refactor", "issue8/refactor");
    wt.issue_state = Some("closed".to_string());

    let report = diagnose(&HealInput {
        worktrees: &[wt],
        ..Default::default()
    });

    let finding = report
        .findings
        .iter()
        .find(|f| f.category == HealCategory::ClosedIssueWorktree);
    assert!(
        finding.is_some(),
        "should flag closed-issue worktree: {:?}",
        report.findings
    );
}

// ---------------------------------------------------------------------------
// Session naming mismatch
// ---------------------------------------------------------------------------

/// Scenario: Session whose name doesn't match the expected convention is warned.
#[test]
fn session_naming_mismatch_detected() {
    let sessions = vec![session("wrong-name", "/workspace/feature-login")];
    let mut wt = worktree("/workspace/feature-login", "feature/login");
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
    assert!(finding.is_some(), "should detect naming mismatch");
    assert!(finding.unwrap().message.contains("myrepo_feature-login"));
    assert!(finding.unwrap().message.contains("wrong-name"));
}

// ---------------------------------------------------------------------------
// Multiple sessions per worktree
// ---------------------------------------------------------------------------

/// Scenario: Two sessions pointing to the same worktree path triggers a warning.
#[test]
fn multiple_sessions_per_worktree_detected() {
    let sessions = vec![
        session("myrepo_issue10-api", "/workspace/issue10-api"),
        session("extra-session", "/workspace/issue10-api"),
    ];
    let wt = worktree("/workspace/issue10-api", "issue10/api");

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

// ---------------------------------------------------------------------------
// Report format
// ---------------------------------------------------------------------------

/// Scenario: Report uses structured output with icons.
#[test]
fn report_format_includes_icons() {
    let sessions = vec![session("orphan", "/tmp")]; // /tmp is a real path but not a worktree
    let worktrees = vec![];

    let report = diagnose(&HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        ..Default::default()
    });
    let text = format_report(&report, None);

    // Should contain at least one icon character.
    let has_icon =
        text.contains('\u{2713}') || text.contains('\u{26a0}') || text.contains('\u{2716}');
    assert!(has_icon, "report should contain icon characters: {text}");
}

/// Scenario: JSON output contains a "findings" array with required fields.
#[test]
fn json_output_contains_findings_array() {
    let sessions = vec![session("orphan", "/tmp")];
    let worktrees = vec![];

    let report = diagnose(&HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        ..Default::default()
    });
    let json_str = serde_json::to_string(&report).expect("serialize report");
    let json: serde_json::Value = serde_json::from_str(&json_str).expect("parse json");

    let findings = json.get("findings").expect("should have findings field");
    assert!(findings.is_array(), "findings should be an array");

    let arr = findings.as_array().unwrap();
    if !arr.is_empty() {
        let first = &arr[0];
        assert!(
            first.get("category").is_some(),
            "finding should have category"
        );
        assert!(
            first.get("severity").is_some(),
            "finding should have severity"
        );
        assert!(
            first.get("message").is_some(),
            "finding should have message"
        );
        assert!(first.get("action").is_some(), "finding should have action");
    }
}

// ---------------------------------------------------------------------------
// Regression: must never kill self (#361)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Self-protection: outside tmux (#361)
// ---------------------------------------------------------------------------

/// When current_session is None (running outside tmux), no self-protection is
/// applied: the OrphanedSession finding has is_self == false, and apply_fixes
/// goes through the kill path (not the skip path) for an otherwise-applicable
/// KillSession action.
///
/// The kill is observed through a recording executor (issue #372), so no real
/// tmux session is touched.
#[test]
fn outside_tmux_no_self_protection_applied() {
    let session_name = "orchardist-test-issue361-orphan";

    // Session at /tmp — path exists but no matching worktree → OrphanedSession.
    let sessions = vec![session(session_name, "/tmp")];
    let worktrees: Vec<HealWorktree> = vec![];

    // current_session = None simulates running outside tmux.
    let report = diagnose(&HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        ..Default::default()
    });

    let finding = report
        .findings
        .iter()
        .find(|f| f.category == HealCategory::OrphanedSession);
    assert!(
        finding.is_some(),
        "should produce an OrphanedSession finding: {:?}",
        report.findings
    );
    assert!(
        !finding.unwrap().is_self,
        "is_self must be false when current_session is None"
    );

    // apply_fixes must go through the kill path, not skip.
    let exec = RecordingFixExecutor::new();
    let fix_results = apply_fixes_with(&report.findings, &exec);
    assert!(
        fix_results.iter().any(|r| r.outcome == FixOutcome::Killed),
        "apply_fixes must attempt kill (not skip) when is_self=false; got: {:?}",
        fix_results.iter().map(|r| r.outcome).collect::<Vec<_>>()
    );
    assert_eq!(
        exec.killed_sessions(),
        vec![session_name],
        "the orphan session must be the one and only kill issued"
    );
}

/// Regression for issue #361: heal --fix from inside a tmux session must not kill the invoking session.
#[test]
fn regression_heal_must_never_kill_invoking_session() {
    // Build a TmuxSession named "orchardist" pointing to /tmp.
    // /tmp exists on disk so we don't trip the dead-path (DeadSessionDirectory) branch,
    // but there is no matching worktree → this produces an OrphanedSession finding
    // with action KillSession("orchardist").
    let sessions = vec![session("orchardist", "/tmp")];
    let worktrees: Vec<orchard::heal::HealWorktree> = vec![];

    // Pass `Some("orchardist")` as the `current_session` field so the diagnose
    // self-protection branch fires.
    let report = diagnose(&HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        current_session: Some("orchardist"),
        ..Default::default()
    });

    let exec = RecordingFixExecutor::new().with_current_session("orchardist");
    let fix_results = apply_fixes_with(&report.findings, &exec);

    // The invoking session must NOT be killed — asserted against the effects
    // the executor actually received, not against a display string (#372).
    assert!(
        exec.calls().is_empty(),
        "heal --fix must issue no effect at all for the invoking session \"orchardist\"; got: {:?}",
        exec.calls()
    );

    // There must be at least one skip result for the invoking session.
    assert!(
        fix_results
            .iter()
            .any(|r| r.outcome == FixOutcome::SkippedSelf),
        "heal --fix must emit a skip result for the invoking session \"orchardist\"; got: {:?}",
        fix_results.iter().map(|r| r.outcome).collect::<Vec<_>>()
    );
}

// ---------------------------------------------------------------------------
// Self-protection: format_report annotates is_self findings (#361)
// ---------------------------------------------------------------------------

/// AC #3 (dry-run): format_report marks the invoking session as "skipped (self)".
#[test]
fn format_report_marks_invoking_session_as_skipped() {
    // Session "orchardist" at /tmp — path exists but no matching worktree
    // → OrphanedSession with KillSession("orchardist") and is_self=true.
    let sessions = vec![session("orchardist", "/tmp")];
    let worktrees: Vec<orchard::heal::HealWorktree> = vec![];

    let report = diagnose(&HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        current_session: Some("orchardist"),
        ..Default::default()
    });
    let text = format_report(&report, None);

    assert!(
        text.contains("skipped (self)"),
        "format_report must annotate the invoking session as 'skipped (self)': {text}"
    );
    assert!(
        text.contains("orchardist"),
        "format_report output must mention the session name 'orchardist': {text}"
    );
}

/// AC #3 (JSON): the is_self field is serialized and exposed for the invoking session finding.
#[test]
fn json_output_exposes_is_self_field() {
    let sessions = vec![session("orchardist", "/tmp")];
    let worktrees: Vec<orchard::heal::HealWorktree> = vec![];

    let report = diagnose(&HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        current_session: Some("orchardist"),
        ..Default::default()
    });
    let json_str = serde_json::to_string(&report).expect("serialize report");
    let json: serde_json::Value = serde_json::from_str(&json_str).expect("parse json");

    let findings = json
        .get("findings")
        .expect("should have findings field")
        .as_array()
        .expect("findings should be an array");

    // Find the finding whose action targets "orchardist".
    let orchardist_finding = findings.iter().find(|f| {
        f.get("action")
            .and_then(|a| a.get("value"))
            .and_then(|v| v.as_str())
            .map(|s| s == "orchardist")
            .unwrap_or(false)
    });

    assert!(
        orchardist_finding.is_some(),
        "should find a finding targeting orchardist in JSON: {json_str}"
    );
    assert_eq!(
        orchardist_finding.unwrap().get("is_self"),
        Some(&serde_json::Value::Bool(true)),
        "is_self must be true for the invoking session finding: {json_str}"
    );
}

// Regression: must never kill self (#361) — full coverage

/// Full pipeline test: the invoking session is skipped while unrelated orphans are
/// still killed.
///
/// Proves that self-protection isolates only the invoking session; other orphaned
/// sessions that don't match the current session name still go through the kill path.
#[test]
fn regression_full_pipeline_from_inside_named_tmux_session_never_kills_self() {
    // Both sessions point to /tmp (exists but not a worktree) → two OrphanedSession findings.
    let orphan_name = "myrepo_old-feature-test-issue361";
    let sessions = vec![session("orchardist", "/tmp"), session(orphan_name, "/tmp")];
    let worktrees: Vec<HealWorktree> = vec![];

    let report = diagnose(&HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        current_session: Some("orchardist"),
        ..Default::default()
    });
    let exec = RecordingFixExecutor::new().with_current_session("orchardist");
    let fix_results = apply_fixes_with(&report.findings, &exec);

    // Self must be skipped, not killed.
    assert!(
        fix_results
            .iter()
            .any(|r| r.outcome == FixOutcome::SkippedSelf),
        "must emit a skip result for the invoking session \"orchardist\"; got: {:?}",
        fix_results.iter().map(|r| r.outcome).collect::<Vec<_>>()
    );

    // Self-protection isolates only the invoking session: the non-self orphan
    // is the one and only kill that reaches the executor.
    assert_eq!(
        exec.killed_sessions(),
        vec![orphan_name],
        "exactly the non-self orphan must be killed; got: {:?}",
        exec.calls()
    );
}

/// Sister windows inside the same tmux session share the `#S` session name.
/// `current_session_name()` returns the session name regardless of which window
/// invoked the command, so name-based self-detection covers sister windows automatically.
///
/// This test pins down that `is_self` is computed from the session name match
/// alone — not from pane state or active_pane_cwd. Two scenarios are checked:
///
/// 1. Plain session (no active_pane_cwd).
/// 2. Session with active_pane_cwd set (simulating a sister window that has cd'd elsewhere).
///
/// Both must produce `is_self == true`.
#[test]
fn regression_sister_window_of_invoking_session_is_still_treated_as_self() {
    use orchard::types::TmuxSession;

    // Scenario 1: plain session — no active_pane_cwd.
    {
        let sessions = vec![TmuxSession {
            name: "orchardist".to_string(),
            path: "/tmp".to_string(),
            attached: false,
            pane_title: None,
            active_pane_cwd: None,
        }];
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
            .find(|f| f.category == HealCategory::OrphanedSession);
        assert!(
            finding.is_some(),
            "scenario 1: should produce an OrphanedSession finding"
        );
        assert!(
            finding.unwrap().is_self,
            "scenario 1 (no active_pane_cwd): is_self must be true when session name matches current_session"
        );
    }

    // Scenario 2: session with active_pane_cwd set to an existing path — simulates a
    // sister window that has cd'd to a different (but real) directory. The session name
    // still matches current_session, so is_self must still be true regardless of the
    // active pane path. We use /tmp so the path-exists check passes and we stay in the
    // OrphanedSession branch (no matching worktree).
    {
        let sessions = vec![TmuxSession {
            name: "orchardist".to_string(),
            path: "/tmp".to_string(),
            attached: false,
            pane_title: None,
            active_pane_cwd: Some("/tmp".to_string()),
        }];
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
            .find(|f| f.category == HealCategory::OrphanedSession);
        assert!(
            finding.is_some(),
            "scenario 2: should produce an OrphanedSession finding"
        );
        assert!(
            finding.unwrap().is_self,
            "scenario 2 (with active_pane_cwd): is_self must be true — sister windows share \
             the session name, so name-based self-detection covers them automatically"
        );
    }
}

/// Negative case for AC #2: only Error-severity self findings trigger the abort path.
/// A Warning-severity OrphanedSession with is_self=true must NOT cause detect_self_error
/// to return Some — that would abort heal for a normally "unregistered standalone session"
/// classification, which is overly aggressive.
#[test]
fn regression_warning_severity_self_does_not_trigger_abort_path() {
    // Session "orchardist" at /tmp — real path → OrphanedSession (Warning severity).
    let sessions = vec![session("orchardist", "/tmp")];
    let worktrees: Vec<HealWorktree> = vec![];

    let report = diagnose(&HealInput {
        sessions: &sessions,
        worktrees: &worktrees,
        current_session: Some("orchardist"),
        ..Default::default()
    });

    // Confirm we actually got a self finding at Warning severity.
    let self_finding = report
        .findings
        .iter()
        .find(|f| f.is_self && f.category == HealCategory::OrphanedSession);
    assert!(
        self_finding.is_some(),
        "test precondition: should have an OrphanedSession finding with is_self=true"
    );
    assert_eq!(
        self_finding.unwrap().severity,
        Severity::Warning,
        "test precondition: the self finding must be Warning severity (not Error)"
    );

    // The abort gate must NOT fire for Warning-severity self findings.
    let abort = detect_self_error(&report);
    assert!(
        abort.is_none(),
        "detect_self_error must return None for Warning-severity self finding; \
         only Error-severity self findings should abort the heal run"
    );
}
