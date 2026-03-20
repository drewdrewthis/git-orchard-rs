use std::path::Path;
use std::process::Command;

use anyhow::anyhow;

use crate::logger::LOG;
use crate::types::{RemoteConfig, TmuxSession, Worktree};

/// Shell-escape a string for safe use in SSH command strings.
pub fn shell_escape(s: &str) -> String {
    if s.is_empty() {
        return "''".to_string();
    }
    // If it only contains safe characters, return as-is
    if s.chars().all(|c| c.is_alphanumeric() || "-_./=@:+~".contains(c)) {
        return s.to_string();
    }
    // Otherwise, wrap in single quotes and escape any internal single quotes
    format!("'{}'", s.replace('\'', "'\\''"))
}

// SSH flags used for all orchard remote connections.
const SSH_FLAGS: &[&str] = &[
    "-o",
    "ConnectTimeout=5",
    "-o",
    "BatchMode=yes",
    "-o",
    "ControlMaster=auto",
    "-o",
    "ControlPath=/tmp/orchard-ssh-%r@%h:%p",
    "-o",
    "ControlPersist=600",
];

/// Runs a shell command on a remote host over SSH and returns stdout.
pub fn ssh_exec(host: &str, command: &str) -> anyhow::Result<String> {
    let mut args: Vec<&str> = SSH_FLAGS.to_vec();
    args.push(host);
    args.push(command);

    let out = Command::new("ssh").args(&args).output()?;
    if !out.status.success() {
        let stderr = String::from_utf8_lossy(&out.stderr).into_owned();
        return Err(anyhow!("ssh command failed: {}", stderr));
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}

/// Returns all git worktrees on the remote machine for the configured repo path.
/// Returns an empty `Vec` on any error.
pub fn list_remote_worktrees(remote: &RemoteConfig) -> Vec<Worktree> {
    let cmd = format!("cd {} && git worktree list --porcelain", shell_escape(&remote.repo_path));
    let out = match ssh_exec(&remote.host, &cmd) {
        Ok(o) => o,
        Err(err) => {
            LOG.warn(&format!("remote[{}]: failed to list worktrees: {}", remote.host, err));
            return Vec::new();
        }
    };

    let mut worktrees = crate::git::parse_porcelain(&out);
    for wt in &mut worktrees {
        wt.remote = Some(remote.host.clone());
    }
    LOG.info(&format!("remote[{}]: {} worktrees", remote.host, worktrees.len()));
    worktrees
}

/// Returns all tmux sessions on the remote machine.
/// Returns an empty `Vec` on any error.
pub fn list_remote_tmux_sessions(remote: &RemoteConfig) -> Vec<TmuxSession> {
    let cmd =
        "tmux list-sessions -F '#{session_name}\t#{session_path}\t#{session_attached}'";
    let out = match ssh_exec(&remote.host, cmd) {
        Ok(o) => o,
        Err(_) => return Vec::new(),
    };
    let sessions = parse_tmux_output(&out);
    LOG.info(&format!("remote[{}]: {} tmux sessions", remote.host, sessions.len()));
    sessions
}

fn parse_tmux_output(out: &str) -> Vec<TmuxSession> {
    let mut sessions = Vec::new();
    for line in out.trim().lines() {
        if line.is_empty() {
            continue;
        }
        let parts: Vec<&str> = line.splitn(3, '\t').collect();
        if parts.len() != 3 {
            continue;
        }
        sessions.push(TmuxSession {
            name: parts[0].to_string(),
            path: parts[1].to_string(),
            attached: parts[2] == "1",
            pane_title: None,
        });
    }
    sessions
}

/// Fetches worktrees and tmux sessions from the remote in parallel using threads,
/// then attaches matching sessions to their worktrees.
pub fn fetch_remote_worktrees(remote: &RemoteConfig) -> Vec<Worktree> {
    let remote_wt = remote.clone();
    let remote_tmux = remote.clone();

    let wt_handle = std::thread::spawn(move || list_remote_worktrees(&remote_wt));
    let tmux_handle = std::thread::spawn(move || list_remote_tmux_sessions(&remote_tmux));

    let mut worktrees = wt_handle.join().unwrap_or_default();
    let sessions = tmux_handle.join().unwrap_or_default();

    for wt in &mut worktrees {
        if let Some(sess) = match_session(&sessions, &wt.path, wt.branch.as_deref()) {
            wt.tmux_session = Some(sess.name.clone());
            wt.tmux_attached = sess.attached;
        }
    }
    worktrees
}

fn match_session<'a>(
    sessions: &'a [TmuxSession],
    path: &str,
    branch: Option<&str>,
) -> Option<&'a TmuxSession> {
    let dir_name = Path::new(path)
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or("");
    let branch_slug = branch.map(|b| b.replace('/', "-"));

    for s in sessions {
        if s.path == path {
            return Some(s);
        }
        if s.name == dir_name {
            return Some(s);
        }
        if let Some(ref slug) = branch_slug {
            if &s.name == slug {
                return Some(s);
            }
        }
    }
    None
}

/// Kills the named tmux session on the remote host.
pub fn kill_remote_tmux_session(host: &str, name: &str) -> anyhow::Result<()> {
    ssh_exec(host, &format!("tmux kill-session -t {}", shell_escape(name)))?;
    LOG.info(&format!("remote: killed tmux session {}", name));
    Ok(())
}

/// Removes a worktree on the remote host.
/// First tries `git worktree remove --force`; falls back to `git worktree prune && rm -rf`.
pub fn remove_remote_worktree(
    host: &str,
    repo_path: &str,
    wt_path: &str,
) -> anyhow::Result<()> {
    let cmd = format!(
        "cd {} && git worktree remove --force {}",
        shell_escape(repo_path), shell_escape(wt_path)
    );
    if ssh_exec(host, &cmd).is_ok() {
        return Ok(());
    }

    let fallback = format!(
        "cd {} && git worktree prune && rm -rf {}",
        shell_escape(repo_path), shell_escape(wt_path)
    );
    ssh_exec(host, &fallback)?;
    Ok(())
}

/// Creates a new detached tmux session on the remote host.
/// If the session already exists the error is silently ignored.
pub fn create_remote_session(host: &str, name: &str, path: &str) -> anyhow::Result<()> {
    let cmd = format!("tmux new-session -d -s {} -c {}", shell_escape(name), shell_escape(path));
    match ssh_exec(host, &cmd) {
        Ok(_) => Ok(()),
        Err(e) if e.to_string().contains("duplicate session") => Ok(()),
        Err(e) => Err(e),
    }
}

/// Checks if a local proxy session exists and is healthy.
/// Returns `true` if the session needs to be (re)created.
/// Determines whether the local proxy session needs to be (re)created.
/// `remote_was_fresh` indicates the remote session was just created, meaning
/// any existing proxy is connected to a stale session.
fn should_recreate_proxy(local_name: &str, remote_was_fresh: bool) -> bool {
    let proxy_exists = Command::new("tmux")
        .args(["has-session", "-t", local_name])
        .status()
        .map(|s| s.success())
        .unwrap_or(false);

    if !proxy_exists {
        return true;
    }

    // If we just created the remote session, the existing proxy is stale.
    if remote_was_fresh {
        LOG.info(&format!("remote: killing stale proxy {} (remote was recreated)", local_name));
        let _ = Command::new("tmux")
            .args(["kill-session", "-t", local_name])
            .status();
        return true;
    }

    let pane_dead = Command::new("tmux")
        .args(["list-panes", "-t", local_name, "-F", "#{pane_dead}"])
        .output()
        .ok()
        .map(|o| String::from_utf8_lossy(&o.stdout).trim() == "1")
        .unwrap_or(false);

    let pane_stuck = if !pane_dead {
        Command::new("tmux")
            .args(["capture-pane", "-t", local_name, "-p", "-S", "-1"])
            .output()
            .ok()
            .map(|o| {
                let content = String::from_utf8_lossy(&o.stdout);
                content.contains("mosh: Last contact")
                    || (content.contains("Connection to") && content.contains("closed"))
            })
            .unwrap_or(false)
    } else {
        false
    };

    if pane_dead || pane_stuck {
        LOG.info(&format!(
            "remote: killing {} proxy session {}",
            if pane_dead { "dead" } else { "stuck" },
            local_name
        ));
        let _ = Command::new("tmux")
            .args(["kill-session", "-t", local_name])
            .status();
        return true;
    }

    false
}

/// Creates a local proxy tmux session that connects to the remote session via ssh or
/// mosh. Does NOT switch the local tmux client to it. Returns the local session name.
///
/// This is the popup-mode entry point: the caller gets the local session name back
/// and prints it to stdout for the wrapper script to call `tmux switch-client`.
pub fn create_remote_proxy_session(
    host: &str,
    name: &str,
    path: &str,
    shell: &str,
) -> anyhow::Result<String> {
    let shell = if shell.is_empty() { "ssh" } else { shell };

    // Create the remote session if it doesn't exist yet.
    let remote_was_fresh = ssh_exec(host, &format!("tmux has-session -t {}", shell_escape(name))).is_err();
    if remote_was_fresh {
        create_remote_session(host, name, path)?;
    }

    let local_name = format!("remote_{}", name);
    let connect_cmd = if shell == "mosh" {
        format!(
            "env LC_ALL=en_US.UTF-8 mosh --predict=always {} -- tmux attach-session -t {}",
            shell_escape(host), shell_escape(name)
        )
    } else {
        format!(
            "ssh -tt {} tmux attach-session -t {}",
            shell_escape(host), shell_escape(name)
        )
    };

    let need_create = should_recreate_proxy(&local_name, remote_was_fresh);
    if need_create {
        let create_out = Command::new("tmux")
            .args([
                "new-session",
                "-d",
                "-s",
                &local_name,
                "--",
                "sh",
                "-c",
                &connect_cmd,
            ])
            .output()?;

        if !create_out.status.success() {
            let stderr = String::from_utf8_lossy(&create_out.stderr);
            if !stderr.contains("duplicate session") {
                return Err(anyhow!(
                    "creating local proxy session {:?}: {}",
                    local_name,
                    stderr
                ));
            }
        }
    }

    // Keep the pane alive after the SSH/mosh process exits.
    let _ = Command::new("tmux")
        .args(["set-option", "-t", &local_name, "remain-on-exit", "on"])
        .status();

    LOG.info(&format!("createRemoteProxySession: {} -> {}", name, local_name));
    Ok(local_name)
}

/// Captures the pane content of a remote tmux session via SSH.
pub fn capture_remote_pane_content(
    host: &str,
    session: &str,
    lines: u32,
) -> anyhow::Result<String> {
    let cmd = format!(
        "tmux capture-pane -t {} -p -J -S -{}",
        shell_escape(session), lines
    );
    let out = ssh_exec(host, &cmd)?;
    Ok(out.trim_end_matches('\n').to_string())
}

/// Removes the remmy session registry file for the given session name on the
/// remote host.
pub fn remove_remote_registry_entry(host: &str, name: &str) -> anyhow::Result<()> {
    ssh_exec(host, &format!("rm -f ~/.remmy/sessions/{}.json", shell_escape(name)))?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn shell_escape_returns_empty_quoted_for_empty_string() {
        assert_eq!(shell_escape(""), "''");
    }

    #[test]
    fn shell_escape_passes_through_safe_characters() {
        assert_eq!(shell_escape("hello-world_123"), "hello-world_123");
    }

    #[test]
    fn shell_escape_passes_through_tilde() {
        assert_eq!(shell_escape("~/path/to/dir"), "~/path/to/dir");
    }

    #[test]
    fn shell_escape_passes_through_at_and_colon() {
        assert_eq!(shell_escape("user@host:path"), "user@host:path");
    }

    #[test]
    fn shell_escape_wraps_spaces_in_single_quotes() {
        assert_eq!(shell_escape("hello world"), "'hello world'");
    }

    #[test]
    fn shell_escape_escapes_single_quotes() {
        assert_eq!(shell_escape("it's"), "'it'\\''s'");
    }

    #[test]
    fn shell_escape_wraps_semicolons() {
        assert_eq!(shell_escape("cmd;evil"), "'cmd;evil'");
    }

    #[test]
    fn shell_escape_wraps_dollar_signs() {
        assert_eq!(shell_escape("$HOME"), "'$HOME'");
    }

    #[test]
    fn shell_escape_wraps_backticks() {
        assert_eq!(shell_escape("`whoami`"), "'`whoami`'");
    }
}
