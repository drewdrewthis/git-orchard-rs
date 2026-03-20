mod dialogs;
mod list;
mod state;
mod widgets;

use std::sync::mpsc;
use std::time::{Duration, Instant};

use crossterm::event::{self, Event, KeyCode, KeyEvent, KeyModifiers};
use ratatui::prelude::*;

use crate::collector;
use crate::config;
use crate::git;
use crate::remote;
use crate::tmux;
use crate::transfer;
use crate::types::{IssueState, OrchardConfig, Worktree};

use state::{
    AppMsg, CleanupState, Phase, ViewState,
};

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const SPINNER_FRAMES: &[&str] = &[
    "\u{280b}", "\u{2819}", "\u{2839}", "\u{2838}", "\u{283c}", "\u{2834}", "\u{2826}",
    "\u{2827}", "\u{2807}", "\u{280f}",
];

const AUTO_REFRESH_SECS: u64 = 60;
const WARNING_DURATION_SECS: u64 = 3;
const POLL_TIMEOUT_MS: u64 = 100;

// ---------------------------------------------------------------------------
// App
// ---------------------------------------------------------------------------

pub struct App {
    worktrees: Vec<Worktree>,
    cursor: usize,
    loading: bool,
    refreshing: bool,
    error: Option<String>,
    warning: Option<(String, Instant)>,
    config: OrchardConfig,
    repo_root: String,
    repo_name: String,
    pane_content: String,
    view: ViewState,

    // Task-centric state
    app_state: crate::state::AppState,
    backlog_page: usize,

    // Background data channel
    tx: mpsc::Sender<AppMsg>,
    rx: mpsc::Receiver<AppMsg>,

    // Session to switch to after the TUI exits. Set by Enter key handler.
    switch_target: Option<String>,

    // Auto-refresh
    last_refresh: Instant,
    spinner_frame: usize,
}

impl App {
    fn new(command: &str) -> Self {
        let cfg = config::load_config();
        let repo_root = git::find_repo_root();
        let repo_name = git::get_repo_name();
        let app_state = crate::state::load_state();
        let (tx, rx) = mpsc::channel();

        let view = if command == "cleanup" {
            ViewState::Cleanup(CleanupState {
                stale: Vec::new(),
                selected: std::collections::HashSet::new(),
                cursor: 0,
                phase: Phase::Idle,
                deleted: Vec::new(),
                errors: Vec::new(),
            })
        } else {
            ViewState::List
        };

        App {
            worktrees: Vec::new(),
            cursor: 0,
            loading: true,
            refreshing: false,
            error: None,
            warning: None,
            config: cfg,
            repo_root,
            repo_name,
            pane_content: String::new(),
            view,
            app_state,
            backlog_page: 0,
            tx,
            rx,
            last_refresh: Instant::now(),
            spinner_frame: 0,
            switch_target: None,
        }
    }

    // -------------------------------------------------------------------
    // Background refresh pipeline
    // -------------------------------------------------------------------

    fn start_refresh(&self) {
        let tx = self.tx.clone();
        std::thread::spawn(move || {
            let tx_clone = tx.clone();
            let update_fn = move |trees: &[Worktree]| {
                let _ = tx_clone.send(AppMsg::Worktrees(trees.to_vec()));
            };
            let tx_err = tx.clone();
            let error_fn = move |msg: &str| {
                let _ = tx_err.send(AppMsg::Error(msg.to_string()));
            };
            if let Err(e) = collector::refresh_worktrees(&update_fn, &error_fn) {
                let _ = tx.send(AppMsg::Error(e.to_string()));
            }
        });
    }

    fn sync_issues(&mut self) {
        if let Ok((owner, name)) = crate::github::get_repo() {
            let slug = format!("{owner}/{name}");
            if crate::issue_sync::sync_issues(&mut self.app_state, &slug) {
                if let Err(e) = crate::state::save_state(&self.app_state) {
                    crate::logger::LOG.info(&format!("tui: save_state failed: {e}"));
                }
            }
        }
    }

    fn fetch_pane_content(&self) {
        if self.worktrees.is_empty() || self.cursor >= self.worktrees.len() {
            return;
        }
        let wt = &self.worktrees[self.cursor];
        self.fetch_pane_content_for_worktree(wt);
    }

    /// Fetches pane content for a specific worktree (used by both worktree and task views).
    fn fetch_pane_content_for_worktree(&self, wt: &Worktree) {
        let session = match &wt.tmux_session {
            Some(s) => s.clone(),
            None => return,
        };
        let remote_host = wt.remote.clone();
        let tx = self.tx.clone();
        std::thread::spawn(move || {
            let content = if let Some(host) = remote_host {
                remote::capture_remote_pane_content(&host, &session, 100).unwrap_or_default()
            } else {
                tmux::capture_pane_content(&session, 100).unwrap_or_default()
            };
            let _ = tx.send(AppMsg::PaneContent(session.clone(), content));
        });
    }

    // -------------------------------------------------------------------
    // Drain messages from background threads
    // -------------------------------------------------------------------

    fn check_updates(&mut self) {
        while let Ok(msg) = self.rx.try_recv() {
            match msg {
                AppMsg::Worktrees(trees) => {
                    let still_loading = trees.iter().any(|wt| wt.pr_loading);

                    // During refresh, preserve remote worktrees from previous data
                    // until the pipeline sends an update that includes them.
                    let has_remotes = trees.iter().any(|wt| wt.remote.is_some());
                    if !has_remotes && !self.worktrees.is_empty() {
                        let mut merged = trees;
                        let existing_remotes: Vec<Worktree> = self
                            .worktrees
                            .iter()
                            .filter(|wt| wt.remote.is_some())
                            .cloned()
                            .collect();
                        merged.extend(existing_remotes);
                        self.worktrees = merged;
                    } else {
                        self.worktrees = trees;
                    }

                    merge_worktrees_into_tasks(&mut self.app_state.tasks, &self.worktrees);
                    // Save if merge changed any task status
                    if let Err(e) = crate::state::save_state(&self.app_state) {
                        crate::logger::LOG.info(&format!("tui: save_state failed: {e}"));
                    }

                    self.loading = false;
                    if !still_loading {
                        self.refreshing = false;
                        if let Err(e) = crate::status::write_status(&self.worktrees) {
                            crate::logger::LOG.info(&format!("status write failed: {e}"));
                        }
                        // Sync GitHub issues into task state on each full refresh.
                        self.sync_issues();
                    }
                    self.error = None;
                    if self.cursor >= self.worktrees.len() && !self.worktrees.is_empty() {
                        self.cursor = self.worktrees.len() - 1;
                    }
                    // Populate cleanup stale list if in cleanup view with empty stale.
                    if let ViewState::Cleanup(ref mut cs) = self.view {
                        if cs.stale.is_empty() {
                            cs.stale = filter_stale(&self.worktrees);
                            cs.selected =
                                cs.stale.iter().map(|wt| wt.path.clone()).collect();
                        }
                    }
                    // Fetch pane content for the current selection.
                    if !self.app_state.tasks.is_empty() {
                        // Task mode: fetch via task's worktree.
                        self.fetch_task_pane_content();
                    } else if !self.worktrees.is_empty()
                        && self.cursor < self.worktrees.len()
                        && self.worktrees[self.cursor].tmux_session.is_some()
                    {
                        self.fetch_pane_content();
                    } else {
                        self.pane_content.clear();
                    }
                }
                AppMsg::PaneContent(session_name, content) => {
                    // Accept pane content if the session matches the current
                    // selection — works for both task mode and worktree mode.
                    let matches = if !self.app_state.tasks.is_empty() {
                        // Task mode: check if the task's sessions contain this one.
                        let (visible, _) = crate::tui::list::visible_tasks(
                            &self.app_state.tasks, &self.worktrees, self.backlog_page,
                        );
                        visible.get(self.cursor).is_some_and(|vt| {
                            vt.task.sessions.contains(&session_name)
                        })
                    } else {
                        let current_session = self
                            .worktrees
                            .get(self.cursor)
                            .and_then(|wt| wt.tmux_session.as_ref());
                        current_session == Some(&session_name)
                    };
                    if matches {
                        self.pane_content = content;
                    }
                }
                AppMsg::DeleteDone => {
                    if let ViewState::ConfirmDelete(ref mut ds) = self.view {
                        ds.phase = Phase::Done;
                    }
                    self.warning =
                        Some(("Worktree deleted.".to_string(), Instant::now()));
                    self.start_refresh();
                }
                AppMsg::DeleteErr(e) => {
                    if let ViewState::ConfirmDelete(ref mut ds) = self.view {
                        ds.phase = Phase::Error;
                        ds.error = Some(e);
                    }
                }
                AppMsg::TransferDone => {
                    if let ViewState::Transfer(ref mut ts) = self.view {
                        ts.phase = Phase::Done;
                    }
                    self.warning =
                        Some(("Transfer complete.".to_string(), Instant::now()));
                    self.start_refresh();
                }
                AppMsg::TransferErr(e) => {
                    if let ViewState::Transfer(ref mut ts) = self.view {
                        ts.phase = Phase::Error;
                        ts.error = Some(e);
                    }
                }
                AppMsg::CleanupDone => {
                    if let ViewState::Cleanup(ref mut cs) = self.view {
                        cs.phase = Phase::Done;
                    }
                    self.start_refresh();
                }
                AppMsg::Error(e) => {
                    self.error = Some(e);
                    self.loading = false;
                    self.refreshing = false;
                }
            }
        }
    }

    // -------------------------------------------------------------------
    // Key handling -- returns true to quit
    // -------------------------------------------------------------------

    fn handle_key(&mut self, key: KeyEvent) -> bool {
        crate::logger::LOG.info(&format!("tui: key event: {:?} view={:?}", key.code, self.view_name()));

        // Ctrl+C: quit (same as q — no switch target).
        if key.modifiers.contains(KeyModifiers::CONTROL) && key.code == KeyCode::Char('c') {
            return true;
        }

        // We need to temporarily take the view out of self so we can pass
        // mutable references to both self and the dialog state.
        let mut view = std::mem::replace(&mut self.view, ViewState::List);
        match view {
            ViewState::List => {
                self.view = ViewState::List;
                self.handle_list_key(key)
            }
            ViewState::ConfirmDelete(ref mut ds) => {
                let r = self.handle_delete_key(ds, key);
                if !matches!(self.view, ViewState::List) {
                    self.view = view;
                }
                r
            }
            ViewState::Transfer(ref mut ts) => {
                let r = self.handle_transfer_key(ts, key);
                if !matches!(self.view, ViewState::List) {
                    self.view = view;
                }
                r
            }
            ViewState::Cleanup(ref mut cs) => {
                let r = self.handle_cleanup_key(cs, key);
                if !matches!(self.view, ViewState::List) {
                    self.view = view;
                }
                r
            }
            ViewState::NewSession(ref mut ns) => {
                let r = self.handle_new_session_key(ns, key);
                if !matches!(self.view, ViewState::List) {
                    self.view = view;
                }
                r
            }
            ViewState::SetPriority(ref mut sp) => {
                let r = self.handle_set_priority_key(sp, key);
                if !matches!(self.view, ViewState::List) {
                    self.view = view;
                }
                r
            }
        }
    }

    /// Returns a debug-friendly name for the current view state.
    fn view_name(&self) -> &'static str {
        match self.view {
            ViewState::List => "List",
            ViewState::ConfirmDelete(_) => "ConfirmDelete",
            ViewState::Transfer(_) => "Transfer",
            ViewState::Cleanup(_) => "Cleanup",
            ViewState::NewSession(_) => "NewSession",
            ViewState::SetPriority(_) => "SetPriority",
        }
    }

    // -------------------------------------------------------------------
    // Rendering
    // -------------------------------------------------------------------

    fn render(&mut self, f: &mut Frame) {
        self.spinner_frame = (self.spinner_frame + 1) % SPINNER_FRAMES.len();

        match &self.view {
            ViewState::List => self.render_list(f),
            ViewState::ConfirmDelete(_) => {
                // Need to temporarily take view out to get the state
                let view = std::mem::replace(&mut self.view, ViewState::List);
                if let ViewState::ConfirmDelete(ds) = &view {
                    self.render_delete(ds, f);
                }
                self.view = view;
            }
            ViewState::Transfer(_) => {
                let view = std::mem::replace(&mut self.view, ViewState::List);
                if let ViewState::Transfer(ts) = &view {
                    self.render_transfer(ts, f);
                }
                self.view = view;
            }
            ViewState::Cleanup(_) => {
                let view = std::mem::replace(&mut self.view, ViewState::List);
                if let ViewState::Cleanup(cs) = &view {
                    self.render_cleanup(cs, f);
                }
                self.view = view;
            }
            ViewState::NewSession(_) => {
                self.render_list(f);
                let view = std::mem::replace(&mut self.view, ViewState::List);
                if let ViewState::NewSession(ns) = &view {
                    self.render_new_session(ns, f);
                }
                self.view = view;
            }
            ViewState::SetPriority(_) => {
                self.render_list(f);
                let view = std::mem::replace(&mut self.view, ViewState::List);
                if let ViewState::SetPriority(sp) = &view {
                    self.render_set_priority(sp, f);
                }
                self.view = view;
            }
        }
    }

    // -------------------------------------------------------------------
    // Actions (delete, transfer, cleanup)
    // -------------------------------------------------------------------

    fn start_delete(&self, target: &Worktree) {
        let wt = target.clone();
        let config = self.config.clone();
        let tx = self.tx.clone();
        std::thread::spawn(move || {
            match delete_worktree(&wt, &config) {
                Ok(()) => {
                    let _ = tx.send(AppMsg::DeleteDone);
                }
                Err(e) => {
                    let _ = tx.send(AppMsg::DeleteErr(e.to_string()));
                }
            }
        });
    }

    fn start_transfer(&self, target: &Worktree) {
        let wt = target.clone();
        let config = self.config.clone();
        let repo_root = self.repo_root.clone();
        let tx = self.tx.clone();
        std::thread::spawn(move || {
            let remote_cfg = match config.remote {
                Some(ref r) => r,
                None => {
                    let _ = tx.send(AppMsg::TransferErr("No remote configured".to_string()));
                    return;
                }
            };
            let result = if wt.remote.is_some() {
                transfer::pull_to_local(&wt, remote_cfg, &repo_root, &|_| {})
            } else {
                transfer::push_to_remote(&wt, remote_cfg, &|_| {})
            };
            match result {
                Ok(()) => {
                    let _ = tx.send(AppMsg::TransferDone);
                }
                Err(e) => {
                    let _ = tx.send(AppMsg::TransferErr(e.to_string()));
                }
            }
        });
    }

    fn start_cleanup(&self, items: Vec<Worktree>) {
        let config = self.config.clone();
        let tx = self.tx.clone();
        std::thread::spawn(move || {
            for wt in &items {
                let _ = delete_worktree(wt, &config);
            }
            let _ = tx.send(AppMsg::CleanupDone);
        });
    }
}

// ---------------------------------------------------------------------------
// Public entry point
// ---------------------------------------------------------------------------

/// Runs the Ratatui TUI. `command` determines the initial view ("cleanup" or default list).
///
/// Returns `Ok(Some(session_name))` when the user selects a session to switch to,
/// or `Ok(None)` when the user quits without selecting. The caller is responsible
/// for performing the `tmux switch-client` (or printing the session name to stdout
/// for the wrapper script to handle).
pub fn run(command: &str) -> anyhow::Result<Option<String>> {
    // Render the TUI to /dev/tty so stdout stays clean for the session name.
    // This is the standard approach (fzf, tig, lazygit all do this) — the TUI
    // talks directly to the terminal, stdout is reserved for machine output.
    let tty = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open("/dev/tty")?;

    crossterm::terminal::enable_raw_mode()?;
    let mut tty_write = tty.try_clone()?;
    crossterm::execute!(tty_write, crossterm::terminal::EnterAlternateScreen)?;
    let backend = ratatui::backend::CrosstermBackend::new(tty_write);
    let mut terminal = ratatui::Terminal::new(backend)?;
    terminal.clear()?;

    let mut app = App::new(command);

    // Initial data fetch in background
    app.start_refresh();

    let result = run_loop(&mut terminal, &mut app);

    // Restore terminal
    crossterm::terminal::disable_raw_mode()?;
    crossterm::execute!(
        terminal.backend_mut(),
        crossterm::terminal::LeaveAlternateScreen
    )?;
    terminal.show_cursor()?;

    result
}

fn run_loop(
    terminal: &mut ratatui::Terminal<ratatui::backend::CrosstermBackend<std::fs::File>>,
    app: &mut App,
) -> anyhow::Result<Option<String>> {
    loop {
        terminal.draw(|f| app.render(f))?;

        // Poll for events with timeout (for spinner animation).
        if event::poll(Duration::from_millis(POLL_TIMEOUT_MS))? {
            if let Event::Key(key) = event::read()? {
                if app.handle_key(key) {
                    break;
                }
            }
        }

        // Check for background data updates.
        app.check_updates();

        // Auto-refresh.
        if app.last_refresh.elapsed() > Duration::from_secs(AUTO_REFRESH_SECS) {
            app.last_refresh = Instant::now();
            app.refreshing = true;
            app.start_refresh();
        }
    }
    Ok(app.switch_target.take())
}

// ---------------------------------------------------------------------------
// Stale worktree filter
// ---------------------------------------------------------------------------

fn filter_stale(worktrees: &[Worktree]) -> Vec<Worktree> {
    worktrees
        .iter()
        .filter(|wt| {
            if wt.is_bare {
                return false;
            }
            if let Some(ref pr) = wt.pr {
                return pr.state == "merged" || pr.state == "closed";
            }
            if wt.pr.is_none() {
                if let Some(state) = wt.issue_state {
                    return state == IssueState::Completed || state == IssueState::Closed;
                }
            }
            false
        })
        .cloned()
        .collect()
}

// ---------------------------------------------------------------------------
// Delete worktree (shared by single-delete and cleanup)
// ---------------------------------------------------------------------------

fn delete_worktree(wt: &Worktree, config: &OrchardConfig) -> anyhow::Result<()> {
    if let Some(ref host) = wt.remote {
        // Remote deletion
        if let Some(ref sess) = wt.tmux_session {
            let _ = remote::kill_remote_tmux_session(host, sess);
        }
        if let Some(ref branch) = wt.branch {
            let slug = transfer::sanitize_branch_slug(branch);
            let _ = remote::remove_remote_registry_entry(host, &slug);
        }
        if let Some(ref remote_cfg) = config.remote {
            remote::remove_remote_worktree(host, &remote_cfg.repo_path, &wt.path)?;
        }
        return Ok(());
    }

    // Local deletion
    if let Some(ref sess) = wt.tmux_session {
        let _ = tmux::kill_tmux_session(sess);
    }
    if git::remove_worktree(&wt.path, false).is_err() {
        git::remove_worktree(&wt.path, true)?;
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Task–worktree merge
// ---------------------------------------------------------------------------

/// Merges live worktree data into tasks and auto-detects task status.
///
/// 1. Matches tasks to worktrees by issue number extracted from branch names
/// 2. Updates PR number and session info from matched worktrees
/// 3. Auto-promotes task status based on worktree/PR state:
///    - Has worktree + open PR with review → in_review
///    - Has worktree or session → in_progress
///    - PR merged/closed → done
pub(crate) fn merge_worktrees_into_tasks(tasks: &mut [crate::state::Task], worktrees: &[Worktree]) {
    use crate::state::TaskStatus;

    for task in tasks.iter_mut() {
        let issue_num = match &task.source {
            crate::state::TaskSource::GithubIssue { number, .. } => *number,
        };

        // Try to find a matching worktree by existing path reference first,
        // then by issue number in branch name.
        let wt = if let Some(ref path) = task.worktree {
            worktrees.iter().find(|w| &w.path == path)
        } else {
            None
        }
        .or_else(|| {
            worktrees.iter().find(|w| {
                w.issue_number == Some(issue_num)
                    || w.branch
                        .as_ref()
                        .map(|b| branch_contains_issue(b, issue_num))
                        .unwrap_or(false)
            })
        });

        let Some(wt) = wt else { continue };

        // Bind worktree path to task if not already set.
        if task.worktree.is_none() {
            task.worktree = Some(wt.path.clone());
        }

        // Propagate remote host info.
        if wt.remote.is_some() {
            task.remote_host = wt.remote.clone();
        }

        // Update PR info.
        if let Some(ref pr) = wt.pr {
            task.pr = Some(pr.number);
        }

        // Update session info.
        if let Some(ref session) = wt.tmux_session {
            if !task.sessions.contains(session) {
                task.sessions.push(session.clone());
            }
        }

        // Auto-detect status (only promote, never demote — user may have
        // manually set a status we shouldn't override).
        if task.status == TaskStatus::Backlog || task.status == TaskStatus::Ready {
            if let Some(ref pr) = wt.pr {
                if pr.state == "merged" || pr.state == "closed" {
                    task.status = TaskStatus::Done;
                } else if pr.review_decision != crate::types::ReviewDecision::None {
                    task.status = TaskStatus::InReview;
                } else {
                    task.status = TaskStatus::InProgress;
                }
            } else if wt.tmux_session.is_some() || !wt.is_bare {
                task.status = TaskStatus::InProgress;
            }
        }
    }
}

/// Checks if a branch name contains the issue number (e.g., "issue-47", "feat/47-login").
fn branch_contains_issue(branch: &str, issue: u32) -> bool {
    let num_str = issue.to_string();
    // Match patterns like: "47", "47-foo", "foo-47", "issue47", "issue-47"
    branch.split(|c: char| !c.is_ascii_digit()).any(|seg| seg == num_str)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::TaskStatus;
    use crate::types::{ChecksStatus, IssueState, PrInfo, ReviewDecision};

    #[test]
    fn filter_stale_merged_pr() {
        let trees = vec![
            Worktree {
                pr: Some(PrInfo {
                    number: 1,
                    state: "merged".into(),
                    title: String::new(),
                    url: String::new(),
                    review_decision: ReviewDecision::None,
                    unresolved_threads: 0,
                    checks_status: ChecksStatus::None,
                    has_conflicts: false,
                }),
                ..Default::default()
            },
            Worktree::default(),
        ];
        let stale = filter_stale(&trees);
        assert_eq!(stale.len(), 1);
    }

    #[test]
    fn filter_stale_closed_issue() {
        let trees = vec![Worktree {
            issue_state: Some(IssueState::Closed),
            ..Default::default()
        }];
        let stale = filter_stale(&trees);
        assert_eq!(stale.len(), 1);
    }

    #[test]
    fn filter_stale_closed_pr() {
        let trees = vec![Worktree {
            pr: Some(PrInfo {
                number: 1,
                state: "closed".into(),
                title: String::new(),
                url: String::new(),
                review_decision: ReviewDecision::None,
                unresolved_threads: 0,
                checks_status: ChecksStatus::None,
                has_conflicts: false,
            }),
            ..Default::default()
        }];
        let stale = filter_stale(&trees);
        assert_eq!(stale.len(), 1);
    }

    #[test]
    fn filter_stale_completed_issue() {
        let trees = vec![Worktree {
            issue_state: Some(IssueState::Completed),
            ..Default::default()
        }];
        let stale = filter_stale(&trees);
        assert_eq!(stale.len(), 1);
    }

    #[test]
    fn filter_stale_skips_bare() {
        let trees = vec![Worktree {
            is_bare: true,
            pr: Some(PrInfo {
                number: 1,
                state: "merged".into(),
                title: String::new(),
                url: String::new(),
                review_decision: ReviewDecision::None,
                unresolved_threads: 0,
                checks_status: ChecksStatus::None,
                has_conflicts: false,
            }),
            ..Default::default()
        }];
        let stale = filter_stale(&trees);
        assert!(stale.is_empty());
    }

    fn make_task(status: crate::state::TaskStatus, priority: u32, issue: u32) -> crate::state::Task {
        use chrono::Utc;
        crate::state::Task {
            id: format!("repo#{}", issue),
            title: String::new(),
            source: crate::state::TaskSource::GithubIssue {
                repo: "owner/repo".to_string(),
                number: issue,
            },
            status,
            priority,
            worktree: None,
            sessions: Vec::new(),
            pr: None,
            remote_host: None,
            created_at: Utc::now(),
            updated_at: Utc::now(),
        }
    }

    #[test]
    fn merge_worktrees_updates_task_pr() {
        let mut tasks = vec![{
            let mut t = make_task(crate::state::TaskStatus::InProgress, 1, 47);
            t.worktree = Some("/ws/repo-47".to_string());
            t
        }];
        let worktrees = vec![Worktree {
            path: "/ws/repo-47".to_string(),
            pr: Some(PrInfo {
                number: 53,
                state: "open".into(),
                title: String::new(),
                url: String::new(),
                review_decision: ReviewDecision::None,
                unresolved_threads: 0,
                checks_status: ChecksStatus::None,
                has_conflicts: false,
            }),
            ..Default::default()
        }];
        merge_worktrees_into_tasks(&mut tasks, &worktrees);
        assert_eq!(tasks[0].pr, Some(53));
    }

    #[test]
    fn merge_worktrees_discovers_session() {
        let mut tasks = vec![{
            let mut t = make_task(crate::state::TaskStatus::InProgress, 1, 47);
            t.worktree = Some("/ws/repo-47".to_string());
            t
        }];
        let worktrees = vec![Worktree {
            path: "/ws/repo-47".to_string(),
            tmux_session: Some("repo_47_main".to_string()),
            ..Default::default()
        }];
        merge_worktrees_into_tasks(&mut tasks, &worktrees);
        assert!(tasks[0].sessions.contains(&"repo_47_main".to_string()));
    }

    #[test]
    fn merge_worktrees_skips_task_without_worktree() {
        let mut tasks = vec![make_task(crate::state::TaskStatus::Ready, 1, 48)];
        let worktrees = vec![Worktree {
            path: "/ws/repo-47".to_string(),
            tmux_session: Some("repo_47_main".to_string()),
            ..Default::default()
        }];
        merge_worktrees_into_tasks(&mut tasks, &worktrees);
        assert!(tasks[0].sessions.is_empty());
    }

    #[test]
    fn merge_worktrees_does_not_duplicate_session() {
        let mut tasks = vec![{
            let mut t = make_task(crate::state::TaskStatus::InProgress, 1, 47);
            t.worktree = Some("/ws/repo-47".to_string());
            t.sessions = vec!["repo_47_main".to_string()];
            t
        }];
        let worktrees = vec![Worktree {
            path: "/ws/repo-47".to_string(),
            tmux_session: Some("repo_47_main".to_string()),
            ..Default::default()
        }];
        merge_worktrees_into_tasks(&mut tasks, &worktrees);
        assert_eq!(tasks[0].sessions.len(), 1);
    }

    #[test]
    fn task_status_transition_to_done() {
        let mut task = make_task(TaskStatus::InProgress, 1, 47);
        let before = task.updated_at;
        // Simulate a tiny sleep so updated_at can differ.
        std::thread::sleep(std::time::Duration::from_millis(1));
        task.status = TaskStatus::Done;
        task.updated_at = chrono::Utc::now();
        assert_eq!(task.status, TaskStatus::Done);
        assert!(task.updated_at >= before);
    }

    #[test]
    fn task_priority_update() {
        let mut task = make_task(TaskStatus::Ready, 5, 47);
        assert_eq!(task.priority, 5);
        task.priority = 1;
        task.updated_at = chrono::Utc::now();
        assert_eq!(task.priority, 1);
    }

    #[test]
    fn start_moves_backlog_to_in_progress() {
        let mut task = make_task(TaskStatus::Backlog, 3, 52);
        assert_eq!(task.status, TaskStatus::Backlog);
        task.status = TaskStatus::InProgress;
        task.updated_at = chrono::Utc::now();
        assert_eq!(task.status, TaskStatus::InProgress);
    }
}
