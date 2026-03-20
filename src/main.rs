mod browser;
mod collector;
pub mod events;
mod config;
mod git;
mod github;
mod issue_sync;
mod logger;
mod navigation;
mod paths;
mod remote;
mod session_discovery;
mod shell;
mod state;
mod status;
mod tmux;
mod transfer;
mod types;
mod tui;

use std::env;
use std::io::IsTerminal;

fn main() {
    let args: Vec<String> = env::args().collect();

    let mut json_flag = false;
    let mut command = String::new();

    for arg in &args[1..] {
        match arg.as_str() {
            "--json" => json_flag = true,
            "--help" | "-h" => {
                print_usage();
                return;
            }
            _ if !arg.starts_with('-') && command.is_empty() => command = arg.clone(),
            _ => {}
        }
    }

    logger::LOG.info(&format!(
        "startup: orchard{}",
        if command.is_empty() {
            String::new()
        } else {
            format!(" command={command}")
        }
    ));

    match command.as_str() {
        "init" => handle_init(),
        "upgrade" => handle_upgrade(),
        _ => {
            if json_flag {
                handle_json();
            } else {
                handle_tui(&command);
            }
        }
    }
}

fn handle_init() {
    match shell::run_init_wizard() {
        Ok(()) => {}
        Err(e) => {
            eprintln!("{}", e);
            std::process::exit(1);
        }
    }
}

fn handle_upgrade() {
    eprintln!("Upgrade not yet implemented for the Rust binary.");
    eprintln!(
        "Download the latest from: https://github.com/drewdrewthis/orchard-rs/releases/latest"
    );
}

fn handle_json() {
    match collector::collect_worktree_data() {
        Ok(data) => {
            let json = serde_json::to_string_pretty(&data).unwrap_or_else(|e| {
                eprintln!("Error serializing JSON: {e}");
                std::process::exit(1);
            });
            println!("{json}");
        }
        Err(e) => {
            eprintln!("Error collecting data: {e}");
            std::process::exit(1);
        }
    }
}

/// Runs the TUI. If inside tmux and run directly (not via popup wrapper),
/// re-launches itself as a tmux popup using the wrapper script so that
/// session switching works correctly after the popup closes.
fn handle_tui(command: &str) {
    let inside_tmux = env::var("TMUX").is_ok();
    let is_tty = std::io::stdout().is_terminal();

    // If inside tmux and stdout is a TTY, we were run directly (not via popup).
    // Re-launch as a popup through the wrapper script.
    if inside_tmux && is_tty {
        let wrapper = dirs::home_dir()
            .map(|h| h.join(".local/bin/orchard-popup"))
            .unwrap_or_else(|| std::path::PathBuf::from("orchard-popup"));

        if wrapper.exists() {
            let _ = std::process::Command::new("tmux")
                .args([
                    "display-popup", "-E",
                    "-w", "90%", "-h", "80%",
                    &wrapper.to_string_lossy(),
                ])
                .status();
        } else {
            eprintln!("Wrapper script not found. Run `orchard init` first.");
            std::process::exit(1);
        }
        return;
    }

    // Inside popup (stdout captured) or outside tmux — run the TUI directly.
    match tui::run(command) {
        Ok(Some(session_name)) => {
            if inside_tmux {
                // Inside popup — print for wrapper to switch-client.
                println!("{session_name}");
            } else {
                // Outside tmux — attach to the session.
                let _ = std::process::Command::new("tmux")
                    .args(["attach-session", "-t", &session_name])
                    .status();
            }
        }
        Ok(None) => {}
        Err(e) => {
            eprintln!("{e}");
            std::process::exit(1);
        }
    }
}

fn print_usage() {
    eprintln!(
        r#"Usage:
  orchard              Interactive worktree manager (popup mode)
  orchard init         Interactive setup wizard for popup mode
  orchard upgrade      Upgrade to the latest version

Options:
  --json    Output worktree data as JSON and exit

Navigation:
  1-9     Jump to worktree by number
  ↑/↓     Select worktree
  Enter/t tmux into worktree (creates session if needed, then exits)
  d       Delete selected worktree
  c       Cleanup merged worktrees
  r       Refresh list
  q/Esc   Close popup (no switch)"#
    );
}
