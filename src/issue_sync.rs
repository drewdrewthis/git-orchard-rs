use std::process::Command;

use chrono::Utc;

use crate::events;
use crate::state::{AppState, Task, TaskSource, TaskStatus};

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

/// Syncs GitHub issues with the task state.
/// - Creates new tasks for open issues not already in state
/// - Marks tasks as done when their issue is closed
/// Returns true if state was modified (caller should save).
pub fn sync_issues(state: &mut AppState, repo_slug: &str) -> bool {
    let issues = fetch_github_issues(repo_slug);
    sync_issues_with_data(state, repo_slug, &issues)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

/// Fetches open issues from GitHub using `gh issue list`.
/// Returns (number, title, state) tuples where state is "OPEN" or "CLOSED".
fn fetch_github_issues(repo_slug: &str) -> Vec<(u32, String, String)> {
    let out = match Command::new("gh")
        .args([
            "issue",
            "list",
            "--repo",
            repo_slug,
            "--state",
            "all",
            "--limit",
            "100",
            "--json",
            "number,title,state",
        ])
        .output()
    {
        Ok(o) => o,
        Err(_) => return Vec::new(),
    };

    let raws: Vec<serde_json::Value> = match serde_json::from_slice(&out.stdout) {
        Ok(v) => v,
        Err(_) => return Vec::new(),
    };

    raws.into_iter()
        .filter_map(|entry| {
            let number = entry["number"].as_u64()? as u32;
            let title = entry["title"].as_str().unwrap_or("").to_string();
            let state = entry["state"].as_str().unwrap_or("").to_string();
            Some((number, title, state))
        })
        .collect()
}

/// Derives the repo name from a slug like "hopegrace/git-orchard-rs" -> "git-orchard-rs".
fn repo_name_from_slug(repo_slug: &str) -> &str {
    repo_slug.rsplit('/').next().unwrap_or(repo_slug)
}

/// Checks whether state already has a task for the given issue.
fn has_task_for_issue(state: &AppState, repo_slug: &str, number: u32) -> bool {
    state.tasks.iter().any(|t| {
        matches!(
            &t.source,
            TaskSource::GithubIssue { repo, number: n } if repo == repo_slug && *n == number
        )
    })
}

/// Returns a mutable reference to the task for the given issue, if any.
fn find_task_for_issue_mut<'a>(
    state: &'a mut AppState,
    repo_slug: &str,
    number: u32,
) -> Option<&'a mut Task> {
    state.tasks.iter_mut().find(|t| {
        matches!(
            &t.source,
            TaskSource::GithubIssue { repo, number: n } if repo == repo_slug && *n == number
        )
    })
}

/// Testable version that takes issues directly instead of calling `gh`.
fn sync_issues_with_data(
    state: &mut AppState,
    repo_slug: &str,
    issues: &[(u32, String, String)],
) -> bool {
    let mut modified = false;
    let repo_name = repo_name_from_slug(repo_slug);

    // Backfill titles for existing tasks created before we stored titles.
    for (number, title, _) in issues {
        if !title.is_empty() {
            if let Some(task) = find_task_for_issue_mut(state, repo_slug, *number) {
                if task.title.is_empty() {
                    task.title = title.clone();
                    modified = true;
                }
            }
        }
    }

    for (number, title, issue_state) in issues {
        if issue_state == "OPEN" {
            if !has_task_for_issue(state, repo_slug, *number) {
                let id = format!("{repo_name}#{number}");
                let now = Utc::now();
                let task = Task {
                    id: id.clone(),
                    title: title.clone(),
                    source: TaskSource::GithubIssue {
                        repo: repo_slug.to_string(),
                        number: *number,
                    },
                    status: TaskStatus::Backlog,
                    priority: 5,
                    worktree: None,
                    sessions: Vec::new(),
                    pr: None,
                    remote_host: None,
                    created_at: now,
                    updated_at: now,
                };
                state.tasks.push(task);
                events::log_task_created(&id, &format!("github_issue:{repo_slug}#{number}"));
                modified = true;
            }
        } else {
            // Issue is closed — mark any non-done task as done.
            if let Some(task) = find_task_for_issue_mut(state, repo_slug, *number) {
                if task.status != TaskStatus::Done {
                    let from = status_name(task.status);
                    task.status = TaskStatus::Done;
                    task.updated_at = Utc::now();
                    events::log_task_status_change(&task.id, from, "done", "issue_closed");
                    modified = true;
                }
            }
        }
    }

    modified
}

fn status_name(status: TaskStatus) -> &'static str {
    match status {
        TaskStatus::Backlog => "backlog",
        TaskStatus::Ready => "ready",
        TaskStatus::InProgress => "in_progress",
        TaskStatus::InReview => "in_review",
        TaskStatus::Done => "done",
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    fn empty_state() -> AppState {
        AppState::default()
    }

    fn open_issue(number: u32, title: &str) -> (u32, String, String) {
        (number, title.to_string(), "OPEN".to_string())
    }

    fn closed_issue(number: u32, title: &str) -> (u32, String, String) {
        (number, title.to_string(), "CLOSED".to_string())
    }

    fn state_with_task(repo_slug: &str, number: u32, status: TaskStatus) -> AppState {
        let repo_name = repo_name_from_slug(repo_slug);
        let id = format!("{repo_name}#{number}");
        let now = Utc::now();
        let task = Task {
            id,
            title: "Existing task".to_string(),
            source: TaskSource::GithubIssue {
                repo: repo_slug.to_string(),
                number,
            },
            status,
            priority: 5,
            worktree: None,
            sessions: Vec::new(),
            pr: None,
            remote_host: None,
            created_at: now,
            updated_at: now,
        };
        AppState {
            version: 1,
            tasks: vec![task],
        }
    }

    #[test]
    fn sync_creates_tasks_for_new_issues() {
        let mut state = empty_state();
        let issues = vec![
            open_issue(47, "Task-centric state system"),
            open_issue(48, "Issue sync"),
            open_issue(52, "TUI layout"),
        ];
        let repo = "hopegrace/git-orchard-rs";

        let modified = sync_issues_with_data(&mut state, repo, &issues);

        assert!(modified);
        assert_eq!(state.tasks.len(), 3);
        for task in &state.tasks {
            assert_eq!(task.status, TaskStatus::Backlog);
            assert_eq!(task.priority, 5);
        }
    }

    #[test]
    fn sync_does_not_duplicate_existing_tasks() {
        let repo = "hopegrace/git-orchard-rs";
        let mut state = state_with_task(repo, 47, TaskStatus::InProgress);
        let issues = vec![open_issue(47, "Task-centric state system")];

        let modified = sync_issues_with_data(&mut state, repo, &issues);

        assert!(!modified);
        assert_eq!(state.tasks.len(), 1);
    }

    #[test]
    fn sync_marks_done_when_issue_closed() {
        let repo = "hopegrace/git-orchard-rs";
        let mut state = state_with_task(repo, 47, TaskStatus::InProgress);
        let issues = vec![closed_issue(47, "Task-centric state system")];

        let modified = sync_issues_with_data(&mut state, repo, &issues);

        assert!(modified);
        assert_eq!(state.tasks[0].status, TaskStatus::Done);
    }

    #[test]
    fn sync_ignores_already_done_tasks() {
        let repo = "hopegrace/git-orchard-rs";
        let mut state = state_with_task(repo, 47, TaskStatus::Done);
        let original_updated_at = state.tasks[0].updated_at;
        let issues = vec![closed_issue(47, "Task-centric state system")];

        let modified = sync_issues_with_data(&mut state, repo, &issues);

        assert!(!modified);
        assert_eq!(state.tasks[0].status, TaskStatus::Done);
        assert_eq!(state.tasks[0].updated_at, original_updated_at);
    }

    #[test]
    fn derive_repo_name_from_slug() {
        assert_eq!(repo_name_from_slug("hopegrace/git-orchard-rs"), "git-orchard-rs");
    }

    #[test]
    fn sync_assigns_correct_task_id_format() {
        let mut state = empty_state();
        let repo = "hopegrace/git-orchard-rs";
        let issues = vec![open_issue(47, "Task-centric state system")];

        sync_issues_with_data(&mut state, repo, &issues);

        assert_eq!(state.tasks[0].id, "git-orchard-rs#47");
    }

    #[test]
    fn sync_assigns_correct_task_source() {
        let mut state = empty_state();
        let repo = "hopegrace/git-orchard-rs";
        let issues = vec![open_issue(47, "Task-centric state system")];

        sync_issues_with_data(&mut state, repo, &issues);

        assert_eq!(
            state.tasks[0].source,
            TaskSource::GithubIssue {
                repo: "hopegrace/git-orchard-rs".to_string(),
                number: 47,
            }
        );
    }

    #[test]
    fn sync_returns_false_with_no_issues() {
        let mut state = empty_state();
        let modified = sync_issues_with_data(&mut state, "hopegrace/git-orchard-rs", &[]);
        assert!(!modified);
        assert!(state.tasks.is_empty());
    }
}
