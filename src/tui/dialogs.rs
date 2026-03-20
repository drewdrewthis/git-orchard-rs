use crossterm::event::{KeyCode, KeyEvent};
use ratatui::prelude::*;
use ratatui::widgets::Padding;

use crate::paths;
use crate::tui::state::{
    CleanupState, DeleteState, NewSessionState, Phase, SetPriorityState, TransferState, ViewState,
};
use crate::tui::widgets::render_popup;
use crate::tui::App;
use crate::tui::SPINNER_FRAMES;
use crate::types::SwitchToSessionOptions;

use std::time::Instant;

// ---------------------------------------------------------------------------
// Delete dialog
// ---------------------------------------------------------------------------

impl App {
    pub(crate) fn handle_delete_key(&mut self, state: &mut DeleteState, key: KeyEvent) -> bool {
        match state.phase {
            Phase::Confirm => match key.code {
                KeyCode::Char('y') => {
                    state.phase = Phase::InProgress;
                    self.start_delete(&state.target);
                    false
                }
                KeyCode::Char('n') | KeyCode::Esc => {
                    self.view = ViewState::List;
                    false
                }
                _ => false,
            },
            Phase::Done | Phase::Error => {
                self.view = ViewState::List;
                false
            }
            _ => false,
        }
    }

    pub(crate) fn render_delete(&self, state: &DeleteState, f: &mut Frame) {
        let wt = &state.target;
        let branch_label = wt.branch.as_deref().unwrap_or("(detached)");
        let path_str = paths::tildify(&wt.path);

        let mut lines: Vec<Line> = Vec::new();

        match state.phase {
            Phase::Confirm => {
                lines.push(Line::from(format!(
                    "Delete worktree {} at {}?",
                    branch_label, path_str
                )));
                if let Some(ref pr) = wt.pr {
                    lines.push(Line::from(format!("PR #{} is {}.", pr.number, pr.state)));
                }
                if let Some(ref sess) = wt.tmux_session {
                    lines.push(Line::from(format!(
                        "tmux session {:?} will be killed.",
                        sess
                    )));
                }
                lines.push(Line::from(""));
                lines.push(Line::from(vec![
                    Span::styled("y", Style::default().add_modifier(Modifier::BOLD)),
                    Span::raw(" yes  "),
                    Span::styled("n", Style::default().add_modifier(Modifier::BOLD)),
                    Span::raw(" no"),
                ]));
            }
            Phase::InProgress => {
                let spinner = SPINNER_FRAMES[self.spinner_frame];
                lines.push(Line::from(format!("{} Removing worktree...", spinner)));
            }
            Phase::Done => {
                lines.push(Line::styled(
                    "\u{2713} Worktree deleted.",
                    Style::default().fg(Color::Green),
                ));
                lines.push(Line::from(""));
                lines.push(Line::styled(
                    "Press any key to go back.",
                    Style::default().fg(Color::DarkGray),
                ));
            }
            Phase::Error => {
                let err_msg = state.error.as_deref().unwrap_or("unknown error");
                lines.push(Line::styled(
                    format!("\u{2716} Error: {}", err_msg),
                    Style::default().fg(Color::Red),
                ));
                lines.push(Line::from(""));
                lines.push(Line::styled(
                    "Press any key to go back.",
                    Style::default().fg(Color::DarkGray),
                ));
            }
            Phase::Idle => {}
        }

        render_popup(
            f,
            lines,
            Color::Yellow,
            70,
            12,
            None,
            Padding::new(2, 2, 1, 1),
        );
    }
}

// ---------------------------------------------------------------------------
// Transfer dialog
// ---------------------------------------------------------------------------

impl App {
    pub(crate) fn handle_transfer_key(
        &mut self,
        state: &mut TransferState,
        key: KeyEvent,
    ) -> bool {
        match state.phase {
            Phase::Confirm => match key.code {
                KeyCode::Char('y') => {
                    state.phase = Phase::InProgress;
                    self.start_transfer(&state.target);
                    false
                }
                KeyCode::Char('n') | KeyCode::Esc => {
                    self.view = ViewState::List;
                    false
                }
                _ => false,
            },
            Phase::Done | Phase::Error => {
                self.view = ViewState::List;
                false
            }
            _ => false,
        }
    }

    pub(crate) fn render_transfer(&self, state: &TransferState, f: &mut Frame) {
        let wt = &state.target;
        let branch_label = wt.branch.as_deref().unwrap_or("(detached)");
        let path_str = paths::tildify(&wt.path);
        let direction = if wt.remote.is_some() {
            "pull to local"
        } else {
            "push to remote"
        };

        let mut lines: Vec<Line> = Vec::new();

        match state.phase {
            Phase::Confirm => {
                lines.push(Line::from(format!(
                    "Transfer {} \u{2014} {}",
                    branch_label, direction
                )));
                lines.push(Line::from(format!("from {}", path_str)));
                if wt.tmux_attached {
                    lines.push(Line::styled(
                        "Session is currently attached \u{2014} it will be killed.",
                        Style::default().fg(Color::Yellow),
                    ));
                }
                lines.push(Line::from(""));
                lines.push(Line::from(vec![
                    Span::styled("y", Style::default().add_modifier(Modifier::BOLD)),
                    Span::raw(" yes  "),
                    Span::styled("n", Style::default().add_modifier(Modifier::BOLD)),
                    Span::raw(" no"),
                ]));
            }
            Phase::InProgress => {
                let spinner = SPINNER_FRAMES[self.spinner_frame];
                lines.push(Line::from(format!("{} Transferring...", spinner)));
            }
            Phase::Done => {
                lines.push(Line::styled(
                    "\u{2713} Transfer complete.",
                    Style::default().fg(Color::Green),
                ));
                lines.push(Line::from(""));
                lines.push(Line::styled(
                    "Press any key to continue.",
                    Style::default().fg(Color::DarkGray),
                ));
            }
            Phase::Error => {
                let err_msg = state.error.as_deref().unwrap_or("unknown error");
                lines.push(Line::styled(
                    format!("\u{2716} Error: {}", err_msg),
                    Style::default().fg(Color::Red),
                ));
                lines.push(Line::from(""));
                lines.push(Line::styled(
                    "Press any key to continue.",
                    Style::default().fg(Color::DarkGray),
                ));
            }
            Phase::Idle => {}
        }

        render_popup(
            f,
            lines,
            Color::Yellow,
            70,
            12,
            None,
            Padding::new(2, 2, 1, 1),
        );
    }
}

// ---------------------------------------------------------------------------
// Cleanup dialog
// ---------------------------------------------------------------------------

impl App {
    pub(crate) fn handle_cleanup_key(
        &mut self,
        state: &mut CleanupState,
        key: KeyEvent,
    ) -> bool {
        if state.phase == Phase::Done {
            match key.code {
                KeyCode::Char('q') | KeyCode::Esc => {
                    self.view = ViewState::List;
                }
                _ => {}
            }
            return false;
        }

        if state.phase == Phase::InProgress {
            return false;
        }

        match key.code {
            KeyCode::Up | KeyCode::Char('k') => {
                if state.cursor > 0 {
                    state.cursor -= 1;
                }
            }
            KeyCode::Down | KeyCode::Char('j') => {
                if !state.stale.is_empty() && state.cursor < state.stale.len() - 1 {
                    state.cursor += 1;
                }
            }
            KeyCode::Char(' ') => {
                if !state.stale.is_empty() && state.cursor < state.stale.len() {
                    let path = state.stale[state.cursor].path.clone();
                    if state.selected.contains(&path) {
                        state.selected.remove(&path);
                    } else {
                        state.selected.insert(path);
                    }
                }
            }
            KeyCode::Enter => {
                let selected: Vec<_> = state
                    .stale
                    .iter()
                    .filter(|wt| state.selected.contains(&wt.path))
                    .cloned()
                    .collect();
                if selected.is_empty() {
                    self.warning = Some(("No items selected.".to_string(), Instant::now()));
                } else {
                    state.phase = Phase::InProgress;
                    self.start_cleanup(selected);
                }
            }
            KeyCode::Char('q') | KeyCode::Esc => {
                self.view = ViewState::List;
            }
            _ => {}
        }
        false
    }

    pub(crate) fn render_cleanup(&self, state: &CleanupState, f: &mut Frame) {
        let mut lines: Vec<Line> = Vec::new();

        match state.phase {
            Phase::Done => {
                lines.push(Line::styled(
                    "Cleanup complete",
                    Style::default()
                        .fg(Color::Green)
                        .add_modifier(Modifier::BOLD),
                ));
                lines.push(Line::from(""));
                if !state.deleted.is_empty() {
                    lines.push(Line::from(format!(
                        "Deleted {} worktree(s).",
                        state.deleted.len()
                    )));
                }
                for e in &state.errors {
                    lines.push(Line::styled(
                        format!("\u{2716} {}", e),
                        Style::default().fg(Color::Red),
                    ));
                }
                lines.push(Line::from(""));
                lines.push(Line::styled(
                    "Press q to go back.",
                    Style::default().fg(Color::DarkGray),
                ));
            }
            Phase::InProgress => {
                let spinner = SPINNER_FRAMES[self.spinner_frame];
                lines.push(Line::from(format!("{} Cleaning up...", spinner)));
            }
            _ => {
                lines.push(Line::styled(
                    "Cleanup \u{2014} Stale worktrees (merged/closed PRs, closed issues)",
                    Style::default().add_modifier(Modifier::BOLD),
                ));
                lines.push(Line::styled(
                    "space toggle  enter confirm  q cancel",
                    Style::default().fg(Color::DarkGray),
                ));
                lines.push(Line::from(""));

                if state.stale.is_empty() {
                    lines.push(Line::styled(
                        "No stale worktrees found.",
                        Style::default().fg(Color::Green),
                    ));
                    lines.push(Line::from(""));
                    lines.push(Line::styled(
                        "Press q to go back.",
                        Style::default().fg(Color::DarkGray),
                    ));
                } else {
                    for (i, wt) in state.stale.iter().enumerate() {
                        let cursor_char = if i == state.cursor {
                            "\u{25b8} "
                        } else {
                            "  "
                        };

                        let check = if state.selected.contains(&wt.path) {
                            "[\u{2713}]"
                        } else {
                            "[ ]"
                        };

                        let path_str = paths::truncate_left(&paths::tildify(&wt.path), 40);
                        let branch_str = wt.branch.as_deref().unwrap_or("");

                        let mut parts = format!(
                            "{}{}  {}  {}",
                            cursor_char, check, path_str, branch_str
                        );

                        if let Some(ref pr) = wt.pr {
                            parts.push_str(&format!("  PR #{}", pr.number));
                        } else if let Some(num) = wt.issue_number {
                            parts.push_str(&format!("  issue #{}", num));
                        }

                        if let Some(ref host) = wt.remote {
                            parts.push_str(&format!("  @{}", host));
                        }

                        if i == state.cursor {
                            lines.push(Line::styled(
                                parts,
                                Style::default()
                                    .fg(Color::Cyan)
                                    .add_modifier(Modifier::BOLD),
                            ));
                        } else {
                            lines.push(Line::from(parts));
                        }
                    }
                }
            }
        }

        render_popup(
            f,
            lines,
            Color::Cyan,
            90,
            24,
            None,
            Padding::new(2, 2, 1, 1),
        );
    }
}

// ---------------------------------------------------------------------------
// New session dialog
// ---------------------------------------------------------------------------

impl App {
    pub(crate) fn handle_new_session_key(
        &mut self,
        state: &mut NewSessionState,
        key: KeyEvent,
    ) -> bool {
        match key.code {
            KeyCode::Esc => {
                self.view = ViewState::List;
            }
            KeyCode::Enter => {
                if !state.name.is_empty() {
                    let name = state.name.clone();
                    let worktree_path = self.repo_root.clone();
                    let opts = SwitchToSessionOptions {
                        session_name: name.clone(),
                        worktree_path,
                        branch: None,
                        pr: None,
                    };
                    match crate::tmux::create_session(&opts) {
                        Ok(()) => {
                            self.switch_target = Some(name);
                            return true;
                        }
                        Err(e) => {
                            self.view = ViewState::List;
                            self.warning = Some((
                                format!("session error: {e}"),
                                Instant::now(),
                            ));
                        }
                    }
                }
            }
            KeyCode::Backspace => {
                state.name.pop();
                state.cursor = state.name.len();
            }
            KeyCode::Char(c) => {
                if c.is_alphanumeric() || c == '-' || c == '_' {
                    state.name.push(c);
                    state.cursor = state.name.len();
                }
            }
            _ => {}
        }
        false
    }

    pub(crate) fn render_new_session(&self, state: &NewSessionState, f: &mut Frame) {
        let input_with_cursor = format!("{}\u{2588}", state.name);

        let lines = vec![
            Line::from("Session name:"),
            Line::from(Span::styled(
                input_with_cursor,
                Style::default().fg(Color::Cyan),
            )),
            Line::from(""),
            Line::styled(
                "enter confirm  esc cancel",
                Style::default()
                    .fg(Color::DarkGray)
                    .add_modifier(Modifier::DIM),
            ),
        ];

        render_popup(
            f,
            lines,
            Color::Cyan,
            60,
            7,
            Some(" New Session "),
            Padding::new(2, 2, 0, 0),
        );
    }
}

// ---------------------------------------------------------------------------
// Set priority dialog
// ---------------------------------------------------------------------------

impl App {
    /// Handles key input while in the SetPriority view.
    ///
    /// Digit 1-9: apply priority, save state, return to List.
    /// Escape: cancel, return to List.
    pub(crate) fn handle_set_priority_key(
        &mut self,
        state: &mut SetPriorityState,
        key: KeyEvent,
    ) -> bool {
        match key.code {
            KeyCode::Esc => {
                self.view = ViewState::List;
            }
            KeyCode::Char(c) if c.is_ascii_digit() && c != '0' => {
                let priority = (c as u32) - ('0' as u32);
                let task_id = state.task_id.clone();
                if let Some(task) = self.app_state.tasks.iter_mut().find(|t| t.id == task_id) {
                    task.priority = priority;
                    task.updated_at = chrono::Utc::now();
                }
                if let Err(e) = crate::state::save_state(&self.app_state) {
                    crate::logger::LOG.info(&format!("tui: save_state failed: {e}"));
                }
                self.view = ViewState::List;
            }
            _ => {}
        }
        false
    }

    /// Renders the set-priority popup overlay.
    pub(crate) fn render_set_priority(&self, _state: &SetPriorityState, f: &mut Frame) {
        let lines = vec![
            Line::from("Priority (1-9, 1 = highest):"),
            Line::from(""),
            Line::styled(
                "press 1-9 to set  esc cancel",
                Style::default()
                    .fg(Color::DarkGray)
                    .add_modifier(Modifier::DIM),
            ),
        ];

        render_popup(
            f,
            lines,
            Color::Yellow,
            50,
            6,
            Some(" Set Priority "),
            Padding::new(2, 2, 1, 0),
        );
    }
}
