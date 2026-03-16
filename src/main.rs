mod browser;
mod collector;
mod config;
mod git;
mod github;
mod logger;
mod navigation;
mod paths;
mod remote;
mod shell;
mod tmux;
mod transfer;
mod types;
mod tui;

use std::env;

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
        "startup: git-orchard{}",
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
    println!("{}", shell::get_init_instructions());
}

fn handle_upgrade() {
    eprintln!("Upgrade not yet implemented for the Rust binary.");
    eprintln!(
        "Download the latest from: https://github.com/drewdrewthis/git-orchard-rs/releases/latest"
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

fn handle_tui(command: &str) {
    if let Err(e) = tui::run(command) {
        eprintln!("Error running TUI: {e}");
        std::process::exit(1);
    }
}

fn print_usage() {
    eprintln!(
        r#"Usage:
  git-orchard              Interactive worktree manager
  git-orchard init         Print shell function for tmux session integration
  git-orchard upgrade      Upgrade to the latest version
  git-orchard cleanup      Find worktrees with merged PRs to remove

Options:
  --json    Output worktree data as JSON and exit

Navigation:
  1-9     Jump to worktree by number
  ↑/↓     Select worktree
  t       tmux into worktree (attach or create session)
  d       Delete selected worktree
  c       Cleanup merged worktrees
  r       Refresh list
  q       Switch back to previous tmux session (quit if not in tmux)"#
    );
}
