use crossterm::event::{KeyCode, KeyEvent};
use ratatui::prelude::*;
use ratatui::widgets::*;

use std::collections::HashSet;
use std::time::Instant;

use crate::navigation;
use crate::paths;
use crate::state::{Task, TaskStatus};
use crate::tui::state::{
    CleanupState, DeleteState, NewSessionState, Phase, SetPriorityState, TransferState, ViewState,
};
use crate::tui::widgets::{claude_badge, status_badge};
use crate::tui::{filter_stale, App, SPINNER_FRAMES, WARNING_DURATION_SECS};
use crate::remote;
use crate::tmux;
use crate::types::Worktree;

// ---------------------------------------------------------------------------
// Task view helpers
// ---------------------------------------------------------------------------

const BACKLOG_PAGE_SIZE: usize = 10;

/// Display groups for the task view, ordered by priority of attention needed.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum DisplayGroup {
    NeedsYou,
    ClaudeWorking,
    ClaudeDone,
    InReview,
    Backlog,
}

impl DisplayGroup {
    fn label(self) -> &'static str {
        match self {
            Self::NeedsYou => "needs you",
            Self::ClaudeWorking => "claude working",
            Self::ClaudeDone => "claude done",
            Self::InReview => "in review",
            Self::Backlog => "backlog",
        }
    }

    fn color(self) -> Color {
        match self {
            Self::NeedsYou => Color::Red,
            Self::ClaudeWorking => Color::Green,
            Self::ClaudeDone => Color::Yellow,
            Self::InReview => Color::Cyan,
            Self::Backlog => Color::DarkGray,
        }
    }

    fn order(self) -> u8 {
        match self {
            Self::NeedsYou => 0,
            Self::ClaudeWorking => 1,
            Self::ClaudeDone => 2,
            Self::InReview => 3,
            Self::Backlog => 4,
        }
    }
}

/// Derives the display group for a task based on its state and matching worktree.
fn derive_display_group(task: &Task, worktrees: &[Worktree]) -> DisplayGroup {
    // Find matching worktree by task.worktree path
    let wt = task.worktree.as_ref().and_then(|path| {
        worktrees.iter().find(|w| &w.path == path)
    });

    if let Some(wt) = wt {
        // Check PR-based conditions for NeedsYou
        if let Some(ref pr) = wt.pr {
            if pr.has_conflicts {
                return DisplayGroup::NeedsYou;
            }
            if pr.checks_status == crate::types::ChecksStatus::Fail {
                return DisplayGroup::NeedsYou;
            }
            if pr.review_decision == crate::types::ReviewDecision::ChangesRequested {
                return DisplayGroup::NeedsYou;
            }
            if pr.unresolved_threads > 0 {
                return DisplayGroup::NeedsYou;
            }
            if pr.review_decision == crate::types::ReviewDecision::Approved {
                return DisplayGroup::InReview;
            }
        }

        // Check tmux session for Claude activity
        if wt.tmux_session.is_some() {
            if let Some(ref title) = wt.tmux_pane_title {
                if title.to_lowercase().contains("claude") {
                    return DisplayGroup::ClaudeWorking;
                }
            }
            return DisplayGroup::ClaudeDone;
        }
    }

    DisplayGroup::Backlog
}

/// A task entry prepared for rendering in the task-centric view.
#[derive(Debug)]
pub(crate) struct VisibleTask<'a> {
    /// Sequential display number (1-based).
    pub num: usize,
    pub task: &'a Task,
    pub group: DisplayGroup,
}

/// Returns the visible tasks (excluding done), grouped by DisplayGroup and sorted for rendering.
///
/// Order: NEEDS YOU → CLAUDE WORKING → CLAUDE DONE → IN REVIEW → BACKLOG.
/// Within each group: priority ascending (1 = highest), then issue number ascending.
/// BACKLOG is paginated: only tasks for the given page are returned.
pub(crate) fn visible_tasks<'a>(
    tasks: &'a [Task],
    worktrees: &[Worktree],
    backlog_page: usize,
) -> (Vec<VisibleTask<'a>>, usize) {
    let mut grouped: Vec<(&Task, DisplayGroup)> = tasks
        .iter()
        .filter(|t| t.status != TaskStatus::Done)
        .map(|t| {
            let group = derive_display_group(t, worktrees);
            (t, group)
        })
        .collect();

    fn sort_key(item: &(&Task, DisplayGroup)) -> (u8, u32, u32) {
        let issue = match &item.0.source {
            crate::state::TaskSource::GithubIssue { number, .. } => *number,
        };
        (item.1.order(), item.0.priority, issue)
    }

    grouped.sort_by_key(sort_key);

    let total_backlog = grouped.iter().filter(|(_, g)| *g == DisplayGroup::Backlog).count();

    // Separate non-backlog and backlog items
    let non_backlog: Vec<_> = grouped.iter().filter(|(_, g)| *g != DisplayGroup::Backlog).collect();
    let backlog_all: Vec<_> = grouped.iter().filter(|(_, g)| *g == DisplayGroup::Backlog).collect();

    let backlog_start = backlog_page * BACKLOG_PAGE_SIZE;
    let backlog_page_slice = if backlog_start < backlog_all.len() {
        &backlog_all[backlog_start..(backlog_start + BACKLOG_PAGE_SIZE).min(backlog_all.len())]
    } else {
        &[][..]
    };

    let mut result = Vec::new();
    let mut num = 1usize;

    for &&(task, group) in &non_backlog {
        result.push(VisibleTask { num, task, group });
        num += 1;
    }
    for &&(task, group) in backlog_page_slice {
        result.push(VisibleTask { num, task, group });
        num += 1;
    }

    (result, total_backlog)
}

fn issue_number_from_task(task: &Task) -> u32 {
    match &task.source {
        crate::state::TaskSource::GithubIssue { number, .. } => *number,
    }
}

/// Returns a single PR status string for the task, based on the matching worktree's PR.
fn pr_status_text(task: &Task, worktrees: &[Worktree]) -> (String, Style) {
    let wt = task.worktree.as_ref().and_then(|path| {
        worktrees.iter().find(|w| &w.path == path)
    });

    let Some(wt) = wt else {
        return ("no PR".to_string(), Style::default().fg(Color::DarkGray));
    };

    let Some(ref pr) = wt.pr else {
        return ("no PR".to_string(), Style::default().fg(Color::DarkGray));
    };

    // Priority order for status display
    if pr.review_decision == crate::types::ReviewDecision::Approved {
        return ("\u{2713} approved".to_string(), Style::default().fg(Color::Green));
    }
    if pr.review_decision == crate::types::ReviewDecision::ChangesRequested {
        return ("\u{2716} changes req".to_string(), Style::default().fg(Color::Red));
    }
    if pr.has_conflicts {
        return ("\u{2716} conflict".to_string(), Style::default().fg(Color::Red));
    }
    if pr.unresolved_threads > 0 {
        return (
            format!("\u{25cb} unresolved ({})", pr.unresolved_threads),
            Style::default().fg(Color::Yellow),
        );
    }
    if pr.checks_status == crate::types::ChecksStatus::Fail {
        return ("\u{2716} failing".to_string(), Style::default().fg(Color::Red));
    }
    if pr.checks_status == crate::types::ChecksStatus::Pending {
        return ("\u{25d0} pending CI".to_string(), Style::default().fg(Color::Yellow));
    }
    if pr.state == "draft" {
        return ("draft".to_string(), Style::default().fg(Color::DarkGray));
    }
    // Default for open PR with no special state
    ("\u{25cb} needs review".to_string(), Style::default().fg(Color::DarkGray))
}

/// Returns a Claude activity indicator for the task.
fn claude_status_text(task: &Task, worktrees: &[Worktree]) -> (String, Style) {
    let wt = task.worktree.as_ref().and_then(|path| {
        worktrees.iter().find(|w| &w.path == path)
    });

    let Some(wt) = wt else {
        return ("\u{25cb} none".to_string(), Style::default().fg(Color::DarkGray));
    };

    if wt.tmux_session.is_none() {
        return ("\u{25cb} none".to_string(), Style::default().fg(Color::DarkGray));
    }

    if let Some(ref title) = wt.tmux_pane_title {
        if title.to_lowercase().contains("claude") {
            return ("\u{26a1} active".to_string(), Style::default().fg(Color::Green));
        }
    }

    ("\u{25cf} idle".to_string(), Style::default().fg(Color::Yellow))
}

impl App {
    pub(crate) fn handle_list_key(&mut self, key: KeyEvent) -> bool {
        // In task mode, navigation uses the visible task count.
        if !self.app_state.tasks.is_empty() {
            return self.handle_task_list_key(key);
        }

        match key.code {
            // Digit jump 1-9
            KeyCode::Char(c) if c.is_ascii_digit() && c != '0' => {
                if let Some(idx) =
                    navigation::cursor_index_from_digit(c, self.worktrees.len())
                {
                    self.cursor = idx;
                    self.pane_content.clear();
                    self.fetch_pane_content();
                }
                false
            }
            KeyCode::Up | KeyCode::Char('k') => {
                if self.cursor > 0 {
                    self.cursor -= 1;
                    self.pane_content.clear();
                    self.fetch_pane_content();
                }
                false
            }
            KeyCode::Down | KeyCode::Char('j') => {
                if !self.worktrees.is_empty() && self.cursor < self.worktrees.len() - 1 {
                    self.cursor += 1;
                    self.pane_content.clear();
                    self.fetch_pane_content();
                }
                false
            }
            KeyCode::Enter | KeyCode::Char('t') => {
                self.switch_to_tmux_session()
            }
            KeyCode::Char('o') => {
                self.open_pr_url();
                false
            }
            KeyCode::Char('p') => {
                self.start_transfer_dialog();
                false
            }
            KeyCode::Char('d') => {
                self.start_delete_dialog();
                false
            }
            KeyCode::Char('c') => {
                self.enter_cleanup_view();
                false
            }
            KeyCode::Char('n') => {
                self.view = ViewState::NewSession(NewSessionState {
                    name: String::new(),
                    cursor: 0,
                });
                false
            }
            KeyCode::Char('r') => {
                self.refreshing = true;
                self.start_refresh();
                false
            }
            KeyCode::Char('q') | KeyCode::Esc => {
                // Quit without switching sessions.
                true
            }
            _ => false,
        }
    }

    fn handle_task_list_key(&mut self, key: KeyEvent) -> bool {
        let (tasks, total_backlog) = visible_tasks(&self.app_state.tasks, &self.worktrees, self.backlog_page);
        let visible_count = tasks.len();

        match key.code {
            // Digit jump 1-9: jump to flat index
            KeyCode::Char(c) if c.is_ascii_digit() && c != '0' => {
                if let Some(idx) = navigation::cursor_index_from_digit(c, visible_count) {
                    self.cursor = idx;
                    self.fetch_task_pane_content();
                }
                false
            }
            KeyCode::Up | KeyCode::Char('k') => {
                if self.cursor > 0 {
                    self.cursor -= 1;
                    self.fetch_task_pane_content();
                } else if self.backlog_page > 0 {
                    // At top of page: scroll backlog page back
                    self.backlog_page -= 1;
                    let (new_tasks, _) = visible_tasks(&self.app_state.tasks, &self.worktrees, self.backlog_page);
                    self.cursor = new_tasks.len().saturating_sub(1);
                    self.fetch_task_pane_content();
                }
                false
            }
            KeyCode::Down | KeyCode::Char('j') => {
                if visible_count > 0 && self.cursor < visible_count - 1 {
                    self.cursor += 1;
                    self.fetch_task_pane_content();
                } else if self.cursor == visible_count.saturating_sub(1) {
                    // At bottom: try advancing backlog page
                    let next_start = (self.backlog_page + 1) * BACKLOG_PAGE_SIZE;
                    if next_start < total_backlog {
                        self.backlog_page += 1;
                        // Keep cursor at the first backlog item of the new page.
                        let non_backlog = tasks.iter().filter(|t| t.group != DisplayGroup::Backlog).count();
                        self.cursor = non_backlog;
                        self.fetch_task_pane_content();
                    }
                }
                false
            }
            KeyCode::Char('p') => {
                if let Some(task_id) = self.task_id_at_cursor() {
                    self.view = ViewState::SetPriority(SetPriorityState { task_id });
                }
                false
            }
            KeyCode::Char('s') => {
                self.start_task();
                false
            }
            KeyCode::Enter => {
                // Switch to the task's session. If the task has a worktree,
                // delegate to the existing switch_to_tmux_session logic by
                // finding the matching worktree and selecting it.
                let (visible, _) = visible_tasks(&self.app_state.tasks, &self.worktrees, self.backlog_page);
                if let Some(vt) = visible.get(self.cursor) {
                    if let Some(ref wt_path) = vt.task.worktree {
                        // Find the worktree index in self.worktrees.
                        if let Some(idx) = self.worktrees.iter().position(|w| &w.path == wt_path) {
                            self.cursor = idx;
                            return self.switch_to_tmux_session();
                        }
                    }
                    // Task has a session but no worktree match — switch directly.
                    if let Some(session) = vt.task.sessions.first() {
                        self.switch_target = Some(session.clone());
                        return true;
                    }
                }
                false
            }
            KeyCode::Char('o') => {
                // Open PR URL in browser for the selected task.
                let (visible, _) = visible_tasks(&self.app_state.tasks, &self.worktrees, self.backlog_page);
                if let Some(vt) = visible.get(self.cursor) {
                    if let Some(ref wt_path) = vt.task.worktree {
                        if let Some(wt) = self.worktrees.iter().find(|w| &w.path == wt_path) {
                            if let Some(ref pr) = wt.pr {
                                if !pr.url.is_empty() {
                                    crate::browser::open_url(&pr.url);
                                }
                            }
                        }
                    }
                }
                false
            }
            KeyCode::Char('c') => {
                // Cleanup: enter cleanup view for done/stale worktrees.
                self.enter_cleanup_view();
                false
            }
            KeyCode::Char('r') => {
                self.refreshing = true;
                self.start_refresh();
                false
            }
            KeyCode::Char('q') | KeyCode::Esc => true,
            _ => false,
        }
    }

    /// Fetches pane content for the task at the current cursor position.
    pub(crate) fn fetch_task_pane_content(&mut self) {
        self.pane_content.clear();
        let (visible, _) = visible_tasks(&self.app_state.tasks, &self.worktrees, self.backlog_page);
        if let Some(vt) = visible.get(self.cursor) {
            if let Some(ref wt_path) = vt.task.worktree {
                if let Some(wt) = self.worktrees.iter().find(|w| &w.path == wt_path) {
                    self.fetch_pane_content_for_worktree(wt);
                }
            }
        }
    }

    /// Returns the id of the task under the cursor, if any.
    fn task_id_at_cursor(&self) -> Option<String> {
        let (visible, _) = visible_tasks(&self.app_state.tasks, &self.worktrees, self.backlog_page);
        visible.get(self.cursor).map(|vt| vt.task.id.clone())
    }

    /// Marks the task under the cursor as Done, saves state, and logs the event.
    fn mark_task_done(&mut self) {
        let task_id = match self.task_id_at_cursor() {
            Some(id) => id,
            None => return,
        };
        let from_status = {
            let task = self.app_state.tasks.iter().find(|t| t.id == task_id);
            match task {
                Some(t) => task_status_str(t.status),
                None => return,
            }
        };
        if let Some(task) = self.app_state.tasks.iter_mut().find(|t| t.id == task_id) {
            task.status = crate::state::TaskStatus::Done;
            task.updated_at = chrono::Utc::now();
        }
        if let Err(e) = crate::state::save_state(&self.app_state) {
            crate::logger::LOG.info(&format!("tui: save_state failed: {e}"));
        }
        crate::events::log_task_status_change(&task_id, from_status, "done", "keypress");
        // Clamp cursor so it doesn't point past the end after removal from view.
        let (visible, _) = visible_tasks(&self.app_state.tasks, &self.worktrees, self.backlog_page);
        if !visible.is_empty() && self.cursor >= visible.len() {
            self.cursor = visible.len() - 1;
        }
    }

    /// Promotes the task under the cursor to InProgress, saves state, and logs the event.
    fn start_task(&mut self) {
        let task_id = match self.task_id_at_cursor() {
            Some(id) => id,
            None => return,
        };
        let from_status = {
            let task = self.app_state.tasks.iter().find(|t| t.id == task_id);
            match task {
                Some(t) => task_status_str(t.status),
                None => return,
            }
        };
        if let Some(task) = self.app_state.tasks.iter_mut().find(|t| t.id == task_id) {
            task.status = crate::state::TaskStatus::InProgress;
            task.updated_at = chrono::Utc::now();
        }
        if let Err(e) = crate::state::save_state(&self.app_state) {
            crate::logger::LOG.info(&format!("tui: save_state failed: {e}"));
        }
        crate::events::log_task_status_change(&task_id, from_status, "in_progress", "keypress");
    }

    pub(crate) fn render_list(&self, f: &mut Frame) {
        // Delegate to task-centric view when tasks are available.
        if !self.app_state.tasks.is_empty() {
            self.render_task_list(f);
            return;
        }

        let area = f.area();
        let width = area.width as usize;
        let hdr_height = header_height(area.height);

        // Error state
        if let Some(ref err) = self.error {
            let chunks = Layout::default()
                .direction(Direction::Vertical)
                .constraints([Constraint::Length(hdr_height), Constraint::Length(1), Constraint::Min(3)])
                .split(area);
            self.render_header(f, chunks[0]);
            let err_para = Paragraph::new(err.as_str())
                .style(Style::default().fg(Color::Red))
                .block(
                    Block::default()
                        .borders(Borders::ALL)
                        .border_style(Style::default().fg(Color::Red))
                        .border_type(BorderType::Rounded),
                )
                .wrap(Wrap { trim: true });
            f.render_widget(err_para, chunks[2]);
            return;
        }

        // Loading state
        if self.loading && self.worktrees.is_empty() {
            let chunks = Layout::default()
                .direction(Direction::Vertical)
                .constraints([Constraint::Length(hdr_height), Constraint::Length(1), Constraint::Min(3)])
                .split(area);
            self.render_header(f, chunks[0]);
            let spinner = SPINNER_FRAMES[self.spinner_frame];
            let loading_text = format!("{} Loading worktrees...", spinner);
            let para = Paragraph::new(loading_text)
                .style(Style::default().fg(Color::Cyan))
                .alignment(Alignment::Center);
            f.render_widget(para, chunks[2]);
            return;
        }

        // Empty state
        if self.worktrees.is_empty() {
            let chunks = Layout::default()
                .direction(Direction::Vertical)
                .constraints([
                    Constraint::Length(hdr_height),
                    Constraint::Length(1),
                    Constraint::Min(3),
                    Constraint::Length(1),
                ])
                .split(area);
            self.render_header(f, chunks[0]);
            let empty = Paragraph::new("No worktrees found.")
                .style(Style::default().fg(Color::Yellow))
                .block(
                    Block::default()
                        .borders(Borders::ALL)
                        .border_style(Style::default().fg(Color::Yellow))
                        .border_type(BorderType::Rounded),
                )
                .alignment(Alignment::Center);
            f.render_widget(empty, chunks[2]);
            self.render_hints(f, chunks[3]);
            return;
        }

        // Calculate preview height (+2 for borders, +1 for column header row)
        let list_height = (self.worktrees.len() as u16) + 3;
        let has_preview = !self.pane_content.is_empty()
            && self.cursor < self.worktrees.len()
            && self.worktrees[self.cursor].tmux_session.is_some();

        let has_warning = self
            .warning
            .as_ref()
            .is_some_and(|(_, t)| t.elapsed().as_secs() < WARNING_DURATION_SECS);

        let mut constraints = vec![
            Constraint::Length(hdr_height), // header
            Constraint::Length(1),          // spacer
            Constraint::Length(list_height), // worktree list
        ];

        if has_preview {
            constraints.push(Constraint::Length(1)); // spacer
            constraints.push(Constraint::Min(4));    // preview fills remaining
        }

        if has_warning {
            constraints.push(Constraint::Length(1)); // warning
        }

        constraints.push(Constraint::Length(1)); // hints

        // If no preview, add remainder absorber between list and hints
        if !has_preview {
            let hints_idx = constraints.len() - 1;
            constraints.insert(hints_idx, Constraint::Min(0));
        }

        let chunks = Layout::default()
            .direction(Direction::Vertical)
            .constraints(constraints)
            .split(area);

        let mut chunk_idx = 0;

        // Header
        self.render_header(f, chunks[chunk_idx]);
        chunk_idx += 1;

        // Spacer
        chunk_idx += 1;

        // Worktree list
        self.render_worktree_list(f, chunks[chunk_idx], width);
        chunk_idx += 1;

        // Preview
        if has_preview {
            chunk_idx += 1; // spacer
            self.render_preview(f, chunks[chunk_idx]);
            chunk_idx += 1;
        }

        // Warning
        if has_warning {
            if let Some((ref msg, _)) = self.warning {
                let warn = Paragraph::new(msg.as_str())
                    .style(Style::default().fg(Color::Yellow))
                    .alignment(Alignment::Center);
                f.render_widget(warn, chunks[chunk_idx]);
            }
            chunk_idx += 1;
        }

        // Hints
        self.render_hints(f, chunks[chunk_idx]);
    }

    pub(crate) fn render_header(&self, f: &mut Frame, area: Rect) {
        if header_height(f.area().height) == 1 {
            let line = Line::from(vec![
                Span::styled(
                    "🌳 Git Orchard",
                    Style::default()
                        .fg(Color::Green)
                        .add_modifier(Modifier::BOLD),
                ),
                Span::styled(
                    "  r:refresh  ?:help",
                    Style::default().fg(Color::DarkGray),
                ),
            ]);
            f.render_widget(Paragraph::new(line), area);
            return;
        }

        let header_text = vec![
            Line::from("🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴"),
            Line::from("┌─┐┬┌┬┐╔═╗╦═╗╔═╗╦ ╦╔═╗╦═╗╔╦╗"),
            Line::from("│ ┬│ │ ║ ║╠╦╝║  ╠═╣╠═╣╠╦╝ ║║"),
            Line::from("└─┘┴ ┴ ╚═╝╩╚═╚═╝╩ ╩╩ ╩╩╚══╩╝"),
            Line::from("🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴"),
        ];
        let header = Paragraph::new(header_text)
            .alignment(Alignment::Center)
            .style(
                Style::default()
                    .fg(Color::Green)
                    .add_modifier(Modifier::BOLD),
            )
            .block(
                Block::default()
                    .borders(Borders::ALL)
                    .border_style(Style::default().fg(Color::Green))
                    .border_type(BorderType::Rounded),
            );
        f.render_widget(header, area);
    }

    pub(crate) fn render_worktree_list(&self, f: &mut Frame, area: Rect, _term_width: usize) {
        let has_remote = self.worktrees.iter().any(|wt| wt.remote.is_some());

        // Compute actual content widths for dynamic columns
        let max_path_len = self
            .worktrees
            .iter()
            .map(|wt| paths::tildify(&wt.path).len())
            .max()
            .unwrap_or(10);
        let max_branch_len = self
            .worktrees
            .iter()
            .map(|wt| wt.branch.as_deref().unwrap_or("(detached)").len())
            .max()
            .unwrap_or(10);
        let max_session_len = self
            .worktrees
            .iter()
            .filter_map(|wt| wt.tmux_session.as_ref())
            .map(|s| s.len() + 2) // +2 for icon prefix
            .max()
            .unwrap_or(10);

        // Build column constraints — use content-based sizing with Fill for the two big columns
        let mut widths = vec![
            Constraint::Length(4),                                        // cursor+index
            Constraint::Max(max_path_len as u16 + 1),                    // path: fit content, shrink if needed
            Constraint::Max(max_branch_len as u16 + 1),                  // branch: fit content, shrink if needed
            Constraint::Length(12),                                       // status
            Constraint::Length(8),                                        // claude
        ];
        if has_remote {
            widths.push(Constraint::Length(14));                          // remote
        }
        widths.push(Constraint::Max(max_session_len as u16 + 1));        // session: fit content

        // Header
        let header_style = Style::default()
            .fg(Color::DarkGray)
            .add_modifier(Modifier::BOLD);
        let mut header_cells = vec![
            Cell::from("  #"),
            Cell::from("PATH"),
            Cell::from("BRANCH"),
            Cell::from("STATUS"),
            Cell::from("CLAUDE"),
        ];
        if has_remote {
            header_cells.push(Cell::from("REMOTE"));
        }
        header_cells.push(Cell::from("SESSION"));
        let header_row = Row::new(header_cells).style(header_style);

        // Data rows
        let rows: Vec<Row> = self
            .worktrees
            .iter()
            .enumerate()
            .map(|(i, wt)| {
                let selected = i == self.cursor;
                let cursor_char = if selected { ">" } else { " " };
                let idx_cell = Cell::from(format!("{}{:>2}", cursor_char, i + 1));

                // Path
                let path_display = paths::tildify(&wt.path);
                let path_cell = Cell::from(path_display);

                // Branch
                let branch_str = wt.branch.as_deref().unwrap_or("(detached)").to_string();
                let branch_cell =
                    Cell::from(branch_str).style(Style::default().fg(Color::Yellow));

                // Status
                let badge = status_badge(wt, self.refreshing);
                let status_cell = Cell::from(badge.text).style(badge.style);

                // Claude
                let cbadge = claude_badge(wt);
                let claude_cell = Cell::from(cbadge.text).style(cbadge.style);

                // Session
                let tmux_str = if let Some(ref sess) = wt.tmux_session {
                    let icon = if wt.tmux_attached {
                        "\u{25b6}"
                    } else {
                        "\u{25fc}"
                    };
                    format!("{} {}", icon, sess)
                } else {
                    String::new()
                };
                let tmux_style = if wt.tmux_session.is_some() {
                    if wt.tmux_attached {
                        Style::default().fg(Color::Green)
                    } else {
                        Style::default().fg(Color::Blue)
                    }
                } else {
                    Style::default().fg(Color::DarkGray)
                };
                let tmux_cell = Cell::from(tmux_str).style(tmux_style);

                let mut cells =
                    vec![idx_cell, path_cell, branch_cell, status_cell, claude_cell];
                if has_remote {
                    let remote_str = wt
                        .remote
                        .as_ref()
                        .map(|h| format!("@{}", h))
                        .unwrap_or_default();
                    cells.push(
                        Cell::from(remote_str).style(Style::default().fg(Color::Magenta)),
                    );
                }
                cells.push(tmux_cell);

                let row = Row::new(cells);
                if selected {
                    row.style(
                        Style::default()
                            .fg(Color::Cyan)
                            .add_modifier(Modifier::BOLD),
                    )
                } else {
                    row
                }
            })
            .collect();

        let block = Block::default()
            .title(" WORKTREES ")
            .title_style(
                Style::default()
                    .fg(Color::Cyan)
                    .add_modifier(Modifier::BOLD),
            )
            .borders(Borders::ALL)
            .border_style(Style::default().fg(Color::Cyan))
            .border_type(BorderType::Rounded);

        let table = Table::new(rows, &widths)
            .header(header_row)
            .block(block)
            .column_spacing(1);

        f.render_widget(table, area);
    }

    pub(crate) fn render_preview(&self, f: &mut Frame, area: Rect) {
        if self.pane_content.is_empty()
            || self.worktrees.is_empty()
            || self.cursor >= self.worktrees.len()
        {
            return;
        }
        let wt = &self.worktrees[self.cursor];
        if wt.tmux_session.is_none() {
            return;
        }

        let branch_label = wt.branch.as_deref().unwrap_or("(detached)");
        let title = format!(" PREVIEW \u{2014} {} ", branch_label);

        let block = Block::default()
            .title(title)
            .title_style(
                Style::default()
                    .fg(Color::DarkGray)
                    .add_modifier(Modifier::BOLD),
            )
            .borders(Borders::ALL)
            .border_style(Style::default().fg(Color::DarkGray))
            .border_type(BorderType::Double);

        // Truncate content lines to fit
        let inner_height = area.height.saturating_sub(2) as usize;
        let all_lines: Vec<&str> = self.pane_content.lines().collect();
        let display_lines = if all_lines.len() > inner_height {
            &all_lines[all_lines.len() - inner_height..]
        } else {
            &all_lines
        };
        let content = display_lines.join("\n");

        let preview = Paragraph::new(content)
            .style(Style::default().fg(Color::DarkGray))
            .block(block);
        f.render_widget(preview, area);
    }

    pub(crate) fn render_hints(&self, f: &mut Frame, area: Rect) {
        let sep = Span::styled(" \u{2502} ", Style::default().fg(Color::DarkGray));

        let mut spans: Vec<Span> = vec![
            Span::styled(
                "1-9",
                Style::default()
                    .fg(Color::Cyan)
                    .add_modifier(Modifier::BOLD),
            ),
            Span::raw(" jump"),
            sep.clone(),
            Span::styled(
                "enter",
                Style::default()
                    .fg(Color::Cyan)
                    .add_modifier(Modifier::BOLD),
            ),
            Span::raw(" tmux"),
        ];

        // PR link hint
        let has_pr_url = !self.worktrees.is_empty()
            && self.cursor < self.worktrees.len()
            && self.worktrees[self.cursor]
                .pr
                .as_ref()
                .is_some_and(|pr| !pr.url.is_empty());
        spans.push(sep.clone());
        if has_pr_url {
            spans.push(Span::styled(
                "o",
                Style::default()
                    .fg(Color::Cyan)
                    .add_modifier(Modifier::BOLD),
            ));
            spans.push(Span::raw(" pr"));
        } else {
            spans.push(Span::styled(
                "o pr",
                Style::default().fg(Color::DarkGray),
            ));
        }

        // Transfer hint
        if self.config.remote.is_some() {
            spans.push(sep.clone());
            let is_remote = !self.worktrees.is_empty()
                && self.cursor < self.worktrees.len()
                && self.worktrees[self.cursor].remote.is_some();
            if is_remote {
                spans.push(Span::styled(
                    "p",
                    Style::default()
                        .fg(Color::Cyan)
                        .add_modifier(Modifier::BOLD),
                ));
                spans.push(Span::raw(" pull"));
            } else {
                spans.push(Span::styled(
                    "p",
                    Style::default()
                        .fg(Color::Cyan)
                        .add_modifier(Modifier::BOLD),
                ));
                spans.push(Span::raw(" push"));
            }
        }

        spans.push(sep.clone());
        spans.push(Span::styled(
            "d",
            Style::default()
                .fg(Color::Cyan)
                .add_modifier(Modifier::BOLD),
        ));
        spans.push(Span::raw(" delete"));

        spans.push(sep.clone());
        spans.push(Span::styled(
            "c",
            Style::default()
                .fg(Color::Cyan)
                .add_modifier(Modifier::BOLD),
        ));
        spans.push(Span::raw(" cleanup"));

        spans.push(sep.clone());
        spans.push(Span::styled(
            "n",
            Style::default()
                .fg(Color::Cyan)
                .add_modifier(Modifier::BOLD),
        ));
        spans.push(Span::raw(" new"));

        spans.push(sep.clone());
        if self.refreshing {
            let spinner = SPINNER_FRAMES[self.spinner_frame];
            spans.push(Span::styled(
                format!("{} refreshing...", spinner),
                Style::default().fg(Color::Cyan),
            ));
        } else {
            spans.push(Span::styled(
                "r",
                Style::default()
                    .fg(Color::Cyan)
                    .add_modifier(Modifier::BOLD),
            ));
            spans.push(Span::raw(" refresh"));
        }

        spans.push(sep);
        spans.push(Span::styled(
            "q",
            Style::default()
                .fg(Color::Cyan)
                .add_modifier(Modifier::BOLD),
        ));
        spans.push(Span::raw(" back"));

        let hints = Paragraph::new(Line::from(spans)).alignment(Alignment::Center);
        f.render_widget(hints, area);
    }

    // -------------------------------------------------------------------
    // Actions triggered from list view
    // -------------------------------------------------------------------

    fn selected_worktree(&self) -> Option<&Worktree> {
        self.worktrees.get(self.cursor)
    }

    /// Ensures the tmux session for the selected worktree exists (creating it if needed),
    /// stores the session name in `switch_target`, and returns `true` so the event loop
    /// breaks and the TUI exits. The session name is then printed to stdout by `main` for
    /// the wrapper script to call `tmux switch-client`.
    fn switch_to_tmux_session(&mut self) -> bool {
        let wt = match self.selected_worktree() {
            Some(wt) => wt.clone(),
            None => return false,
        };
        let repo_name = self.repo_name.clone();

        if let Some(ref host) = wt.remote {
            let session_name = wt
                .tmux_session
                .clone()
                .or_else(|| {
                    wt.branch.as_ref().map(|b| {
                        tmux::derive_session_name(&repo_name, Some(b), &wt.path)
                    })
                })
                .unwrap_or_default();
            if session_name.is_empty() {
                return false;
            }
            let shell = self
                .config
                .remote
                .as_ref()
                .map(|r| r.shell.clone())
                .unwrap_or_else(|| "ssh".to_string());

            // Create the remote session and local proxy (without switching).
            match remote::create_remote_proxy_session(host, &session_name, &wt.path, &shell) {
                Ok(local_name) => {
                    self.switch_target = Some(local_name);
                    true
                }
                Err(e) => {
                    self.warning = Some((format!("remote session error: {e}"), Instant::now()));
                    false
                }
            }
        } else {
            let session_name = wt.tmux_session.clone().unwrap_or_else(|| {
                tmux::derive_session_name(&repo_name, wt.branch.as_deref(), &wt.path)
            });
            // Create local session if it doesn't exist (without switching).
            let opts = crate::types::SwitchToSessionOptions {
                session_name: session_name.clone(),
                worktree_path: wt.path.clone(),
                branch: wt.branch.clone(),
                pr: wt.pr.clone(),
            };
            match tmux::create_session(&opts) {
                Ok(()) => {
                    self.switch_target = Some(session_name);
                    true
                }
                Err(e) => {
                    self.warning = Some((format!("session error: {e}"), Instant::now()));
                    false
                }
            }
        }
    }

    fn open_pr_url(&self) {
        let wt = match self.selected_worktree() {
            Some(wt) => wt,
            None => return,
        };
        if let Some(ref pr) = wt.pr {
            if !pr.url.is_empty() {
                crate::browser::open_url(&pr.url);
            }
        }
    }

    fn start_delete_dialog(&mut self) {
        let wt = match self.selected_worktree().cloned() {
            Some(wt) => wt,
            None => return,
        };
        if wt.is_bare {
            self.warning = Some((
                "Cannot delete the bare worktree.".to_string(),
                Instant::now(),
            ));
            return;
        }
        self.view = ViewState::ConfirmDelete(DeleteState {
            target: wt,
            phase: Phase::Confirm,
            error: None,
        });
    }

    fn start_transfer_dialog(&mut self) {
        if self.worktrees.is_empty()
            || self.cursor >= self.worktrees.len()
            || self.config.remote.is_none()
        {
            return;
        }
        let wt = &self.worktrees[self.cursor];
        if wt.is_bare || wt.branch.is_none() {
            self.warning = Some((
                "Cannot transfer: no branch.".to_string(),
                Instant::now(),
            ));
            return;
        }
        self.view = ViewState::Transfer(TransferState {
            target: wt.clone(),
            phase: Phase::Confirm,
            error: None,
        });
    }

    fn enter_cleanup_view(&mut self) {
        let stale = filter_stale(&self.worktrees);
        let selected = stale.iter().map(|wt| wt.path.clone()).collect::<HashSet<_>>();
        self.view = ViewState::Cleanup(CleanupState {
            stale,
            selected,
            cursor: 0,
            phase: Phase::Idle,
            deleted: Vec::new(),
            errors: Vec::new(),
        });
    }
}

// ---------------------------------------------------------------------------
// Task-centric rendering
// ---------------------------------------------------------------------------

impl App {
    /// Renders the task-grouped view. Called by `render_list` when tasks are present.
    pub(crate) fn render_task_list(&self, f: &mut Frame) {
        let area = f.area();
        let hdr_height = header_height(area.height);

        let (tasks, total_backlog) = visible_tasks(&self.app_state.tasks, &self.worktrees, self.backlog_page);

        // Build rows for the table, including section header rows.
        let (rows, row_heights) = self.build_task_table_rows(&tasks, total_backlog);

        let has_warning = self
            .warning
            .as_ref()
            .is_some_and(|(_, t)| t.elapsed().as_secs() < WARNING_DURATION_SECS);

        // Calculate total table body height from individual row heights
        let body_height: u16 = row_heights.iter().sum::<u16>();
        let table_height = body_height.saturating_add(3); // +2 borders +1 header row

        // Check if selected task has a preview
        let selected_task = tasks.get(self.cursor);
        let has_preview = selected_task.is_some_and(|vt| {
            !self.pane_content.is_empty()
                && vt.task.worktree.as_ref().and_then(|path| {
                    self.worktrees.iter().find(|w| &w.path == path)
                }).is_some_and(|wt| wt.tmux_session.is_some())
        });

        let mut constraints = vec![
            Constraint::Length(hdr_height),
            Constraint::Length(1), // spacer
            Constraint::Length(table_height),
        ];

        if has_preview {
            constraints.push(Constraint::Length(1)); // spacer
            constraints.push(Constraint::Min(4));    // preview fills remaining
        }

        if has_warning {
            constraints.push(Constraint::Length(1));
        }

        // If no preview, add remainder absorber between table and hints
        if !has_preview {
            constraints.push(Constraint::Min(0));
        }

        constraints.push(Constraint::Length(1)); // hints

        let chunks = Layout::default()
            .direction(Direction::Vertical)
            .constraints(constraints)
            .split(area);

        let mut idx = 0;
        self.render_header(f, chunks[idx]);
        idx += 1;
        idx += 1; // spacer

        // Column widths
        let widths = [
            Constraint::Length(3),   // #
            Constraint::Length(7),   // ISSUE
            Constraint::Min(20),     // TITLE (flexible)
            Constraint::Length(12),  // HOST
            Constraint::Length(18),  // STATUS
            Constraint::Length(10),  // CLAUDE
        ];

        // Header row
        let header_style = Style::default()
            .fg(Color::DarkGray)
            .add_modifier(Modifier::BOLD);
        let header_cells = vec![
            Cell::from(" #"),
            Cell::from("ISSUE"),
            Cell::from("TITLE"),
            Cell::from("HOST"),
            Cell::from("STATUS"),
            Cell::from("CLAUDE"),
        ];
        let header_row = Row::new(header_cells).style(header_style);

        let block = Block::default()
            .title(" TASKS ")
            .title_style(Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD))
            .borders(Borders::ALL)
            .border_style(Style::default().fg(Color::Cyan))
            .border_type(BorderType::Rounded);

        let table = Table::new(rows, &widths)
            .header(header_row)
            .block(block)
            .column_spacing(1);

        f.render_widget(table, chunks[idx]);
        idx += 1;

        // Preview
        if has_preview {
            idx += 1; // spacer
            self.render_task_preview(f, chunks[idx], selected_task);
            idx += 1;
        }

        if has_warning {
            if let Some((ref msg, _)) = self.warning {
                let warn = Paragraph::new(msg.as_str())
                    .style(Style::default().fg(Color::Yellow))
                    .alignment(Alignment::Center);
                f.render_widget(warn, chunks[idx]);
            }
            idx += 1;
        }

        if !has_preview {
            idx += 1; // absorber
        }

        self.render_hints_task(f, chunks[idx]);
    }

    fn build_task_table_rows(
        &self,
        tasks: &[VisibleTask],
        total_backlog: usize,
    ) -> (Vec<Row<'static>>, Vec<u16>) {
        let mut rows: Vec<Row<'static>> = Vec::new();
        let mut row_heights: Vec<u16> = Vec::new();
        let mut last_group: Option<DisplayGroup> = None;

        let backlog_count = tasks.iter().filter(|t| t.group == DisplayGroup::Backlog).count();
        let backlog_has_more = total_backlog > backlog_count + self.backlog_page * BACKLOG_PAGE_SIZE;

        for (flat_idx, vt) in tasks.iter().enumerate() {
            let selected = flat_idx == self.cursor;

            // Section header when display group changes
            if last_group != Some(vt.group) {
                last_group = Some(vt.group);
                let header_row = group_header_row(vt.group, backlog_count, total_backlog, backlog_has_more);
                rows.push(header_row);
                row_heights.push(1);
            }

            let issue = issue_number_from_task(vt.task);
            let (pr_text, pr_style) = pr_status_text(vt.task, &self.worktrees);
            let (claude_text, claude_style) = claude_status_text(vt.task, &self.worktrees);

            let host_text = vt.task.remote_host
                .as_ref()
                .map(|h| format!("@{}", h))
                .unwrap_or_default();

            let title_display = if vt.task.title.is_empty() {
                vt.task.id.clone()
            } else {
                vt.task.title.clone()
            };

            let row_style = if selected {
                Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD)
            } else {
                Style::default()
            };

            let cells = vec![
                Cell::from(format!("{:>2}", vt.num)),
                Cell::from(format!("#{}", issue)),
                Cell::from(title_display),
                Cell::from(host_text).style(Style::default().fg(Color::Magenta)),
                Cell::from(pr_text).style(pr_style),
                Cell::from(claude_text).style(claude_style),
            ];

            rows.push(Row::new(cells).style(row_style));
            row_heights.push(1);
        }

        // Edge case: empty visible list but backlog exists
        if last_group.is_none() && total_backlog > 0 {
            let header_row = group_header_row(DisplayGroup::Backlog, 0, total_backlog, backlog_has_more);
            rows.push(header_row);
            row_heights.push(1);
            rows.push(Row::new(vec![
                Cell::from(""),
                Cell::from(""),
                Cell::from("(no tasks on this page)").style(Style::default().fg(Color::DarkGray)),
                Cell::from(""),
                Cell::from(""),
                Cell::from(""),
            ]));
            row_heights.push(1);
        }

        (rows, row_heights)
    }

    /// Renders the preview pane with task metadata in the border title.
    fn render_task_preview(&self, f: &mut Frame, area: Rect, selected_task: Option<&VisibleTask>) {
        if self.pane_content.is_empty() {
            return;
        }
        let Some(vt) = selected_task else { return };

        let issue = issue_number_from_task(vt.task);
        let title_part = if vt.task.title.is_empty() {
            String::new()
        } else {
            format!(" {}", truncate_str(&vt.task.title, 30))
        };
        let wt_part = vt.task.worktree.as_ref().map(|p| {
            let short = paths::tildify(p);
            format!(" \u{2502} wt: {}", truncate_str(&short, 25))
        }).unwrap_or_default();
        let pr_part = vt.task.pr.map(|n| format!(" \u{2502} pr: #{}", n)).unwrap_or_default();

        let title = format!("\u{2500}\u{2500} #{}{}{}{} \u{2500}\u{2500}",
            issue, title_part, wt_part, pr_part);

        let block = Block::default()
            .title(title)
            .title_style(
                Style::default()
                    .fg(Color::DarkGray)
                    .add_modifier(Modifier::BOLD),
            )
            .borders(Borders::ALL)
            .border_style(Style::default().fg(Color::DarkGray))
            .border_type(BorderType::Double);

        // Truncate content lines to fit
        let inner_height = area.height.saturating_sub(2) as usize;
        let all_lines: Vec<&str> = self.pane_content.lines().collect();
        let display_lines = if all_lines.len() > inner_height {
            &all_lines[all_lines.len() - inner_height..]
        } else {
            &all_lines
        };
        let content = display_lines.join("\n");

        let preview = Paragraph::new(content)
            .style(Style::default().fg(Color::DarkGray))
            .block(block);
        f.render_widget(preview, area);
    }

    /// Renders the hint bar for task mode.
    pub(crate) fn render_hints_task(&self, f: &mut Frame, area: Rect) {
        let key_style = Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD);
        let dim_style = Style::default().fg(Color::DarkGray);

        let mut spans: Vec<Span> = vec![
            Span::styled("enter", key_style),
            Span::styled(":switch  ", dim_style),
            Span::styled("o", key_style),
            Span::styled(":open PR  ", dim_style),
            Span::styled("s", key_style),
            Span::styled(":start  ", dim_style),
            Span::styled("p", key_style),
            Span::styled(":priority  ", dim_style),
            Span::styled("o", key_style),
            Span::styled(":open PR  ", dim_style),
            Span::styled("c", key_style),
            Span::styled(":cleanup  ", dim_style),
        ];

        if self.refreshing {
            let spinner = SPINNER_FRAMES[self.spinner_frame];
            spans.push(Span::styled(
                format!("{} refreshing...  ", spinner),
                Style::default().fg(Color::Cyan),
            ));
        } else {
            spans.push(Span::styled("r", key_style));
            spans.push(Span::styled(":refresh  ", dim_style));
        }

        spans.push(Span::styled("q", key_style));
        spans.push(Span::styled(":quit", dim_style));

        let hints = Paragraph::new(Line::from(spans)).alignment(Alignment::Center);
        f.render_widget(hints, area);
    }
}

// ---------------------------------------------------------------------------
// Task view helpers (free functions)
// ---------------------------------------------------------------------------

/// Returns the snake_case string representation of a TaskStatus for event logging.
fn task_status_str(status: TaskStatus) -> &'static str {
    match status {
        TaskStatus::Backlog => "backlog",
        TaskStatus::Ready => "ready",
        TaskStatus::InProgress => "in_progress",
        TaskStatus::InReview => "in_review",
        TaskStatus::Done => "done",
    }
}

/// Creates a section header row spanning all columns for a display group.
fn group_header_row(
    group: DisplayGroup,
    visible_backlog: usize,
    total_backlog: usize,
    has_more: bool,
) -> Row<'static> {
    let label = if group == DisplayGroup::Backlog {
        let count_info = if visible_backlog < total_backlog {
            format!("{} of {}", visible_backlog, total_backlog)
        } else {
            format!("{}", total_backlog)
        };
        let hint = if has_more { "  j/k \u{25bc}\u{25b2}" } else { "" };
        format!("{} ({}){}", group.label(), count_info, hint)
    } else {
        group.label().to_string()
    };

    // ──── label ────
    let line_char = "\u{2500}";
    let padded = format!(" {} ", label);
    let side_len = 8;
    let text = format!(
        "{}{}{}",
        line_char.repeat(side_len),
        padded,
        line_char.repeat(40)
    );

    let color = group.color();
    Row::new(vec![
        Cell::from(""),
        Cell::from(""),
        Cell::from(text).style(Style::default().fg(color)),
        Cell::from(""),
        Cell::from(""),
        Cell::from(""),
    ])
}

fn truncate_str(s: &str, max: usize) -> String {
    let chars: Vec<char> = s.chars().collect();
    if chars.len() <= max {
        s.to_string()
    } else {
        let truncated: String = chars[..max.saturating_sub(1)].iter().collect();
        format!("{}…", truncated)
    }
}

/// Returns the height (in terminal rows) to allocate for the header.
///
/// When the terminal is tall enough (>= 30 rows), the full ASCII art logo is
/// shown in a bordered block (7 rows).  On shorter terminals a single compact
/// line is used instead so the task list gets as much vertical space as
/// possible.
pub(crate) fn header_height(terminal_height: u16) -> u16 {
    if terminal_height >= 30 { 7 } else { 1 }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::{Task, TaskSource, TaskStatus};
    use crate::types::{ChecksStatus, PrInfo, ReviewDecision, Worktree};
    use chrono::Utc;

    fn make_task(status: TaskStatus, priority: u32, issue: u32) -> Task {
        Task {
            id: format!("repo#{}", issue),
            title: format!("Test task {}", issue),
            source: TaskSource::GithubIssue {
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
    fn full_logo_at_threshold() {
        assert_eq!(header_height(30), 7);
    }

    #[test]
    fn full_logo_above_threshold() {
        assert_eq!(header_height(50), 7);
    }

    #[test]
    fn compact_header_just_below_threshold() {
        assert_eq!(header_height(29), 1);
    }

    #[test]
    fn compact_header_on_very_short_terminal() {
        assert_eq!(header_height(10), 1);
    }

    #[test]
    fn tasks_sorted_by_priority_within_group() {
        // Both tasks have no worktree → both Backlog
        let tasks = vec![
            make_task(TaskStatus::Backlog, 2, 47),
            make_task(TaskStatus::Backlog, 1, 48),
        ];
        let (visible, _) = visible_tasks(&tasks, &[], 0);
        assert_eq!(visible[0].num, 1);
        let first_issue = match &visible[0].task.source {
            TaskSource::GithubIssue { number, .. } => *number,
        };
        assert_eq!(first_issue, 48, "priority 1 task should come first");
    }

    #[test]
    fn same_priority_sorted_by_issue_number() {
        let tasks = vec![
            make_task(TaskStatus::Backlog, 1, 52),
            make_task(TaskStatus::Backlog, 1, 48),
        ];
        let (visible, _) = visible_tasks(&tasks, &[], 0);
        let first_issue = match &visible[0].task.source {
            TaskSource::GithubIssue { number, .. } => *number,
        };
        assert_eq!(first_issue, 48, "lower issue number should come first within same priority");
    }

    #[test]
    fn done_tasks_excluded_from_visible() {
        let tasks = vec![
            make_task(TaskStatus::Done, 1, 10),
            make_task(TaskStatus::Backlog, 1, 11),
        ];
        let (visible, _) = visible_tasks(&tasks, &[], 0);
        assert_eq!(visible.len(), 1);
        let issue = match &visible[0].task.source {
            TaskSource::GithubIssue { number, .. } => *number,
        };
        assert_eq!(issue, 11);
    }

    #[test]
    fn backlog_pagination_first_page() {
        let tasks: Vec<Task> = (1u32..=25)
            .map(|i| make_task(TaskStatus::Backlog, 1, i))
            .collect();
        let (visible, total) = visible_tasks(&tasks, &[], 0);
        assert_eq!(total, 25);
        assert_eq!(visible.len(), BACKLOG_PAGE_SIZE);
    }

    #[test]
    fn backlog_pagination_second_page() {
        let tasks: Vec<Task> = (1u32..=25)
            .map(|i| make_task(TaskStatus::Backlog, 1, i))
            .collect();
        let (visible, total) = visible_tasks(&tasks, &[], 1);
        assert_eq!(total, 25);
        assert_eq!(visible.len(), BACKLOG_PAGE_SIZE);
    }

    #[test]
    fn sequential_numbering_across_groups() {
        // Task with conflicting PR → NeedsYou, task with session → ClaudeDone, task without → Backlog
        let mut task1 = make_task(TaskStatus::InProgress, 1, 10);
        task1.worktree = Some("/ws/10".to_string());
        let mut task2 = make_task(TaskStatus::InProgress, 1, 20);
        task2.worktree = Some("/ws/20".to_string());
        let task3 = make_task(TaskStatus::Backlog, 1, 30);

        let worktrees = vec![
            Worktree {
                path: "/ws/10".to_string(),
                pr: Some(PrInfo {
                    number: 11,
                    state: "open".into(),
                    title: String::new(),
                    url: String::new(),
                    review_decision: ReviewDecision::None,
                    unresolved_threads: 0,
                    checks_status: ChecksStatus::Fail,
                    has_conflicts: false,
                }),
                ..Default::default()
            },
            Worktree {
                path: "/ws/20".to_string(),
                tmux_session: Some("sess20".to_string()),
                ..Default::default()
            },
        ];

        let tasks = vec![task1, task2, task3];
        let (visible, _) = visible_tasks(&tasks, &worktrees, 0);
        assert_eq!(visible.len(), 3);
        assert_eq!(visible[0].num, 1);
        assert_eq!(visible[1].num, 2);
        assert_eq!(visible[2].num, 3);
    }

    #[test]
    fn display_group_ordering() {
        // Create tasks that map to different display groups
        let mut needs_you_task = make_task(TaskStatus::InProgress, 1, 1);
        needs_you_task.worktree = Some("/ws/1".to_string());

        let mut claude_working_task = make_task(TaskStatus::InProgress, 1, 2);
        claude_working_task.worktree = Some("/ws/2".to_string());

        let mut claude_done_task = make_task(TaskStatus::InProgress, 1, 3);
        claude_done_task.worktree = Some("/ws/3".to_string());

        let mut in_review_task = make_task(TaskStatus::InReview, 1, 4);
        in_review_task.worktree = Some("/ws/4".to_string());

        let backlog_task = make_task(TaskStatus::Backlog, 1, 5);

        let worktrees = vec![
            Worktree {
                path: "/ws/1".to_string(),
                pr: Some(PrInfo {
                    number: 11, state: "open".into(), title: String::new(), url: String::new(),
                    review_decision: ReviewDecision::ChangesRequested,
                    unresolved_threads: 0, checks_status: ChecksStatus::None, has_conflicts: false,
                }),
                ..Default::default()
            },
            Worktree {
                path: "/ws/2".to_string(),
                tmux_session: Some("sess2".to_string()),
                tmux_pane_title: Some("Claude Code".to_string()),
                ..Default::default()
            },
            Worktree {
                path: "/ws/3".to_string(),
                tmux_session: Some("sess3".to_string()),
                tmux_pane_title: Some("bash".to_string()),
                ..Default::default()
            },
            Worktree {
                path: "/ws/4".to_string(),
                pr: Some(PrInfo {
                    number: 14, state: "open".into(), title: String::new(), url: String::new(),
                    review_decision: ReviewDecision::Approved,
                    unresolved_threads: 0, checks_status: ChecksStatus::Pass, has_conflicts: false,
                }),
                ..Default::default()
            },
        ];

        let tasks = vec![
            backlog_task, in_review_task, claude_done_task, claude_working_task, needs_you_task,
        ];
        let (visible, _) = visible_tasks(&tasks, &worktrees, 0);

        let groups: Vec<DisplayGroup> = visible.iter().map(|v| v.group).collect();
        assert_eq!(groups, vec![
            DisplayGroup::NeedsYou,
            DisplayGroup::ClaudeWorking,
            DisplayGroup::ClaudeDone,
            DisplayGroup::InReview,
            DisplayGroup::Backlog,
        ]);
    }

    #[test]
    fn derive_group_conflict_is_needs_you() {
        let mut task = make_task(TaskStatus::InProgress, 1, 1);
        task.worktree = Some("/ws/1".to_string());
        let worktrees = vec![Worktree {
            path: "/ws/1".to_string(),
            pr: Some(PrInfo {
                number: 1, state: "open".into(), title: String::new(), url: String::new(),
                review_decision: ReviewDecision::None,
                unresolved_threads: 0, checks_status: ChecksStatus::None, has_conflicts: true,
            }),
            ..Default::default()
        }];
        assert_eq!(derive_display_group(&task, &worktrees), DisplayGroup::NeedsYou);
    }

    #[test]
    fn derive_group_approved_pr_is_in_review() {
        let mut task = make_task(TaskStatus::InReview, 1, 1);
        task.worktree = Some("/ws/1".to_string());
        let worktrees = vec![Worktree {
            path: "/ws/1".to_string(),
            pr: Some(PrInfo {
                number: 1, state: "open".into(), title: String::new(), url: String::new(),
                review_decision: ReviewDecision::Approved,
                unresolved_threads: 0, checks_status: ChecksStatus::Pass, has_conflicts: false,
            }),
            ..Default::default()
        }];
        assert_eq!(derive_display_group(&task, &worktrees), DisplayGroup::InReview);
    }

    #[test]
    fn derive_group_claude_active_is_claude_working() {
        let mut task = make_task(TaskStatus::InProgress, 1, 1);
        task.worktree = Some("/ws/1".to_string());
        let worktrees = vec![Worktree {
            path: "/ws/1".to_string(),
            tmux_session: Some("sess".to_string()),
            tmux_pane_title: Some("Claude Code".to_string()),
            ..Default::default()
        }];
        assert_eq!(derive_display_group(&task, &worktrees), DisplayGroup::ClaudeWorking);
    }

    #[test]
    fn derive_group_session_no_claude_is_claude_done() {
        let mut task = make_task(TaskStatus::InProgress, 1, 1);
        task.worktree = Some("/ws/1".to_string());
        let worktrees = vec![Worktree {
            path: "/ws/1".to_string(),
            tmux_session: Some("sess".to_string()),
            tmux_pane_title: Some("bash".to_string()),
            ..Default::default()
        }];
        assert_eq!(derive_display_group(&task, &worktrees), DisplayGroup::ClaudeDone);
    }

    #[test]
    fn derive_group_no_worktree_is_backlog() {
        let task = make_task(TaskStatus::Backlog, 1, 1);
        assert_eq!(derive_display_group(&task, &[]), DisplayGroup::Backlog);
    }

    #[test]
    fn pr_status_approved_text() {
        let mut task = make_task(TaskStatus::InReview, 1, 1);
        task.worktree = Some("/ws/1".to_string());
        let worktrees = vec![Worktree {
            path: "/ws/1".to_string(),
            pr: Some(PrInfo {
                number: 1, state: "open".into(), title: String::new(), url: String::new(),
                review_decision: ReviewDecision::Approved,
                unresolved_threads: 0, checks_status: ChecksStatus::Pass, has_conflicts: false,
            }),
            ..Default::default()
        }];
        let (text, _) = pr_status_text(&task, &worktrees);
        assert!(text.contains("approved"), "expected 'approved' in: {}", text);
    }

    #[test]
    fn pr_status_no_worktree() {
        let task = make_task(TaskStatus::Backlog, 1, 1);
        let (text, _) = pr_status_text(&task, &[]);
        assert_eq!(text, "no PR");
    }

    #[test]
    fn claude_status_active() {
        let mut task = make_task(TaskStatus::InProgress, 1, 1);
        task.worktree = Some("/ws/1".to_string());
        let worktrees = vec![Worktree {
            path: "/ws/1".to_string(),
            tmux_session: Some("sess".to_string()),
            tmux_pane_title: Some("Claude Code".to_string()),
            ..Default::default()
        }];
        let (text, _) = claude_status_text(&task, &worktrees);
        assert!(text.contains("active"), "expected 'active' in: {}", text);
    }

    #[test]
    fn claude_status_idle() {
        let mut task = make_task(TaskStatus::InProgress, 1, 1);
        task.worktree = Some("/ws/1".to_string());
        let worktrees = vec![Worktree {
            path: "/ws/1".to_string(),
            tmux_session: Some("sess".to_string()),
            tmux_pane_title: Some("bash".to_string()),
            ..Default::default()
        }];
        let (text, _) = claude_status_text(&task, &worktrees);
        assert!(text.contains("idle"), "expected 'idle' in: {}", text);
    }

    #[test]
    fn claude_status_none_when_no_session() {
        let task = make_task(TaskStatus::Backlog, 1, 1);
        let (text, _) = claude_status_text(&task, &[]);
        assert!(text.contains("none"), "expected 'none' in: {}", text);
    }
}
