use std::collections::HashSet;
use std::sync::mpsc;
use std::time::{Duration, Instant};

use crossterm::event::{self, Event, KeyCode, KeyEvent, KeyModifiers};
use ratatui::prelude::*;
use ratatui::widgets::*;

use crate::collector;
use crate::config;
use crate::git;
use crate::navigation;
use crate::paths;
use crate::remote;
use crate::tmux;
use crate::transfer;
use crate::types::{
    IssueState, OrchardConfig, PrStatus, SwitchToSessionOptions, Worktree, resolve_pr_status,
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
// View state and phase enums
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ViewState {
    List,
    Cleanup,
    ConfirmDelete,
    Transfer,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Phase {
    Idle,
    Confirm,
    InProgress,
    Done,
    Error,
}

// ---------------------------------------------------------------------------
// Messages from background threads
// ---------------------------------------------------------------------------

enum AppMsg {
    Worktrees(Vec<Worktree>),
    PaneContent(String, String), // (session_name, content)
    Warning(String),
    DeleteDone,
    DeleteErr(String),
    TransferDone,
    TransferErr(String),
    CleanupDone,
    Error(String),
}

/// Describes a tmux session switch that must run with the terminal suspended.
struct TmuxSwitchAction {
    /// For local: SwitchToSessionOptions. For remote: host + session + shell.
    kind: TmuxSwitchKind,
}

enum TmuxSwitchKind {
    Local(SwitchToSessionOptions),
    Remote { host: String, session_name: String, shell: String },
}

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

    // Background data channel
    tx: mpsc::Sender<AppMsg>,
    rx: mpsc::Receiver<AppMsg>,

    // Pending tmux action to run after suspending the terminal
    pending_tmux_switch: Option<TmuxSwitchAction>,

    // Delete state
    delete_target: Option<Worktree>,
    delete_phase: Phase,
    delete_error: Option<String>,

    // Transfer state
    transfer_target: Option<Worktree>,
    transfer_phase: Phase,
    transfer_error: Option<String>,

    // Cleanup state
    cleanup_stale: Vec<Worktree>,
    cleanup_selected: HashSet<String>,
    cleanup_cursor: usize,
    cleanup_phase: Phase,
    cleanup_deleted: Vec<String>,
    cleanup_errors: Vec<String>,

    // Auto-refresh
    last_refresh: Instant,
    spinner_frame: usize,
}

impl App {
    fn new(command: &str) -> Self {
        let cfg = config::load_config();
        let repo_root = git::find_repo_root();
        let repo_name = git::get_repo_name();
        let (tx, rx) = mpsc::channel();

        let view = if command == "cleanup" {
            ViewState::Cleanup
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
            tx,
            rx,
            delete_target: None,
            delete_phase: Phase::Idle,
            delete_error: None,
            transfer_target: None,
            transfer_phase: Phase::Idle,
            transfer_error: None,
            cleanup_stale: Vec::new(),
            cleanup_selected: HashSet::new(),
            cleanup_cursor: 0,
            cleanup_phase: Phase::Idle,
            cleanup_deleted: Vec::new(),
            cleanup_errors: Vec::new(),
            last_refresh: Instant::now(),
            spinner_frame: 0,
            pending_tmux_switch: None,
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
            if let Err(e) = collector::refresh_worktrees(&update_fn) {
                let _ = tx.send(AppMsg::Error(e.to_string()));
            }
        });
    }

    fn fetch_pane_content(&self) {
        if self.worktrees.is_empty() || self.cursor >= self.worktrees.len() {
            return;
        }
        let wt = &self.worktrees[self.cursor];
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
                    // Keep refreshing=true until no worktrees have pr_loading set
                    // (progressive updates set pr_loading=true until the final stage)
                    let still_loading = trees.iter().any(|wt| wt.pr_loading);
                    self.worktrees = trees;
                    self.loading = false;
                    if !still_loading {
                        self.refreshing = false;
                    }
                    self.error = None;
                    if self.cursor >= self.worktrees.len() && !self.worktrees.is_empty() {
                        self.cursor = self.worktrees.len() - 1;
                    }
                    // Populate cleanup stale list if in cleanup view.
                    if self.view == ViewState::Cleanup && self.cleanup_stale.is_empty() {
                        self.cleanup_stale = filter_stale(&self.worktrees);
                        self.cleanup_selected = self
                            .cleanup_stale
                            .iter()
                            .map(|wt| wt.path.clone())
                            .collect();
                    }
                    // Fetch pane content for the current selection.
                    if !self.worktrees.is_empty()
                        && self.cursor < self.worktrees.len()
                        && self.worktrees[self.cursor].tmux_session.is_some()
                    {
                        self.fetch_pane_content();
                    } else {
                        self.pane_content.clear();
                    }
                }
                AppMsg::PaneContent(session_name, content) => {
                    // Only accept if it matches the currently selected worktree's session.
                    let current_session = self.worktrees.get(self.cursor)
                        .and_then(|wt| wt.tmux_session.as_ref());
                    if current_session.map_or(false, |s| s == &session_name) {
                        self.pane_content = content;
                    }
                }
                AppMsg::DeleteDone => {
                    self.delete_phase = Phase::Done;
                    self.warning =
                        Some(("Worktree deleted.".to_string(), Instant::now()));
                    self.start_refresh();
                }
                AppMsg::DeleteErr(e) => {
                    self.delete_phase = Phase::Error;
                    self.delete_error = Some(e);
                }
                AppMsg::TransferDone => {
                    self.transfer_phase = Phase::Done;
                    self.warning =
                        Some(("Transfer complete.".to_string(), Instant::now()));
                    self.start_refresh();
                }
                AppMsg::TransferErr(e) => {
                    self.transfer_phase = Phase::Error;
                    self.transfer_error = Some(e);
                }
                AppMsg::CleanupDone => {
                    self.cleanup_phase = Phase::Done;
                    self.start_refresh();
                }
                AppMsg::Warning(msg) => {
                    self.warning = Some((msg, Instant::now()));
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
    // Key handling — returns true to quit
    // -------------------------------------------------------------------

    fn handle_key(&mut self, key: KeyEvent) -> bool {
        // Ctrl+C always quits.
        if key.modifiers.contains(KeyModifiers::CONTROL) && key.code == KeyCode::Char('c') {
            return true;
        }

        match self.view {
            ViewState::List => self.handle_list_key(key),
            ViewState::ConfirmDelete => self.handle_delete_key(key),
            ViewState::Transfer => self.handle_transfer_key(key),
            ViewState::Cleanup => self.handle_cleanup_key(key),
        }
    }

    fn handle_list_key(&mut self, key: KeyEvent) -> bool {
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
                self.switch_to_tmux_session();
                false
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
            KeyCode::Char('r') => {
                self.refreshing = true;
                self.start_refresh();
                false
            }
            KeyCode::Char('q') => true,
            _ => false,
        }
    }

    fn handle_delete_key(&mut self, key: KeyEvent) -> bool {
        match self.delete_phase {
            Phase::Confirm => match key.code {
                KeyCode::Char('y') => {
                    self.delete_phase = Phase::InProgress;
                    self.start_delete();
                    false
                }
                KeyCode::Char('n') | KeyCode::Esc => {
                    self.view = ViewState::List;
                    self.delete_target = None;
                    self.delete_phase = Phase::Idle;
                    false
                }
                _ => false,
            },
            Phase::Done | Phase::Error => {
                self.view = ViewState::List;
                self.delete_target = None;
                self.delete_phase = Phase::Idle;
                false
            }
            _ => false,
        }
    }

    fn handle_transfer_key(&mut self, key: KeyEvent) -> bool {
        match self.transfer_phase {
            Phase::Confirm => match key.code {
                KeyCode::Char('y') => {
                    self.transfer_phase = Phase::InProgress;
                    self.start_transfer();
                    false
                }
                KeyCode::Char('n') | KeyCode::Esc => {
                    self.view = ViewState::List;
                    self.transfer_target = None;
                    self.transfer_phase = Phase::Idle;
                    false
                }
                _ => false,
            },
            Phase::Done | Phase::Error => {
                self.view = ViewState::List;
                self.transfer_target = None;
                self.transfer_phase = Phase::Idle;
                false
            }
            _ => false,
        }
    }

    fn handle_cleanup_key(&mut self, key: KeyEvent) -> bool {
        if self.cleanup_phase == Phase::Done {
            match key.code {
                KeyCode::Char('q') | KeyCode::Esc => {
                    self.view = ViewState::List;
                    self.cleanup_phase = Phase::Idle;
                }
                _ => {}
            }
            return false;
        }

        if self.cleanup_phase == Phase::InProgress {
            return false;
        }

        match key.code {
            KeyCode::Up | KeyCode::Char('k') => {
                if self.cleanup_cursor > 0 {
                    self.cleanup_cursor -= 1;
                }
            }
            KeyCode::Down | KeyCode::Char('j') => {
                if !self.cleanup_stale.is_empty()
                    && self.cleanup_cursor < self.cleanup_stale.len() - 1
                {
                    self.cleanup_cursor += 1;
                }
            }
            KeyCode::Char(' ') => {
                if !self.cleanup_stale.is_empty()
                    && self.cleanup_cursor < self.cleanup_stale.len()
                {
                    let path = self.cleanup_stale[self.cleanup_cursor].path.clone();
                    if self.cleanup_selected.contains(&path) {
                        self.cleanup_selected.remove(&path);
                    } else {
                        self.cleanup_selected.insert(path);
                    }
                }
            }
            KeyCode::Enter => {
                let selected: Vec<Worktree> = self
                    .cleanup_stale
                    .iter()
                    .filter(|wt| self.cleanup_selected.contains(&wt.path))
                    .cloned()
                    .collect();
                if selected.is_empty() {
                    self.warning = Some(("No items selected.".to_string(), Instant::now()));
                } else {
                    self.cleanup_phase = Phase::InProgress;
                    self.start_cleanup(selected);
                }
            }
            KeyCode::Char('q') | KeyCode::Esc => {
                self.view = ViewState::List;
                self.cleanup_phase = Phase::Idle;
            }
            _ => {}
        }
        false
    }

    // -------------------------------------------------------------------
    // Actions
    // -------------------------------------------------------------------

    fn switch_to_tmux_session(&mut self) {
        if self.worktrees.is_empty() || self.cursor >= self.worktrees.len() {
            return;
        }
        let wt = &self.worktrees[self.cursor];
        let repo_name = &self.repo_name;

        if let Some(ref host) = wt.remote {
            let session_name = wt
                .tmux_session
                .clone()
                .or_else(|| {
                    wt.branch.as_ref().map(|b| {
                        tmux::derive_session_name(repo_name, Some(b), &wt.path)
                    })
                })
                .unwrap_or_default();
            if session_name.is_empty() {
                return;
            }
            let shell = self.config
                .remote
                .as_ref()
                .map(|r| r.shell.clone())
                .unwrap_or_else(|| "ssh".to_string());
            self.pending_tmux_switch = Some(TmuxSwitchAction {
                kind: TmuxSwitchKind::Remote {
                    host: host.clone(),
                    session_name,
                    shell,
                },
            });
        } else {
            let session_name = wt.tmux_session.clone().unwrap_or_else(|| {
                tmux::derive_session_name(
                    repo_name,
                    wt.branch.as_deref(),
                    &wt.path,
                )
            });
            self.pending_tmux_switch = Some(TmuxSwitchAction {
                kind: TmuxSwitchKind::Local(SwitchToSessionOptions {
                    session_name,
                    worktree_path: wt.path.clone(),
                    branch: wt.branch.clone(),
                    pr: wt.pr.clone(),
                }),
            });
        }
    }

    fn open_pr_url(&self) {
        if self.worktrees.is_empty() || self.cursor >= self.worktrees.len() {
            return;
        }
        let wt = &self.worktrees[self.cursor];
        if let Some(ref pr) = wt.pr {
            if !pr.url.is_empty() {
                crate::browser::open_url(&pr.url);
            }
        }
    }

    fn start_delete_dialog(&mut self) {
        if self.worktrees.is_empty() || self.cursor >= self.worktrees.len() {
            return;
        }
        let wt = &self.worktrees[self.cursor];
        if wt.is_bare {
            self.warning = Some(("Cannot delete the bare worktree.".to_string(), Instant::now()));
            return;
        }
        self.delete_target = Some(wt.clone());
        self.view = ViewState::ConfirmDelete;
        self.delete_phase = Phase::Confirm;
        self.delete_error = None;
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
            self.warning =
                Some(("Cannot transfer: no branch.".to_string(), Instant::now()));
            return;
        }
        self.transfer_target = Some(wt.clone());
        self.view = ViewState::Transfer;
        self.transfer_phase = Phase::Confirm;
        self.transfer_error = None;
    }

    fn enter_cleanup_view(&mut self) {
        self.view = ViewState::Cleanup;
        self.cleanup_stale = filter_stale(&self.worktrees);
        self.cleanup_selected = self
            .cleanup_stale
            .iter()
            .map(|wt| wt.path.clone())
            .collect();
        self.cleanup_cursor = 0;
        self.cleanup_phase = Phase::Idle;
        self.cleanup_deleted.clear();
        self.cleanup_errors.clear();
    }

    fn start_delete(&self) {
        let wt = match &self.delete_target {
            Some(wt) => wt.clone(),
            None => return,
        };
        let config = self.config.clone();
        let tx = self.tx.clone();
        std::thread::spawn(move || {
            let result = delete_worktree(&wt, &config);
            match result {
                Ok(()) => {
                    let _ = tx.send(AppMsg::DeleteDone);
                }
                Err(e) => {
                    let _ = tx.send(AppMsg::DeleteErr(e.to_string()));
                }
            }
        });
    }

    fn start_transfer(&self) {
        let wt = match &self.transfer_target {
            Some(wt) => wt.clone(),
            None => return,
        };
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
                if let Some(ref host) = wt.remote {
                    if let Some(ref sess) = wt.tmux_session {
                        let _ = remote::kill_remote_tmux_session(host, sess);
                    }
                    if let Some(ref branch) = wt.branch {
                        let slug = transfer::sanitize_branch_slug(branch);
                        let _ = remote::remove_remote_registry_entry(host, &slug);
                    }
                    if let Some(ref remote_cfg) = config.remote {
                        let _ =
                            remote::remove_remote_worktree(host, &remote_cfg.repo_path, &wt.path);
                    }
                } else {
                    if let Some(ref sess) = wt.tmux_session {
                        let _ = tmux::kill_tmux_session(sess);
                    }
                    let _ = git::remove_worktree(&wt.path, true);
                }
            }
            let _ = tx.send(AppMsg::CleanupDone);
        });
    }

    // -------------------------------------------------------------------
    // Rendering
    // -------------------------------------------------------------------

    fn render(&mut self, f: &mut Frame) {
        self.spinner_frame = (self.spinner_frame + 1) % SPINNER_FRAMES.len();

        match self.view {
            ViewState::List => self.render_list(f),
            ViewState::ConfirmDelete => self.render_delete(f),
            ViewState::Transfer => self.render_transfer(f),
            ViewState::Cleanup => self.render_cleanup(f),
        }
    }

    fn render_list(&self, f: &mut Frame) {
        let area = f.area();
        let width = area.width as usize;

        // Error state
        if let Some(ref err) = self.error {
            let chunks = Layout::default()
                .direction(Direction::Vertical)
                .constraints([Constraint::Length(7), Constraint::Length(1), Constraint::Min(3)])
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
                .constraints([Constraint::Length(7), Constraint::Length(1), Constraint::Min(3)])
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
                    Constraint::Length(7),
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

        // Calculate preview height
        let list_height = (self.worktrees.len() as u16) + 2; // +2 for borders
        let has_preview = !self.pane_content.is_empty()
            && self.cursor < self.worktrees.len()
            && self.worktrees[self.cursor].tmux_session.is_some();

        let has_warning = self
            .warning
            .as_ref()
            .is_some_and(|(_, t)| t.elapsed().as_secs() < WARNING_DURATION_SECS);

        let mut constraints = vec![
            Constraint::Length(7),          // header
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
            // Insert a Min(0) before hints to absorb remaining space
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

    fn render_header(&self, f: &mut Frame, area: Rect) {
        let header_text = vec![
            Line::from("🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴"),
            Line::from("┌─┐┬┌┬┐╔═╗╦═╗╔═╗╦ ╦╔═╗╦═╗╔╦╗"),
            Line::from("│ ┬│ │ ║ ║╠╦╝║  ╠═╣╠═╣╠╦╝ ║║"),
            Line::from("└─┘┴ ┴ ╚═╝╩╚═╚═╝╩ ╩╩ ╩╩╚══╩╝"),
            Line::from("🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴🌲🌳🌴"),
        ];
        let header = Paragraph::new(header_text)
            .alignment(Alignment::Center)
            .style(Style::default().fg(Color::Green).add_modifier(Modifier::BOLD))
            .block(
                Block::default()
                    .borders(Borders::ALL)
                    .border_style(Style::default().fg(Color::Green))
                    .border_type(BorderType::Rounded),
            );
        f.render_widget(header, area);
    }

    fn render_worktree_list(&self, f: &mut Frame, area: Rect, term_width: usize) {
        let max_width = if term_width > 4 { term_width - 4 } else { 40 };

        let path_width = (max_width * 45 / 100).min(50);
        let branch_width = (max_width * 25 / 100).min(30);
        let status_width = 12;
        let tmux_width = (max_width * 20 / 100).min(30);

        let mut lines: Vec<Line> = Vec::with_capacity(self.worktrees.len());

        for (i, wt) in self.worktrees.iter().enumerate() {
            let selected = i == self.cursor;
            let idx_str = format!("{:>2}", i + 1);
            let cursor_char = if selected { " >" } else { "  " };

            // Path column
            let path_display = paths::truncate_left(&paths::tildify(&wt.path), path_width);
            let path_col = pad_right(&path_display, path_width);

            // Branch column
            let branch_raw = wt.branch.as_deref().unwrap_or("(detached)");
            let branch_display = if branch_raw.len() > branch_width {
                format!("{}…", &branch_raw[..branch_width - 1])
            } else {
                branch_raw.to_string()
            };
            let branch_col = pad_right(&branch_display, branch_width);

            // Status column
            let status_str = render_status_badge(wt);
            let status_col = pad_right(&status_str, status_width);

            // Remote column
            let remote_col = if let Some(ref host) = wt.remote {
                let h = if host.len() > 10 {
                    format!("{}…", &host[..9])
                } else {
                    host.clone()
                };
                Some(pad_right(&format!("@{}", h), 12))
            } else {
                None
            };

            // Tmux column
            let tmux_str = if let Some(ref sess) = wt.tmux_session {
                let icon = if wt.tmux_attached { "\u{25b6}" } else { "\u{25fc}" };
                let max_name = if tmux_width > 2 { tmux_width - 2 } else { 0 };
                let name = if max_name > 0 && sess.len() > max_name {
                    format!("{}…", &sess[..max_name - 1])
                } else {
                    sess.clone()
                };
                format!("{} {}", icon, name)
            } else {
                String::new()
            };
            let tmux_col = pad_right(&tmux_str, tmux_width);

            if selected {
                // Entire row in cyan+bold when selected
                let mut row = format!(
                    "{}{} {} {} {}",
                    idx_str, cursor_char, path_col, branch_col, status_col
                );
                if let Some(ref rc) = remote_col {
                    row.push_str(&format!(" {}", rc));
                }
                row.push_str(&format!(" {}", tmux_col));

                lines.push(Line::from(Span::styled(
                    row,
                    Style::default()
                        .fg(Color::Cyan)
                        .add_modifier(Modifier::BOLD),
                )));
            } else {
                let mut spans: Vec<Span> = Vec::new();

                // Index (dim)
                spans.push(Span::styled(
                    idx_str,
                    Style::default().fg(Color::DarkGray),
                ));
                spans.push(Span::raw(format!("{} ", cursor_char)));

                // Path (default)
                spans.push(Span::raw(format!("{} ", path_col)));

                // Branch (yellow)
                spans.push(Span::styled(
                    format!("{} ", branch_col),
                    Style::default().fg(Color::Yellow),
                ));

                // Status (colored by PR status)
                spans.push(Span::styled(
                    status_col,
                    status_badge_style(wt),
                ));

                // Remote (magenta)
                if let Some(ref rc) = remote_col {
                    spans.push(Span::styled(
                        format!(" {}", rc),
                        Style::default().fg(Color::Magenta),
                    ));
                }

                // Tmux session (colored by state)
                let tmux_style = if wt.tmux_session.is_some() {
                    if wt.tmux_attached {
                        Style::default().fg(Color::Green)
                    } else {
                        Style::default().fg(Color::Blue)
                    }
                } else {
                    Style::default().fg(Color::DarkGray)
                };
                spans.push(Span::styled(format!(" {}", tmux_col), tmux_style));

                lines.push(Line::from(spans));
            }
        }

        let block = Block::default()
            .title(" WORKTREES ")
            .title_style(Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD))
            .borders(Borders::ALL)
            .border_style(Style::default().fg(Color::Cyan))
            .border_type(BorderType::Rounded);

        let list = Paragraph::new(lines).block(block);
        f.render_widget(list, area);
    }

    fn render_preview(&self, f: &mut Frame, area: Rect) {
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

    fn render_hints(&self, f: &mut Frame, area: Rect) {
        let sep = Span::styled(" \u{2502} ", Style::default().fg(Color::DarkGray));

        let mut spans: Vec<Span> = vec![
            Span::styled("1-9", Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD)),
            Span::raw(" jump"),
            sep.clone(),
            Span::styled("enter", Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD)),
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
                Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD),
            ));
            spans.push(Span::raw(" pr"));
        } else {
            spans.push(Span::styled("o pr", Style::default().fg(Color::DarkGray)));
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
                    Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD),
                ));
                spans.push(Span::raw(" pull"));
            } else {
                spans.push(Span::styled(
                    "p",
                    Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD),
                ));
                spans.push(Span::raw(" push"));
            }
        }

        spans.push(sep.clone());
        spans.push(Span::styled(
            "d",
            Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD),
        ));
        spans.push(Span::raw(" delete"));

        spans.push(sep.clone());
        spans.push(Span::styled(
            "c",
            Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD),
        ));
        spans.push(Span::raw(" cleanup"));

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
                Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD),
            ));
            spans.push(Span::raw(" refresh"));
        }

        spans.push(sep);
        spans.push(Span::styled(
            "q",
            Style::default().fg(Color::Cyan).add_modifier(Modifier::BOLD),
        ));
        spans.push(Span::raw(" quit"));

        let hints = Paragraph::new(Line::from(spans)).alignment(Alignment::Center);
        f.render_widget(hints, area);
    }

    // -------------------------------------------------------------------
    // Delete view
    // -------------------------------------------------------------------

    fn render_delete(&self, f: &mut Frame) {
        let wt = match &self.delete_target {
            Some(wt) => wt,
            None => return,
        };

        let branch_label = wt.branch.as_deref().unwrap_or("(detached)");
        let path_str = paths::tildify(&wt.path);

        let mut lines: Vec<Line> = Vec::new();

        match self.delete_phase {
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
                let err_msg = self.delete_error.as_deref().unwrap_or("unknown error");
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

        let content = Paragraph::new(lines).block(
            Block::default()
                .borders(Borders::ALL)
                .border_style(Style::default().fg(Color::Yellow))
                .border_type(BorderType::Rounded)
                .padding(Padding::new(2, 2, 1, 1)),
        );

        let popup = centered_rect(70, 12, f.area());
        f.render_widget(Clear, popup);
        f.render_widget(content, popup);
    }

    // -------------------------------------------------------------------
    // Transfer view
    // -------------------------------------------------------------------

    fn render_transfer(&self, f: &mut Frame) {
        let wt = match &self.transfer_target {
            Some(wt) => wt,
            None => return,
        };

        let branch_label = wt.branch.as_deref().unwrap_or("(detached)");
        let path_str = paths::tildify(&wt.path);
        let direction = if wt.remote.is_some() {
            "pull to local"
        } else {
            "push to remote"
        };

        let mut lines: Vec<Line> = Vec::new();

        match self.transfer_phase {
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
                let err_msg = self.transfer_error.as_deref().unwrap_or("unknown error");
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

        let content = Paragraph::new(lines).block(
            Block::default()
                .borders(Borders::ALL)
                .border_style(Style::default().fg(Color::Yellow))
                .border_type(BorderType::Rounded)
                .padding(Padding::new(2, 2, 1, 1)),
        );

        let popup = centered_rect(70, 12, f.area());
        f.render_widget(Clear, popup);
        f.render_widget(content, popup);
    }

    // -------------------------------------------------------------------
    // Cleanup view
    // -------------------------------------------------------------------

    fn render_cleanup(&self, f: &mut Frame) {
        let mut lines: Vec<Line> = Vec::new();

        match self.cleanup_phase {
            Phase::Done => {
                lines.push(Line::styled(
                    "Cleanup complete",
                    Style::default()
                        .fg(Color::Green)
                        .add_modifier(Modifier::BOLD),
                ));
                lines.push(Line::from(""));
                if !self.cleanup_deleted.is_empty() {
                    lines.push(Line::from(format!(
                        "Deleted {} worktree(s).",
                        self.cleanup_deleted.len()
                    )));
                }
                for e in &self.cleanup_errors {
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

                if self.cleanup_stale.is_empty() {
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
                    for (i, wt) in self.cleanup_stale.iter().enumerate() {
                        let cursor_char = if i == self.cleanup_cursor {
                            "\u{25b8} "
                        } else {
                            "  "
                        };

                        let check = if self.cleanup_selected.contains(&wt.path) {
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

                        if i == self.cleanup_cursor {
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

        let content = Paragraph::new(lines).block(
            Block::default()
                .borders(Borders::ALL)
                .border_style(Style::default().fg(Color::Cyan))
                .border_type(BorderType::Rounded)
                .padding(Padding::new(2, 2, 1, 1)),
        );

        let popup = centered_rect(90, 24, f.area());
        f.render_widget(Clear, popup);
        f.render_widget(content, popup);
    }
}

// ---------------------------------------------------------------------------
// Public entry point
// ---------------------------------------------------------------------------

/// Runs the Ratatui TUI. `command` determines the initial view ("cleanup" or default list).
pub fn run(command: &str) -> anyhow::Result<()> {
    // Setup terminal — no alternate screen so tmux switch-client works seamlessly
    crossterm::terminal::enable_raw_mode()?;
    let stdout = std::io::stdout();
    let backend = ratatui::backend::CrosstermBackend::new(stdout);
    let mut terminal = ratatui::Terminal::new(backend)?;
    terminal.clear()?;

    let mut app = App::new(command);

    // Initial data fetch in background
    app.start_refresh();

    let result = run_loop(&mut terminal, &mut app);

    // Restore terminal
    crossterm::terminal::disable_raw_mode()?;
    terminal.clear()?;
    terminal.show_cursor()?;

    result
}

fn run_loop(
    terminal: &mut ratatui::Terminal<ratatui::backend::CrosstermBackend<std::io::Stdout>>,
    app: &mut App,
) -> anyhow::Result<()> {
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

        // If a tmux switch is pending, run it directly.
        // No alternate screen to worry about — tmux switch-client just works.
        if let Some(action) = app.pending_tmux_switch.take() {
            let result = match action.kind {
                TmuxSwitchKind::Local(opts) => tmux::switch_to_session(&opts),
                TmuxSwitchKind::Remote { host, session_name, shell } => {
                    remote::attach_remote_session(&host, &session_name, &shell)
                }
            };

            if let Err(e) = result {
                let msg = e.to_string();
                if msg.contains("no current client") {
                    app.warning = Some((
                        "Not inside tmux — run `orchard` shell function instead".to_string(),
                        Instant::now(),
                    ));
                } else {
                    app.warning = Some((format!("tmux: {msg}"), Instant::now()));
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
    Ok(())
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
// Delete worktree
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
    if let Err(_) = git::remove_worktree(&wt.path, false) {
        git::remove_worktree(&wt.path, true)?;
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Rendering helpers
// ---------------------------------------------------------------------------

fn render_status_badge(wt: &Worktree) -> String {
    if wt.has_conflicts {
        return "\u{2716} conflict".to_string();
    }
    if wt.pr_loading {
        return "\u{00b7}\u{00b7}\u{00b7}".to_string();
    }
    if wt.pr.is_none() {
        if let Some(state) = wt.issue_state {
            if state == IssueState::Closed || state == IssueState::Completed {
                return "\u{2713} closed".to_string();
            }
        }
        return "no PR".to_string();
    }
    let pr = wt.pr.as_ref().unwrap();
    let status = resolve_pr_status(pr);
    let display = status.display();
    format!("{} {}", display.icon, display.label)
}

fn status_badge_style(wt: &Worktree) -> Style {
    if wt.has_conflicts {
        return Style::default().fg(Color::Red);
    }
    if wt.pr_loading {
        return Style::default().fg(Color::DarkGray);
    }
    if wt.pr.is_none() {
        if let Some(state) = wt.issue_state {
            if state == IssueState::Closed || state == IssueState::Completed {
                return Style::default().fg(Color::Green);
            }
        }
        return Style::default().fg(Color::DarkGray);
    }
    let pr = wt.pr.as_ref().unwrap();
    let status = resolve_pr_status(pr);
    Style::default().fg(status_color(status))
}

fn status_color(status: PrStatus) -> Color {
    match status {
        PrStatus::Conflict | PrStatus::Failing | PrStatus::ChangesRequested | PrStatus::Closed => {
            Color::Red
        }
        PrStatus::Unresolved | PrStatus::ReviewNeeded | PrStatus::PendingCi => Color::Yellow,
        PrStatus::Approved => Color::Green,
        PrStatus::Merged => Color::Magenta,
    }
}

fn pad_right(s: &str, width: usize) -> String {
    let runes: Vec<char> = s.chars().collect();
    if runes.len() >= width {
        return s.to_string();
    }
    let padding = " ".repeat(width - runes.len());
    format!("{}{}", s, padding)
}

/// Returns a centered rectangle within `r`, constrained by percent width and absolute height.
fn centered_rect(percent_x: u16, height: u16, r: Rect) -> Rect {
    let popup_width = r.width * percent_x / 100;
    let popup_height = height.min(r.height);
    let x = (r.width.saturating_sub(popup_width)) / 2;
    let y = (r.height.saturating_sub(popup_height)) / 2;
    Rect::new(r.x + x, r.y + y, popup_width, popup_height)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::{ChecksStatus, PrInfo, ReviewDecision};

    #[test]
    fn status_badge_conflict() {
        let wt = Worktree {
            has_conflicts: true,
            ..Default::default()
        };
        assert_eq!(render_status_badge(&wt), "\u{2716} conflict");
    }

    #[test]
    fn status_badge_loading() {
        let wt = Worktree {
            pr_loading: true,
            ..Default::default()
        };
        assert_eq!(render_status_badge(&wt), "\u{00b7}\u{00b7}\u{00b7}");
    }

    #[test]
    fn status_badge_no_pr() {
        let wt = Worktree::default();
        assert_eq!(render_status_badge(&wt), "no PR");
    }

    #[test]
    fn status_badge_issue_closed() {
        let wt = Worktree {
            issue_state: Some(IssueState::Closed),
            ..Default::default()
        };
        assert_eq!(render_status_badge(&wt), "\u{2713} closed");
    }

    #[test]
    fn status_badge_with_pr() {
        let wt = Worktree {
            pr: Some(PrInfo {
                number: 42,
                state: "open".into(),
                title: String::new(),
                url: String::new(),
                review_decision: ReviewDecision::Approved,
                unresolved_threads: 0,
                checks_status: ChecksStatus::Pass,
                has_conflicts: false,
            }),
            ..Default::default()
        };
        let badge = render_status_badge(&wt);
        assert!(badge.contains("ready"), "expected 'ready' in badge: {}", badge);
    }

    #[test]
    fn status_color_conflict_is_red() {
        assert_eq!(status_color(PrStatus::Conflict), Color::Red);
    }

    #[test]
    fn status_color_approved_is_green() {
        assert_eq!(status_color(PrStatus::Approved), Color::Green);
    }

    #[test]
    fn status_color_merged_is_magenta() {
        assert_eq!(status_color(PrStatus::Merged), Color::Magenta);
    }

    #[test]
    fn status_color_pending_is_yellow() {
        assert_eq!(status_color(PrStatus::PendingCi), Color::Yellow);
    }

    #[test]
    fn pad_right_short_string() {
        assert_eq!(pad_right("abc", 6), "abc   ");
    }

    #[test]
    fn pad_right_exact_width() {
        assert_eq!(pad_right("abcdef", 6), "abcdef");
    }

    #[test]
    fn pad_right_over_width() {
        assert_eq!(pad_right("abcdefgh", 6), "abcdefgh");
    }

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

    #[test]
    fn centered_rect_smaller_than_area() {
        let area = Rect::new(0, 0, 100, 40);
        let popup = centered_rect(70, 12, area);
        assert_eq!(popup.width, 70);
        assert_eq!(popup.height, 12);
        assert_eq!(popup.x, 15);
        assert_eq!(popup.y, 14);
    }

    #[test]
    fn centered_rect_height_clamped() {
        let area = Rect::new(0, 0, 100, 5);
        let popup = centered_rect(70, 12, area);
        assert_eq!(popup.height, 5);
    }

}
