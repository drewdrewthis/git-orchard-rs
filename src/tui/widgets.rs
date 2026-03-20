use ratatui::prelude::*;
use ratatui::widgets::*;

use crate::types::{IssueState, PrStatus, Worktree, resolve_pr_status};

// ---------------------------------------------------------------------------
// Badge display
// ---------------------------------------------------------------------------

pub struct BadgeDisplay {
    pub text: String,
    pub style: Style,
}

pub fn status_badge(wt: &Worktree, refreshing: bool) -> BadgeDisplay {
    if wt.has_conflicts {
        return BadgeDisplay {
            text: "\u{2716} conflict".to_string(),
            style: Style::default().fg(Color::Red),
        };
    }
    if wt.pr_loading || refreshing {
        return BadgeDisplay {
            text: "\u{00b7}\u{00b7}\u{00b7}".to_string(),
            style: Style::default().fg(Color::DarkGray),
        };
    }
    if wt.pr.is_none() {
        if let Some(state) = wt.issue_state {
            if state == IssueState::Closed || state == IssueState::Completed {
                return BadgeDisplay {
                    text: "\u{2713} closed".to_string(),
                    style: Style::default().fg(Color::Green),
                };
            }
        }
        return BadgeDisplay {
            text: "\u{2014}".to_string(),
            style: Style::default().fg(Color::DarkGray),
        };
    }
    let pr = wt.pr.as_ref().unwrap();
    let status = resolve_pr_status(pr);
    let display = status.display();
    BadgeDisplay {
        text: format!("{} {}", display.icon, display.label),
        style: Style::default().fg(status_color(status)),
    }
}

pub fn claude_badge(wt: &Worktree) -> BadgeDisplay {
    if wt.tmux_session.is_none() {
        return BadgeDisplay {
            text: String::new(),
            style: Style::default().fg(Color::DarkGray),
        };
    }
    if let Some(ref title) = wt.tmux_pane_title {
        if title.contains("Claude Code") {
            return BadgeDisplay {
                text: "\u{26a1} active".to_string(),
                style: Style::default().fg(Color::Magenta),
            };
        }
    }
    BadgeDisplay {
        text: "\u{25cf} idle".to_string(),
        style: Style::default().fg(Color::DarkGray),
    }
}

pub fn status_color(status: PrStatus) -> Color {
    match status {
        PrStatus::Conflict | PrStatus::Failing | PrStatus::ChangesRequested | PrStatus::Closed => {
            Color::Red
        }
        PrStatus::Unresolved | PrStatus::ReviewNeeded | PrStatus::PendingCi => Color::Yellow,
        PrStatus::Approved => Color::Green,
        PrStatus::Merged => Color::Magenta,
    }
}

// ---------------------------------------------------------------------------
// Popup rendering helper
// ---------------------------------------------------------------------------

pub fn render_popup(
    f: &mut Frame,
    lines: Vec<Line>,
    border_color: Color,
    percent_x: u16,
    height: u16,
    title: Option<&str>,
    padding: Padding,
) {
    let mut block = Block::default()
        .borders(Borders::ALL)
        .border_style(Style::default().fg(border_color).bg(Color::Black))
        .border_type(BorderType::Rounded)
        .padding(padding);
    if let Some(t) = title {
        block = block
            .title(t)
            .title_style(
                Style::default()
                    .fg(border_color)
                    .bg(Color::Black)
                    .add_modifier(Modifier::BOLD),
            );
    }
    let content = Paragraph::new(lines)
        .style(Style::default().bg(Color::Black))
        .block(block);
    let popup = centered_rect(percent_x, height, f.area());
    f.render_widget(Clear, popup);
    f.render_widget(content, popup);
}

/// Returns a centered rectangle within `r`, constrained by percent width and absolute height.
pub fn centered_rect(percent_x: u16, height: u16, r: Rect) -> Rect {
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
    use crate::types::{ChecksStatus, IssueState, PrInfo, ReviewDecision};

    #[test]
    fn status_badge_conflict() {
        let wt = Worktree {
            has_conflicts: true,
            ..Default::default()
        };
        assert_eq!(status_badge(&wt, false).text, "\u{2716} conflict");
    }

    #[test]
    fn status_badge_loading() {
        let wt = Worktree {
            pr_loading: true,
            ..Default::default()
        };
        assert_eq!(status_badge(&wt, false).text, "\u{00b7}\u{00b7}\u{00b7}");
    }

    #[test]
    fn status_badge_does_not_show_claude_for_claude_code_pane() {
        let wt = Worktree {
            tmux_pane_title: Some("\u{2733} Claude Code".to_string()),
            tmux_session: Some("my-session".to_string()),
            ..Default::default()
        };
        assert_eq!(status_badge(&wt, false).text, "\u{2014}");
    }

    #[test]
    fn claude_badge_active_when_pane_title_contains_claude_code() {
        let wt = Worktree {
            tmux_session: Some("my-session".to_string()),
            tmux_pane_title: Some("\u{2733} Claude Code".to_string()),
            ..Default::default()
        };
        assert_eq!(claude_badge(&wt).text, "\u{26a1} active");
    }

    #[test]
    fn claude_badge_idle_when_session_exists_but_no_claude_code() {
        let wt = Worktree {
            tmux_session: Some("my-session".to_string()),
            tmux_pane_title: Some("bash".to_string()),
            ..Default::default()
        };
        assert_eq!(claude_badge(&wt).text, "\u{25cf} idle");
    }

    #[test]
    fn claude_badge_empty_when_no_session() {
        let wt = Worktree::default();
        assert_eq!(claude_badge(&wt).text, "");
    }

    #[test]
    fn claude_badge_idle_when_session_exists_and_no_pane_title() {
        let wt = Worktree {
            tmux_session: Some("my-session".to_string()),
            tmux_pane_title: None,
            ..Default::default()
        };
        assert_eq!(claude_badge(&wt).text, "\u{25cf} idle");
    }

    #[test]
    fn status_badge_refreshing() {
        let wt = Worktree::default();
        assert_eq!(status_badge(&wt, true).text, "\u{00b7}\u{00b7}\u{00b7}");
    }

    #[test]
    fn status_badge_no_pr() {
        let wt = Worktree::default();
        assert_eq!(status_badge(&wt, false).text, "\u{2014}");
    }

    #[test]
    fn status_badge_issue_closed() {
        let wt = Worktree {
            issue_state: Some(IssueState::Closed),
            ..Default::default()
        };
        assert_eq!(status_badge(&wt, false).text, "\u{2713} closed");
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
        let badge = status_badge(&wt, false);
        assert!(
            badge.text.contains("ready"),
            "expected 'ready' in badge: {}",
            badge.text
        );
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
