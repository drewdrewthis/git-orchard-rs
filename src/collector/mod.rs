use std::collections::HashMap;

use crate::config;
use crate::git;
use crate::github;
use crate::logger::LOG;
use crate::remote;
use crate::timed;
use crate::tmux;
use crate::types::{IssueState, PrInfo, TmuxSession, Worktree};

/// Returns the list of local git worktrees, or an empty vec on failure.
pub fn fetch_git_worktrees() -> Vec<Worktree> {
    timed!("phase:git", {
        let trees = git::list_worktrees().unwrap_or_default();
        LOG.info(&format!("worktrees: {} found", trees.len()));
        trees
    })
}

/// Runs tmux session listing and gh-availability check in parallel.
/// Returns `(sessions, gh_ok)`.
pub fn fetch_tmux_and_gh() -> (Vec<TmuxSession>, bool) {
    timed!("phase:tmux+gh", {
        let tmux_handle = std::thread::spawn(tmux::list_tmux_sessions);
        let gh_ok = github::is_gh_available();
        let sessions = tmux_handle.join().unwrap_or_default();
        (sessions, gh_ok)
    })
}

/// Ensures a tmux session exists at the worktree origin (the first entry from
/// `git worktree list`). Skips creation if a session with the derived name already
/// exists. Returns the (possibly augmented) session list.
///
/// Only call this in TUI mode -- JSON mode is read-only.
pub fn ensure_main_session(
    trees: &[Worktree],
    mut sessions: Vec<TmuxSession>,
    error_fn: &dyn Fn(&str),
) -> Vec<TmuxSession> {
    let origin = match trees.first() {
        Some(wt) if !wt.is_bare => wt,
        _ => return sessions,
    };

    let session_name = tmux::derive_main_session_name(&origin.path, origin.branch.as_deref());

    // Idempotent: skip if session name already exists (regardless of path).
    if sessions.iter().any(|s| s.name == session_name) {
        return sessions;
    }

    match tmux::new_detached_session(&session_name, &origin.path) {
        Ok(()) => {
            sessions.push(TmuxSession {
                name: session_name,
                path: origin.path.clone(),
                attached: false,
                pane_title: None,
            });
        }
        Err(e) => {
            error_fn(&format!("Failed to create main session: {e}"));
        }
    }

    sessions
}

/// Merges tmux session data into the worktrees slice.
/// Sets `pr_loading = true` for non-bare worktrees that have a branch and gh available.
pub fn merge_tmux_sessions(
    trees: &[Worktree],
    sessions: &[TmuxSession],
    gh_ok: bool,
) -> Vec<Worktree> {
    trees
        .iter()
        .map(|tree| {
            let session =
                tmux::find_session_for_worktree(sessions, &tree.path, tree.branch.as_deref());
            let mut t = tree.clone();
            t.tmux_session = session.map(|s| s.name.clone());
            t.tmux_attached = session.is_some_and(|s| s.attached);
            t.tmux_pane_title = session.and_then(|s| s.pane_title.clone());
            t.pr_loading = !tree.is_bare && tree.branch.is_some() && gh_ok;
            t
        })
        .collect()
}

/// Fetches basic PR info for the given branch names.
pub fn fetch_pr_basics(branches: &[String]) -> HashMap<String, PrInfo> {
    timed!("phase:pr-basics", {
        let pr_map = github::get_all_prs(branches);
        LOG.info(&format!("PRs: {} found", pr_map.len()));
        pr_map
    })
}

/// Applies a PR map to a worktrees slice. Clears `pr_loading` on all entries.
pub fn apply_prs(base: &[Worktree], pr_map: &HashMap<String, PrInfo>) -> Vec<Worktree> {
    base.iter()
        .map(|tree| {
            let mut t = tree.clone();
            if let Some(branch) = &tree.branch {
                if !tree.is_bare {
                    t.pr = pr_map.get(branch).cloned();
                }
            }
            t.pr_loading = false;
            t
        })
        .collect()
}

/// Fetches detailed PR data (checks, review threads, conflicts) and updates `pr_map` in-place.
pub fn enrich_prs(pr_map: &mut HashMap<String, PrInfo>) {
    timed!("phase:pr-enrich", {
        github::enrich_pr_details(pr_map);
    })
}

/// Collects issue numbers referenced by branches that have no PR, then fetches their states.
pub fn fetch_issue_states(trees: &[Worktree]) -> HashMap<u32, IssueState> {
    let mut numbers: Vec<u32> = Vec::new();
    for tree in trees {
        if tree.pr.is_some() || tree.is_bare {
            continue;
        }
        if let Some(branch) = &tree.branch {
            if let Some(num) = github::extract_issue_number(branch) {
                if !numbers.contains(&num) {
                    numbers.push(num);
                }
            }
        }
    }
    if numbers.is_empty() {
        return HashMap::new();
    }
    github::get_issue_states(&numbers)
}

/// Applies issue state data to a worktrees slice.
/// Skips bare worktrees and worktrees that already have a PR.
pub fn apply_issue_states(
    trees: &[Worktree],
    issue_states: &HashMap<u32, IssueState>,
) -> Vec<Worktree> {
    if issue_states.is_empty() {
        return trees.to_vec();
    }
    trees
        .iter()
        .map(|tree| {
            if tree.pr.is_some() || tree.is_bare {
                return tree.clone();
            }
            if let Some(branch) = &tree.branch {
                if let Some(num) = github::extract_issue_number(branch) {
                    if let Some(&state) = issue_states.get(&num) {
                        let mut t = tree.clone();
                        t.issue_number = Some(num);
                        t.issue_state = Some(state);
                        return t;
                    }
                }
            }
            tree.clone()
        })
        .collect()
}

/// Runs the full data pipeline synchronously and returns all worktrees.
/// Used for `--json` mode.
pub fn collect_worktree_data() -> anyhow::Result<Vec<Worktree>> {
    let trees = fetch_git_worktrees();
    let (sessions, gh_ok) = fetch_tmux_and_gh();
    let with_tmux = merge_tmux_sessions(&trees, &sessions, gh_ok);

    if !gh_ok {
        return Ok(with_tmux);
    }

    let branches: Vec<String> = with_tmux
        .iter()
        .filter(|t| !t.is_bare && t.branch.is_some())
        .filter_map(|t| t.branch.clone())
        .collect();
    let mut pr_map = fetch_pr_basics(&branches);

    let cfg = config::load_config();
    let remote_cfg = cfg.remote.clone();
    let remote_handle = std::thread::spawn(move || {
        remote_cfg
            .as_ref()
            .map(|r| remote::fetch_remote_worktrees(r))
            .unwrap_or_default()
    });

    enrich_prs(&mut pr_map);
    let remote_trees = remote_handle.join().unwrap_or_default();

    let local_with_prs = apply_prs(&with_tmux, &pr_map);
    let remote_with_prs: Vec<Worktree> = remote_trees
        .iter()
        .map(|t| {
            let mut wt = t.clone();
            if let Some(branch) = &t.branch {
                if !t.is_bare {
                    wt.pr = pr_map.get(branch).cloned();
                }
            }
            wt
        })
        .collect();

    let mut all = local_with_prs;
    all.extend(remote_with_prs);

    let issue_states = fetch_issue_states(&all);
    Ok(apply_issue_states(&all, &issue_states))
}

/// Runs the pipeline in stages, calling `update_fn` after each stage so that
/// a TUI can display progressively richer data. Non-fatal errors (e.g. session
/// creation failures) are reported via `error_fn` without aborting the pipeline.
pub fn refresh_worktrees(
    update_fn: &dyn Fn(&[Worktree]),
    error_fn: &dyn Fn(&str),
) -> anyhow::Result<()> {
    // Stage 1: local worktrees appear immediately.
    let trees = fetch_git_worktrees();
    update_fn(&trees);

    // Stage 2: ensure main session exists, then merge tmux sessions.
    let (sessions, gh_ok) = fetch_tmux_and_gh();
    let sessions = ensure_main_session(&trees, sessions, error_fn);
    let with_tmux = merge_tmux_sessions(&trees, &sessions, gh_ok);
    update_fn(&with_tmux);

    if !gh_ok {
        return Ok(());
    }

    // Stage 3: basic PR info.
    let branches: Vec<String> = with_tmux
        .iter()
        .filter(|t| !t.is_bare && t.branch.is_some())
        .filter_map(|t| t.branch.clone())
        .collect();
    let mut pr_map = fetch_pr_basics(&branches);
    let with_prs = apply_prs(&with_tmux, &pr_map);
    update_fn(&with_prs);

    // Stage 4: enrich PRs and fetch remote worktrees in parallel.
    let cfg = config::load_config();
    let remote_cfg = cfg.remote.clone();
    let remote_handle = std::thread::spawn(move || {
        remote_cfg
            .as_ref()
            .map(|r| remote::fetch_remote_worktrees(r))
            .unwrap_or_default()
    });
    enrich_prs(&mut pr_map);
    let remote_trees = remote_handle.join().unwrap_or_default();

    let local_enriched = apply_prs(&with_tmux, &pr_map);
    let remote_enriched: Vec<Worktree> = remote_trees
        .iter()
        .map(|t| {
            let mut wt = t.clone();
            if let Some(branch) = &t.branch {
                if !t.is_bare {
                    wt.pr = pr_map.get(branch).cloned();
                }
            }
            wt
        })
        .collect();

    let mut all = local_enriched;
    all.extend(remote_enriched);

    // Stage 5: issue states.
    let issue_states = fetch_issue_states(&all);
    let final_trees = apply_issue_states(&all, &issue_states);
    update_fn(&final_trees);

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::{ChecksStatus, PrInfo, ReviewDecision, TmuxSession};

    fn bare_worktree(path: &str) -> Worktree {
        Worktree {
            path: path.to_string(),
            is_bare: true,
            ..Default::default()
        }
    }

    fn branched_worktree(path: &str, branch: &str) -> Worktree {
        Worktree {
            path: path.to_string(),
            branch: Some(branch.to_string()),
            ..Default::default()
        }
    }

    fn make_pr(number: u32, _branch: &str) -> PrInfo {
        PrInfo {
            number,
            state: "open".to_string(),
            title: "Test PR".to_string(),
            url: "https://example.com".to_string(),
            review_decision: ReviewDecision::None,
            unresolved_threads: 0,
            checks_status: ChecksStatus::None,
            has_conflicts: false,
        }
    }

    // -----------------------------------------------------------------------
    // merge_tmux_sessions
    // -----------------------------------------------------------------------

    #[test]
    fn merge_tmux_sets_session_name_by_path() {
        let trees = vec![branched_worktree("/home/user/project", "main")];
        let sessions = vec![TmuxSession {
            name: "myrepo_main".to_string(),
            path: "/home/user/project".to_string(),
            attached: false,
            pane_title: None,
        }];
        let result = merge_tmux_sessions(&trees, &sessions, false);
        assert_eq!(result[0].tmux_session.as_deref(), Some("myrepo_main"));
    }

    #[test]
    fn merge_tmux_sets_attached_flag() {
        let trees = vec![branched_worktree("/home/user/project", "main")];
        let sessions = vec![TmuxSession {
            name: "myrepo_main".to_string(),
            path: "/home/user/project".to_string(),
            attached: true,
            pane_title: None,
        }];
        let result = merge_tmux_sessions(&trees, &sessions, true);
        assert!(result[0].tmux_attached);
    }

    #[test]
    fn merge_tmux_sets_pr_loading_when_gh_ok() {
        let trees = vec![branched_worktree("/home/user/project", "main")];
        let result = merge_tmux_sessions(&trees, &[], true);
        assert!(result[0].pr_loading);
    }

    #[test]
    fn merge_tmux_pr_loading_false_when_gh_not_ok() {
        let trees = vec![branched_worktree("/home/user/project", "main")];
        let result = merge_tmux_sessions(&trees, &[], false);
        assert!(!result[0].pr_loading);
    }

    #[test]
    fn merge_tmux_pr_loading_false_for_bare_worktree() {
        let trees = vec![bare_worktree("/home/user/bare.git")];
        let result = merge_tmux_sessions(&trees, &[], true);
        assert!(!result[0].pr_loading);
    }

    #[test]
    fn merge_tmux_no_session_when_no_match() {
        let trees = vec![branched_worktree("/home/user/project", "main")];
        let result = merge_tmux_sessions(&trees, &[], false);
        assert!(result[0].tmux_session.is_none());
        assert!(!result[0].tmux_attached);
    }

    #[test]
    fn merge_tmux_sets_pane_title() {
        let trees = vec![branched_worktree("/home/user/project", "main")];
        let sessions = vec![TmuxSession {
            name: "myrepo_main".to_string(),
            path: "/home/user/project".to_string(),
            attached: false,
            pane_title: Some("\u{2733} Claude Code".to_string()),
        }];
        let result = merge_tmux_sessions(&trees, &sessions, false);
        assert_eq!(result[0].tmux_pane_title.as_deref(), Some("\u{2733} Claude Code"));
    }

    // -----------------------------------------------------------------------
    // apply_prs
    // -----------------------------------------------------------------------

    #[test]
    fn apply_prs_sets_pr_for_matching_branch() {
        let trees = vec![branched_worktree("/home/user/project", "feat/my-feature")];
        let mut pr_map = HashMap::new();
        pr_map.insert("feat/my-feature".to_string(), make_pr(42, "feat/my-feature"));
        let result = apply_prs(&trees, &pr_map);
        assert_eq!(result[0].pr.as_ref().map(|p| p.number), Some(42));
    }

    #[test]
    fn apply_prs_no_pr_when_branch_not_in_map() {
        let trees = vec![branched_worktree("/home/user/project", "main")];
        let result = apply_prs(&trees, &HashMap::new());
        assert!(result[0].pr.is_none());
    }

    #[test]
    fn apply_prs_skips_bare_worktrees() {
        let trees = vec![bare_worktree("/home/user/bare.git")];
        let pr_map = HashMap::new();
        let result = apply_prs(&trees, &pr_map);
        assert!(result[0].pr.is_none());
    }

    #[test]
    fn apply_prs_clears_pr_loading() {
        let mut tree = branched_worktree("/home/user/project", "main");
        tree.pr_loading = true;
        let result = apply_prs(&[tree], &HashMap::new());
        assert!(!result[0].pr_loading);
    }

    // -----------------------------------------------------------------------
    // apply_issue_states
    // -----------------------------------------------------------------------

    #[test]
    fn apply_issue_states_returns_clone_when_map_empty() {
        let trees = vec![branched_worktree("/home/user/project", "feat/issue-200-thing")];
        let result = apply_issue_states(&trees, &HashMap::new());
        assert_eq!(result.len(), 1);
        assert!(result[0].issue_state.is_none());
    }

    #[test]
    fn apply_issue_states_sets_issue_number_and_state() {
        let trees =
            vec![branched_worktree("/home/user/project", "feat/issue-200-my-feature")];
        let mut issue_states = HashMap::new();
        issue_states.insert(200u32, IssueState::Open);
        let result = apply_issue_states(&trees, &issue_states);
        assert_eq!(result[0].issue_number, Some(200));
        assert_eq!(result[0].issue_state, Some(IssueState::Open));
    }

    #[test]
    fn apply_issue_states_skips_worktrees_with_pr() {
        let mut tree = branched_worktree("/home/user/project", "feat/issue-200-my-feature");
        tree.pr = Some(make_pr(1, "feat/issue-200-my-feature"));
        let mut issue_states = HashMap::new();
        issue_states.insert(200u32, IssueState::Open);
        let result = apply_issue_states(&[tree], &issue_states);
        assert!(result[0].issue_number.is_none());
    }

    #[test]
    fn apply_issue_states_skips_bare_worktrees() {
        let tree = bare_worktree("/home/user/bare.git");
        let mut issue_states = HashMap::new();
        issue_states.insert(200u32, IssueState::Closed);
        let result = apply_issue_states(&[tree], &issue_states);
        assert!(result[0].issue_number.is_none());
    }

    #[test]
    fn apply_issue_states_no_match_leaves_tree_unchanged() {
        let tree = branched_worktree("/home/user/project", "main");
        let mut issue_states = HashMap::new();
        issue_states.insert(999u32, IssueState::Closed);
        let result = apply_issue_states(&[tree], &issue_states);
        assert!(result[0].issue_number.is_none());
        assert!(result[0].issue_state.is_none());
    }

    // -----------------------------------------------------------------------
    // ensure_main_session
    // -----------------------------------------------------------------------

    fn noop_error(_msg: &str) {}

    #[test]
    fn ensure_main_session_skips_when_session_already_exists() {
        let trees = vec![branched_worktree("/home/user/myrepo", "main")];
        let sessions = vec![TmuxSession {
            name: "myrepo_main".to_string(),
            path: "/home/user/myrepo".to_string(),
            attached: false,
            pane_title: None,
        }];
        let result = ensure_main_session(&trees, sessions.clone(), &noop_error);
        assert_eq!(result.len(), 1);
        assert_eq!(result[0].name, "myrepo_main");
    }

    #[test]
    fn ensure_main_session_skips_when_session_exists_at_different_path() {
        let trees = vec![branched_worktree("/home/user/myrepo", "main")];
        let sessions = vec![TmuxSession {
            name: "myrepo_main".to_string(),
            path: "/tmp/other-path".to_string(),
            attached: false,
            pane_title: None,
        }];
        let result = ensure_main_session(&trees, sessions.clone(), &noop_error);
        // Should not create a duplicate -- session name already exists
        assert_eq!(result.len(), 1);
        assert_eq!(result[0].name, "myrepo_main");
    }

    #[test]
    fn ensure_main_session_returns_unchanged_when_no_worktrees() {
        let sessions = vec![TmuxSession {
            name: "other".to_string(),
            path: "/other".to_string(),
            attached: false,
            pane_title: None,
        }];
        let result = ensure_main_session(&[], sessions.clone(), &noop_error);
        assert_eq!(result.len(), 1);
    }

    #[test]
    fn ensure_main_session_skips_when_first_worktree_is_bare() {
        let trees = vec![bare_worktree("/home/user/bare.git")];
        let sessions = vec![];
        let result = ensure_main_session(&trees, sessions, &noop_error);
        assert!(result.is_empty());
    }

    #[test]
    fn ensure_main_session_merge_maps_created_session_to_origin() {
        // Simulate the flow: session already in list -> merge maps it to origin worktree
        let trees = vec![
            branched_worktree("/home/user/myrepo", "main"),
            branched_worktree("/home/user/myrepo/.worktrees/feat", "feature/login"),
        ];
        let sessions = vec![TmuxSession {
            name: "myrepo_main".to_string(),
            path: "/home/user/myrepo".to_string(),
            attached: false,
            pane_title: None,
        }];
        let result = merge_tmux_sessions(&trees, &sessions, false);
        assert_eq!(result[0].tmux_session.as_deref(), Some("myrepo_main"));
        assert!(result[1].tmux_session.is_none());
    }

    #[test]
    fn ensure_main_session_preserves_across_refresh() {
        // Same session exists on re-run -- should stay mapped
        let trees = vec![branched_worktree("/home/user/myrepo", "main")];
        let sessions = vec![TmuxSession {
            name: "myrepo_main".to_string(),
            path: "/home/user/myrepo".to_string(),
            attached: false,
            pane_title: None,
        }];
        let result1 = merge_tmux_sessions(&trees, &sessions, false);
        let result2 = merge_tmux_sessions(&trees, &sessions, false);
        assert_eq!(result1[0].tmux_session, result2[0].tmux_session);
    }
}
