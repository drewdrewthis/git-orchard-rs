use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct Worktree {
    pub path: String,
    pub branch: Option<String>,
    pub head: String,
    pub is_bare: bool,
    pub has_conflicts: bool,
    pub pr: Option<PrInfo>,
    pub pr_loading: bool,
    pub tmux_session: Option<String>,
    pub tmux_attached: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub remote: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub issue_number: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub issue_state: Option<IssueState>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PrInfo {
    pub number: u32,
    pub state: String, // "open", "merged", "closed"
    pub title: String,
    pub url: String,
    pub review_decision: ReviewDecision,
    pub unresolved_threads: u32,
    pub checks_status: ChecksStatus,
    pub has_conflicts: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub enum PrStatus {
    Conflict,
    Failing,
    Unresolved,
    ChangesRequested,
    ReviewNeeded,
    PendingCi,
    Approved,
    Merged,
    Closed,
}

impl PrStatus {
    pub fn display(self) -> StatusDisplay {
        match self {
            Self::Conflict => StatusDisplay { icon: "✖", label: "conflict" },
            Self::Failing => StatusDisplay { icon: "✖", label: "failing" },
            Self::Unresolved => StatusDisplay { icon: "◯", label: "unresolved" },
            Self::ChangesRequested => StatusDisplay { icon: "✖", label: "changes" },
            Self::ReviewNeeded => StatusDisplay { icon: "◯", label: "review" },
            Self::PendingCi => StatusDisplay { icon: "◯", label: "pending" },
            Self::Approved => StatusDisplay { icon: "✓", label: "ready" },
            Self::Merged => StatusDisplay { icon: "●", label: "merged" },
            Self::Closed => StatusDisplay { icon: "●", label: "closed" },
        }
    }

    pub fn color(self) -> &'static str {
        match self {
            Self::Conflict | Self::Failing | Self::ChangesRequested | Self::Closed => "red",
            Self::Unresolved | Self::ReviewNeeded | Self::PendingCi => "yellow",
            Self::Approved => "green",
            Self::Merged => "magenta",
        }
    }
}

pub struct StatusDisplay {
    pub icon: &'static str,
    pub label: &'static str,
}

pub fn resolve_pr_status(pr: &PrInfo) -> PrStatus {
    if pr.has_conflicts {
        return PrStatus::Conflict;
    }
    if pr.checks_status == ChecksStatus::Fail {
        return PrStatus::Failing;
    }
    if pr.unresolved_threads > 0 {
        return PrStatus::Unresolved;
    }
    if pr.review_decision == ReviewDecision::ChangesRequested {
        return PrStatus::ChangesRequested;
    }
    if pr.state == "merged" {
        return PrStatus::Merged;
    }
    if pr.state == "closed" {
        return PrStatus::Closed;
    }
    if pr.review_decision == ReviewDecision::None || pr.review_decision == ReviewDecision::ReviewRequired {
        return PrStatus::ReviewNeeded;
    }
    if pr.checks_status == ChecksStatus::Pending {
        return PrStatus::PendingCi;
    }
    if pr.review_decision == ReviewDecision::Approved {
        return PrStatus::Approved;
    }
    PrStatus::ReviewNeeded
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
pub enum ReviewDecision {
    #[default]
    #[serde(rename = "")]
    None,
    #[serde(rename = "APPROVED")]
    Approved,
    #[serde(rename = "CHANGES_REQUESTED")]
    ChangesRequested,
    #[serde(rename = "REVIEW_REQUIRED")]
    ReviewRequired,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum ChecksStatus {
    Pass,
    Fail,
    Pending,
    #[default]
    None,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum IssueState {
    Open,
    Closed,
    Completed,
}

#[derive(Debug, Clone)]
pub struct TmuxSession {
    pub name: String,
    pub path: String,
    pub attached: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct RemoteConfig {
    pub host: String,
    pub repo_path: String,
    #[serde(default = "default_shell")]
    pub shell: String,
}

fn default_shell() -> String {
    "ssh".to_string()
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct OrchardConfig {
    pub remote: Option<RemoteConfig>,
}

pub struct SwitchToSessionOptions {
    pub session_name: String,
    pub worktree_path: String,
    pub branch: Option<String>,
    pub pr: Option<PrInfo>,
}

pub type StepCallback = Box<dyn Fn(&str) + Send>;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn conflict_highest_priority() {
        let pr = PrInfo {
            number: 1, state: "open".into(), title: String::new(), url: String::new(),
            review_decision: ReviewDecision::None, unresolved_threads: 0,
            checks_status: ChecksStatus::Fail, has_conflicts: true,
        };
        assert_eq!(resolve_pr_status(&pr), PrStatus::Conflict);
    }

    #[test]
    fn failing_ci() {
        let pr = PrInfo {
            number: 1, state: "open".into(), title: String::new(), url: String::new(),
            review_decision: ReviewDecision::None, unresolved_threads: 0,
            checks_status: ChecksStatus::Fail, has_conflicts: false,
        };
        assert_eq!(resolve_pr_status(&pr), PrStatus::Failing);
    }

    #[test]
    fn approved() {
        let pr = PrInfo {
            number: 1, state: "open".into(), title: String::new(), url: String::new(),
            review_decision: ReviewDecision::Approved, unresolved_threads: 0,
            checks_status: ChecksStatus::Pass, has_conflicts: false,
        };
        assert_eq!(resolve_pr_status(&pr), PrStatus::Approved);
    }

    #[test]
    fn merged() {
        let pr = PrInfo {
            number: 1, state: "merged".into(), title: String::new(), url: String::new(),
            review_decision: ReviewDecision::Approved, unresolved_threads: 0,
            checks_status: ChecksStatus::Pass, has_conflicts: false,
        };
        assert_eq!(resolve_pr_status(&pr), PrStatus::Merged);
    }

    #[test]
    fn pending_ci() {
        let pr = PrInfo {
            number: 1, state: "open".into(), title: String::new(), url: String::new(),
            review_decision: ReviewDecision::Approved, unresolved_threads: 0,
            checks_status: ChecksStatus::Pending, has_conflicts: false,
        };
        assert_eq!(resolve_pr_status(&pr), PrStatus::PendingCi);
    }

    #[test]
    fn display_entries_exist() {
        let statuses = [
            PrStatus::Conflict, PrStatus::Failing, PrStatus::Unresolved,
            PrStatus::ChangesRequested, PrStatus::ReviewNeeded, PrStatus::PendingCi,
            PrStatus::Approved, PrStatus::Merged, PrStatus::Closed,
        ];
        for s in statuses {
            let d = s.display();
            assert!(!d.icon.is_empty());
            assert!(!d.label.is_empty());
        }
    }
}
