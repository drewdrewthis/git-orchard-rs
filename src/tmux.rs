use std::process::Command;

use anyhow::{Context, Result};

use crate::logger::LOG;
use crate::types::{PrInfo, SwitchToSessionOptions, TmuxSession};
use crate::types::resolve_pr_status;

/// Lists all active tmux sessions. Returns an empty vec when tmux is not running.
pub fn list_tmux_sessions() -> Vec<TmuxSession> {
    let out = Command::new("tmux")
        .args([
            "list-sessions",
            "-F",
            "#{session_name}\t#{session_path}\t#{session_attached}",
        ])
        .output();

    let output = match out {
        Ok(o) if o.status.success() => o.stdout,
        _ => return Vec::new(),
    };

    let text = String::from_utf8_lossy(&output);
    let mut sessions = Vec::new();

    for line in text.trim().lines() {
        if line.is_empty() {
            continue;
        }
        let parts: Vec<&str> = line.splitn(3, '\t').collect();
        if parts.len() != 3 {
            continue;
        }
        sessions.push(TmuxSession {
            name: parts[0].to_string(),
            path: parts[1].to_string(),
            attached: parts[2] == "1",
            pane_title: None,
        });
    }

    // Fetch pane titles for all sessions in one call (pane index 0 only).
    let pane_out = Command::new("tmux")
        .args(["list-panes", "-a", "-F", "#{session_name}\t#{pane_index}\t#{pane_title}"])
        .output();

    if let Ok(o) = pane_out {
        if o.status.success() {
            let pane_text = String::from_utf8_lossy(&o.stdout);
            for line in pane_text.trim().lines() {
                let parts: Vec<&str> = line.splitn(3, '\t').collect();
                if parts.len() == 3 && parts[1] == "0" {
                    if let Some(session) = sessions.iter_mut().find(|s| s.name == parts[0]) {
                        session.pane_title = Some(parts[2].to_string());
                    }
                }
            }
        }
    }

    LOG.info(&format!("listTmuxSessions: {} sessions", sessions.len()));
    sessions
}

/// Finds the tmux session associated with a worktree.
/// Matches by session path first, then by session name using several fallback patterns.
pub fn find_session_for_worktree<'a>(
    sessions: &'a [TmuxSession],
    path: &str,
    branch: Option<&str>,
) -> Option<&'a TmuxSession> {
    // Highest priority: exact session path match.
    if let Some(s) = sessions.iter().find(|s| s.path == path) {
        return Some(s);
    }

    let dir_name = std::path::Path::new(path)
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or("");

    let branch_slug = branch.map(|b| b.replace('/', "-"));

    for s in sessions {
        let name_suffix = {
            let underscore = s.name.rfind('_');
            let colon = s.name.rfind(':');
            match (underscore, colon) {
                (None, None) => s.name.as_str(),
                (Some(u), None) => &s.name[u + 1..],
                (None, Some(c)) => &s.name[c + 1..],
                (Some(u), Some(c)) => &s.name[u.max(c) + 1..],
            }
        };

        if s.name == dir_name || name_suffix == dir_name {
            return Some(s);
        }
        if let Some(b) = branch {
            if s.name == b {
                return Some(s);
            }
        }
        if let Some(slug) = &branch_slug {
            if s.name == slug.as_str() || name_suffix == slug.as_str() {
                return Some(s);
            }
        }
    }

    None
}

/// Kills the tmux session with the given name.
pub fn kill_tmux_session(name: &str) -> Result<()> {
    Command::new("tmux")
        .args(["kill-session", "-t", name])
        .status()
        .context("tmux kill-session")?;
    LOG.info(&format!("killTmuxSession: {}", name));
    Ok(())
}

/// Captures the pane content of a tmux session, returning the last `lines` lines.
pub fn capture_pane_content(session: &str, lines: u32) -> Result<String> {
    let lines_arg = format!("-{lines}");
    let out = Command::new("tmux")
        .args(["capture-pane", "-t", session, "-p", "-J", "-S", &lines_arg])
        .output()
        .context("tmux capture-pane")?;

    let text = String::from_utf8_lossy(&out.stdout);
    Ok(text.trim_end_matches('\n').to_string())
}

/// Derives the tmux session name in the format "repoName_branch".
/// Slashes in the branch name are replaced with dashes.
/// Falls back to the last path segment, then "orchard".
pub fn derive_session_name(repo_name: &str, branch: Option<&str>, worktree_path: &str) -> String {
    let suffix = match branch {
        Some(b) => b.replace('/', "-"),
        None => {
            let base = std::path::Path::new(worktree_path)
                .file_name()
                .and_then(|n| n.to_str())
                .unwrap_or("");
            if base.is_empty() || base == "." {
                "orchard".to_string()
            } else {
                base.to_string()
            }
        }
    };
    format!("{repo_name}_{suffix}")
}

/// Creates the tmux session for the given worktree if it does not already exist.
/// Applies orchard status bar style. Does NOT switch the client.
pub fn create_session(opts: &SwitchToSessionOptions) -> Result<()> {
    let exists = Command::new("tmux")
        .args(["has-session", "-t", &opts.session_name])
        .status()
        .map(|s| s.success())
        .unwrap_or(false);

    if !exists {
        Command::new("tmux")
            .args([
                "new-session", "-d",
                "-s", &opts.session_name,
                "-c", &opts.worktree_path,
            ])
            .status()
            .with_context(|| format!("creating session {}", opts.session_name))?;
    }

    LOG.info(&format!(
        "createSession: {} ({})",
        opts.session_name,
        if exists { "existing" } else { "new" }
    ));

    apply_session_style(
        &opts.session_name,
        opts.branch.as_deref(),
        opts.pr.as_ref(),
    )?;

    Ok(())
}

pub(crate) const CHEATSHEET: &str =
    "#[fg=colour8]prefix: ctrl-b | o: orchard | (/): prev/next | %%: split-v | \": split-h | arrows: pane | z: zoom | x: close | d: detach";

/// Applies the orchard status bar style to a tmux session.
pub fn apply_session_style(
    name: &str,
    branch: Option<&str>,
    pr: Option<&PrInfo>,
) -> Result<()> {
    let status_left = format_status_left(branch, pr);
    let t = ["-t", name];

    let opts: &[(&str, &str)] = &[
        ("status", "on"),
        ("status-style", "bg=colour235,fg=colour248"),
        ("status-left-length", "60"),
        ("status-right-length", "150"),
        ("status-left", &status_left),
        ("status-right", CHEATSHEET),
    ];

    for (key, value) in opts {
        Command::new("tmux")
            .arg("set-option")
            .args(t)
            .args([*key, value])
            .status()
            .with_context(|| format!("tmux set-option {key}"))?;
    }

    Ok(())
}


/// Formats the tmux status-left string for an orchard session.
pub fn format_status_left(branch: Option<&str>, pr: Option<&PrInfo>) -> String {
    let branch_label = branch.unwrap_or("detached");
    let mut parts = vec![format!(
        "#[fg=colour2,bold] {branch_label} #[fg=colour248,nobold]"
    )];

    if let Some(pr) = pr {
        let status = resolve_pr_status(pr);
        let display = status.display();
        parts.push(format!("PR#{} {} {}", pr.number, display.icon, display.label));
    }

    parts.join(" ")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::{ChecksStatus, PrInfo, ReviewDecision};

    fn make_sessions() -> Vec<TmuxSession> {
        vec![
            TmuxSession { name: "other_main".into(), path: "/other/path".into(), attached: false, pane_title: None },
            TmuxSession { name: "myrepo_feature-x".into(), path: "/home/user/myrepo-feature-x".into(), attached: false, pane_title: None },
            TmuxSession { name: "orchard".into(), path: "/home/user/orchard".into(), attached: true, pane_title: None },
        ]
    }

    #[test]
    fn find_session_matches_by_path() {
        let sessions = make_sessions();
        let result = find_session_for_worktree(&sessions, "/home/user/myrepo-feature-x", None);
        assert!(result.is_some());
        assert_eq!(result.unwrap().name, "myrepo_feature-x");
    }

    #[test]
    fn find_session_matches_by_branch_slug() {
        let sessions = make_sessions();
        // The session "myrepo_feature-x" suffix after "_" is "feature-x"
        let result = find_session_for_worktree(&sessions, "/no/match", Some("feature/x"));
        assert!(result.is_some());
        assert_eq!(result.unwrap().name, "myrepo_feature-x");
    }

    #[test]
    fn find_session_returns_none_when_no_match() {
        let sessions = make_sessions();
        let result = find_session_for_worktree(&sessions, "/no/match", Some("no-match-branch"));
        assert!(result.is_none());
    }

    #[test]
    fn find_session_matches_by_dir_name() {
        let sessions = make_sessions();
        // "orchard" session name == dir name of path "/home/user/orchard"
        let result = find_session_for_worktree(&sessions, "/different/path/orchard", None);
        assert!(result.is_some());
        assert_eq!(result.unwrap().name, "orchard");
    }

    #[test]
    fn derive_session_name_with_branch() {
        assert_eq!(derive_session_name("myrepo", Some("main"), "/any"), "myrepo_main");
    }

    #[test]
    fn derive_session_name_replaces_slashes() {
        assert_eq!(
            derive_session_name("myrepo", Some("feature/my-work"), "/any"),
            "myrepo_feature-my-work"
        );
    }

    #[test]
    fn derive_session_name_uses_path_when_no_branch() {
        assert_eq!(
            derive_session_name("myrepo", None, "/home/user/my-worktree"),
            "myrepo_my-worktree"
        );
    }

    #[test]
    fn derive_session_name_fallback_to_orchard() {
        assert_eq!(derive_session_name("myrepo", None, "/"), "myrepo_orchard");
    }

    #[test]
    fn format_status_left_detached() {
        let s = format_status_left(None, None);
        assert!(s.contains("detached"));
    }

    #[test]
    fn format_status_left_with_branch() {
        let s = format_status_left(Some("main"), None);
        assert!(s.contains("main"));
        assert!(!s.contains("PR#"));
    }

    #[test]
    fn format_status_left_with_pr() {
        let pr = PrInfo {
            number: 42,
            state: "open".into(),
            title: "My PR".into(),
            url: "https://example.com".into(),
            review_decision: ReviewDecision::None,
            unresolved_threads: 0,
            checks_status: ChecksStatus::Pass,
            has_conflicts: false,
        };
        let s = format_status_left(Some("feat"), Some(&pr));
        assert!(s.contains("PR#42"));
    }
}
