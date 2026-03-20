use std::collections::HashSet;

use crate::types::Worktree;

// ---------------------------------------------------------------------------
// View state (sum type carrying dialog state)
// ---------------------------------------------------------------------------

pub enum ViewState {
    List,
    ConfirmDelete(DeleteState),
    Transfer(TransferState),
    Cleanup(CleanupState),
    NewSession(NewSessionState),
    SetPriority(SetPriorityState),
}

pub struct SetPriorityState {
    pub task_id: String,
}

pub struct DeleteState {
    pub target: Worktree,
    pub phase: Phase,
    pub error: Option<String>,
}

pub struct TransferState {
    pub target: Worktree,
    pub phase: Phase,
    pub error: Option<String>,
}

pub struct CleanupState {
    pub stale: Vec<Worktree>,
    pub selected: HashSet<String>,
    pub cursor: usize,
    pub phase: Phase,
    pub deleted: Vec<String>,
    pub errors: Vec<String>,
}

pub struct NewSessionState {
    pub name: String,
    pub cursor: usize,
}

// ---------------------------------------------------------------------------
// Phase enum
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Phase {
    Idle,
    Confirm,
    InProgress,
    Done,
    Error,
}

// ---------------------------------------------------------------------------
// Messages from background threads
// ---------------------------------------------------------------------------

pub enum AppMsg {
    Worktrees(Vec<Worktree>),
    PaneContent(String, String), // (session_name, content)
    DeleteDone,
    DeleteErr(String),
    TransferDone,
    TransferErr(String),
    CleanupDone,
    Error(String),
}
