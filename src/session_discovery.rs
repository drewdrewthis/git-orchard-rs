use crate::events;
use crate::state::AppState;
use crate::types::TmuxSession;
use serde_json::Value;
use std::process::Command;

// ---------------------------------------------------------------------------
// Pane-level detail types
// ---------------------------------------------------------------------------

/// Rich detail for a single tmux pane.
#[derive(Debug, Clone)]
pub struct PaneDetail {
    pub pane_id: String,
    pub command: String,
    pub title: String,
    /// `true` when the pane title contains "claude" (case-insensitive).
    pub is_agent: bool,
}

/// Rich detail for a tmux session, including all its panes.
#[derive(Debug, Clone)]
pub struct SessionDetail {
    pub name: String,
    pub panes: Vec<PaneDetail>,
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

/// Reconciles tmux sessions with task state.
///
/// - Binds sessions to tasks when they match by worktree path.
/// - Flags orphaned sessions (no matching task).
/// - Flags dead sessions (in task state but not in tmux).
///
/// Returns `(orphaned_sessions, dead_sessions)` for the TUI to display.
/// - `orphaned_sessions`: names of tmux sessions with no matching task worktree
/// - `dead_sessions`: `(task_id, session_name)` pairs where the session no longer exists in tmux
pub fn reconcile_sessions(
    state: &mut AppState,
    tmux_sessions: &[TmuxSession],
) -> (Vec<String>, Vec<(String, String)>) {
    let mut orphaned: Vec<String> = Vec::new();
    let mut dead: Vec<(String, String)> = Vec::new();

    // Build a set of all live session names for fast lookup.
    let live_names: std::collections::HashSet<&str> =
        tmux_sessions.iter().map(|s| s.name.as_str()).collect();

    // Step 1: bind sessions to tasks by worktree path.
    for session in tmux_sessions {
        let matching_task = state
            .tasks
            .iter_mut()
            .find(|task| task.worktree.as_deref() == Some(session.path.as_str()));

        match matching_task {
            Some(task) => {
                if !task.sessions.contains(&session.name) {
                    task.sessions.push(session.name.clone());
                    events::log_event(
                        "session.bound",
                        &[
                            ("task", Value::String(task.id.clone())),
                            ("session", Value::String(session.name.clone())),
                            ("reason", Value::String("worktree_path_match".to_string())),
                        ],
                    );
                }
            }
            None => {
                // No task owns this session — it is orphaned.
                orphaned.push(session.name.clone());
                events::log_session_orphaned(&session.name, &session.path);
            }
        }
    }

    // Step 2: detect dead sessions (task references a session that is no longer live).
    for task in &state.tasks {
        for session_name in &task.sessions {
            if !live_names.contains(session_name.as_str()) {
                dead.push((task.id.clone(), session_name.clone()));
                events::log_session_dead(&task.id, session_name);
            }
        }
    }

    (orphaned, dead)
}

/// Fetches pane-level detail for a tmux session.
///
/// Uses: `tmux list-panes -t {session} -F "#{pane_id} #{pane_current_command} #{pane_title}"`
pub fn fetch_session_panes(session_name: &str) -> Vec<PaneDetail> {
    let format = "#{pane_id}\t#{pane_current_command}\t#{pane_title}";
    let out = Command::new("tmux")
        .args(["list-panes", "-t", session_name, "-F", format])
        .output();

    let output = match out {
        Ok(o) if o.status.success() => o.stdout,
        _ => return Vec::new(),
    };

    let text = String::from_utf8_lossy(&output);
    let mut panes = Vec::new();

    for line in text.trim().lines() {
        if line.is_empty() {
            continue;
        }
        let parts: Vec<&str> = line.splitn(3, '\t').collect();
        if parts.len() != 3 {
            continue;
        }
        let pane_id = parts[0].to_string();
        let command = parts[1].to_string();
        let title = parts[2].to_string();
        let is_agent = title.to_lowercase().contains("claude");
        panes.push(PaneDetail {
            pane_id,
            command,
            title,
            is_agent,
        });
    }

    panes
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::{AppState, Task, TaskSource, TaskStatus};
    use chrono::Utc;

    fn make_task(id: &str, worktree: Option<&str>, sessions: Vec<&str>) -> Task {
        Task {
            id: id.to_string(),
            title: String::new(),
            source: TaskSource::GithubIssue {
                repo: "owner/repo".to_string(),
                number: 47,
            },
            status: TaskStatus::InProgress,
            priority: 1,
            worktree: worktree.map(|s| s.to_string()),
            sessions: sessions.into_iter().map(|s| s.to_string()).collect(),
            pr: None,
            remote_host: None,
            created_at: Utc::now(),
            updated_at: Utc::now(),
        }
    }

    fn make_session(name: &str, path: &str) -> TmuxSession {
        TmuxSession {
            name: name.to_string(),
            path: path.to_string(),
            attached: false,
            pane_title: None,
        }
    }

    #[test]
    fn bind_session_to_task_by_worktree_path() {
        let mut state = AppState {
            version: 1,
            tasks: vec![make_task("git-orchard-rs#47", Some("/ws/repo-47"), vec![])],
        };
        let sessions = vec![make_session("git-orchard-rs_47_main", "/ws/repo-47")];

        let (orphaned, dead) = reconcile_sessions(&mut state, &sessions);

        assert!(
            state.tasks[0].sessions.contains(&"git-orchard-rs_47_main".to_string()),
            "session should be bound to the task"
        );
        assert!(orphaned.is_empty(), "no orphaned sessions expected");
        assert!(dead.is_empty(), "no dead sessions expected");
    }

    #[test]
    fn does_not_duplicate_already_bound_session() {
        let mut state = AppState {
            version: 1,
            tasks: vec![make_task(
                "git-orchard-rs#47",
                Some("/ws/repo-47"),
                vec!["git-orchard-rs_47_main"],
            )],
        };
        let sessions = vec![make_session("git-orchard-rs_47_main", "/ws/repo-47")];

        reconcile_sessions(&mut state, &sessions);

        assert_eq!(
            state.tasks[0].sessions.len(),
            1,
            "session must not be duplicated"
        );
    }

    #[test]
    fn orphaned_session_detected() {
        let mut state = AppState {
            version: 1,
            tasks: vec![make_task("git-orchard-rs#47", Some("/ws/repo-47"), vec![])],
        };
        // Session path does not match any task's worktree.
        let sessions = vec![make_session("mystery-session", "/tmp/unknown-path")];

        let (orphaned, dead) = reconcile_sessions(&mut state, &sessions);

        assert_eq!(orphaned, vec!["mystery-session"], "mystery-session should be orphaned");
        assert!(dead.is_empty(), "no dead sessions expected");
    }

    #[test]
    fn dead_session_detected() {
        let mut state = AppState {
            version: 1,
            tasks: vec![make_task(
                "git-orchard-rs#47",
                Some("/ws/repo-47"),
                vec!["git-orchard-rs_47_claude"],
            )],
        };
        // Tmux reports no session with that name.
        let sessions: Vec<TmuxSession> = vec![];

        let (orphaned, dead) = reconcile_sessions(&mut state, &sessions);

        assert!(orphaned.is_empty(), "no orphaned sessions expected");
        assert_eq!(
            dead,
            vec![("git-orchard-rs#47".to_string(), "git-orchard-rs_47_claude".to_string())],
            "claude session should be detected as dead"
        );
    }

    fn pane_from_title(title: &str) -> PaneDetail {
        let is_agent = title.to_lowercase().contains("claude");
        PaneDetail {
            pane_id: "%0".to_string(),
            command: "sh".to_string(),
            title: title.to_string(),
            is_agent,
        }
    }

    #[test]
    fn pane_detail_detects_claude_agent() {
        let pane = pane_from_title("Claude Code");
        assert!(pane.is_agent, "pane with title 'Claude Code' should be is_agent = true");
    }

    #[test]
    fn pane_detail_non_agent() {
        let pane = pane_from_title("zsh");
        assert!(!pane.is_agent, "pane with title 'zsh' should be is_agent = false");
    }
}
