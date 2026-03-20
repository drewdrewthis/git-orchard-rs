/// Writes tmux-formatted status text to `~/.local/state/git-orchard/status.txt`
/// after each collector refresh, so the tmux status bar can display orchard state.
use crate::types::{ChecksStatus, Worktree};

/// Counts active worktrees, claude sessions, and failing CI from the worktree list,
/// then formats a tmux status string and writes it atomically to the status file.
///
/// Silently ignores errors from the caller's perspective — callers should log and discard.
pub fn write_status(worktrees: &[Worktree]) -> anyhow::Result<()> {
    let text = format_status(worktrees);
    write_status_file(&text)
}

/// Formats the tmux status string from the given worktrees.
///
/// Returns:
/// - `#[fg=green]🌳 ORCHARD#[fg=default]` when nothing is active
/// - `#[fg=green]🌳 ORCHARD#[fg=default]: N active · ⚡ M claude · ✗ K failing` when active
pub fn format_status(worktrees: &[Worktree]) -> String {
    let active = count_active(worktrees);
    let claude = count_claude(worktrees);
    let failing = count_failing(worktrees);

    let prefix = "#[fg=green]🌳 ORCHARD#[fg=default]";

    if active == 0 {
        return prefix.to_string();
    }

    let mut parts: Vec<String> = vec![format!("{active} active")];
    if claude > 0 {
        parts.push(format!("⚡ {claude} claude"));
    }
    if failing > 0 {
        parts.push(format!("✗ {failing} failing"));
    }

    format!("{}: {}", prefix, parts.join(" · "))
}

fn count_active(worktrees: &[Worktree]) -> usize {
    worktrees.iter().filter(|wt| wt.tmux_session.is_some()).count()
}

fn count_claude(worktrees: &[Worktree]) -> usize {
    worktrees
        .iter()
        .filter(|wt| {
            wt.tmux_pane_title
                .as_deref()
                .map(|t| t.contains("Claude") || t.contains("claude"))
                .unwrap_or(false)
        })
        .count()
}

fn count_failing(worktrees: &[Worktree]) -> usize {
    worktrees
        .iter()
        .filter(|wt| {
            wt.pr
                .as_ref()
                .map(|pr| pr.checks_status == ChecksStatus::Fail)
                .unwrap_or(false)
        })
        .count()
}

fn write_status_file(text: &str) -> anyhow::Result<()> {
    let state_dir = state_dir()?;
    std::fs::create_dir_all(&state_dir)?;

    let status_path = state_dir.join("status.txt");
    // Write atomically: write to a temp file in the same dir, then rename.
    let tmp_path = state_dir.join("status.txt.tmp");
    std::fs::write(&tmp_path, text)?;
    std::fs::rename(&tmp_path, &status_path)?;

    Ok(())
}

fn state_dir() -> anyhow::Result<std::path::PathBuf> {
    let home = dirs::home_dir().ok_or_else(|| anyhow::anyhow!("could not determine home directory"))?;
    Ok(home.join(".local/state/git-orchard"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::{ChecksStatus, PrInfo, ReviewDecision};

    fn make_worktree_with_session(session: &str) -> Worktree {
        Worktree {
            tmux_session: Some(session.to_string()),
            ..Default::default()
        }
    }

    fn make_worktree_with_claude(session: &str, pane_title: &str) -> Worktree {
        Worktree {
            tmux_session: Some(session.to_string()),
            tmux_pane_title: Some(pane_title.to_string()),
            ..Default::default()
        }
    }

    fn make_worktree_failing_ci(session: &str) -> Worktree {
        Worktree {
            tmux_session: Some(session.to_string()),
            pr: Some(PrInfo {
                number: 1,
                state: "open".into(),
                title: String::new(),
                url: String::new(),
                review_decision: ReviewDecision::None,
                unresolved_threads: 0,
                checks_status: ChecksStatus::Fail,
                has_conflicts: false,
            }),
            ..Default::default()
        }
    }

    #[test]
    fn idle_no_active_worktrees() {
        let worktrees = vec![Worktree::default(), Worktree::default()];
        let status = format_status(&worktrees);
        assert_eq!(status, "#[fg=green]🌳 ORCHARD#[fg=default]");
    }

    #[test]
    fn empty_worktrees_shows_idle() {
        let status = format_status(&[]);
        assert_eq!(status, "#[fg=green]🌳 ORCHARD#[fg=default]");
    }

    #[test]
    fn full_summary_three_active_two_claude_one_failing() {
        let worktrees = vec![
            make_worktree_with_claude("sess1", "Claude Code"),
            make_worktree_with_claude("sess2", "claude"),
            make_worktree_failing_ci("sess3"),
        ];
        let status = format_status(&worktrees);
        assert_eq!(
            status,
            "#[fg=green]🌳 ORCHARD#[fg=default]: 3 active · ⚡ 2 claude · ✗ 1 failing"
        );
    }

    #[test]
    fn active_but_no_claude_no_failing() {
        let worktrees = vec![
            make_worktree_with_session("sess1"),
            make_worktree_with_session("sess2"),
        ];
        let status = format_status(&worktrees);
        assert_eq!(
            status,
            "#[fg=green]🌳 ORCHARD#[fg=default]: 2 active"
        );
    }

    #[test]
    fn active_with_claude_no_failing() {
        let worktrees = vec![
            make_worktree_with_claude("sess1", "Claude Code"),
            make_worktree_with_session("sess2"),
        ];
        let status = format_status(&worktrees);
        assert_eq!(
            status,
            "#[fg=green]🌳 ORCHARD#[fg=default]: 2 active · ⚡ 1 claude"
        );
    }

    #[test]
    fn active_with_failing_no_claude() {
        let worktrees = vec![
            make_worktree_failing_ci("sess1"),
            make_worktree_with_session("sess2"),
        ];
        let status = format_status(&worktrees);
        assert_eq!(
            status,
            "#[fg=green]🌳 ORCHARD#[fg=default]: 2 active · ✗ 1 failing"
        );
    }

    #[test]
    fn pane_title_with_uppercase_claude_matches() {
        let worktrees = vec![make_worktree_with_claude("sess1", "Claude Code (3 active)")];
        assert_eq!(count_claude(&worktrees), 1);
    }

    #[test]
    fn pane_title_with_lowercase_claude_matches() {
        let worktrees = vec![make_worktree_with_claude("sess1", "claude 3.7 sonnet")];
        assert_eq!(count_claude(&worktrees), 1);
    }

    #[test]
    fn pane_title_without_claude_does_not_match() {
        let worktrees = vec![make_worktree_with_claude("sess1", "vim")];
        assert_eq!(count_claude(&worktrees), 0);
    }

    #[test]
    fn worktree_without_session_not_counted_as_active() {
        let worktrees = vec![Worktree {
            tmux_pane_title: Some("Claude Code".into()),
            ..Default::default()
        }];
        assert_eq!(count_active(&worktrees), 0);
    }
}
