use std::path::Path;
use std::process::Command;

use anyhow::anyhow;

use crate::types::{RemoteConfig, TmuxSession, Worktree};

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
    let cmd = format!("cd {} && git worktree list --porcelain", remote.repo_path);
    let out = match ssh_exec(&remote.host, &cmd) {
        Ok(o) => o,
        Err(_) => return Vec::new(),
    };

    let mut worktrees = crate::git::parse_porcelain(&out);
    for wt in &mut worktrees {
        wt.remote = Some(remote.host.clone());
    }
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
    parse_tmux_output(&out)
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
    ssh_exec(host, &format!("tmux kill-session -t {}", name))?;
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
        repo_path, wt_path
    );
    if ssh_exec(host, &cmd).is_ok() {
        return Ok(());
    }

    let fallback = format!(
        "cd {} && git worktree prune && rm -rf {}",
        repo_path, wt_path
    );
    ssh_exec(host, &fallback)?;
    Ok(())
}

/// Creates a new detached tmux session on the remote host.
/// If the session already exists the error is silently ignored.
pub fn create_remote_session(host: &str, name: &str, path: &str) -> anyhow::Result<()> {
    let cmd = format!("tmux new-session -d -s {} -c {}", name, path);
    match ssh_exec(host, &cmd) {
        Ok(_) => Ok(()),
        Err(e) if e.to_string().contains("duplicate session") => Ok(()),
        Err(e) => Err(e),
    }
}

/// Creates a local tmux session that connects to the remote session via ssh or
/// mosh, then switches the local tmux client to it.
pub fn attach_remote_session(host: &str, name: &str, shell: &str) -> anyhow::Result<()> {
    let shell = if shell.is_empty() { "ssh" } else { shell };

    // Verify the remote session is alive.
    ssh_exec(host, &format!("tmux has-session -t {}", name))
        .map_err(|_| anyhow!("remote session {:?} not found on {}", name, host))?;

    let local_name = format!("remote_{}", name);
    let connect_cmd = if shell == "mosh" {
        format!("mosh {} -- tmux attach -t {}", host, name)
    } else {
        format!("ssh {} -t tmux attach -t {}", host, name)
    };

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
                "creating local session {:?}: {}",
                local_name,
                stderr
            ));
        }
    }

    let switch = Command::new("tmux")
        .args(["switch-client", "-t", &local_name])
        .status()?;
    if !switch.success() {
        return Err(anyhow!("switching to session {:?}", local_name));
    }
    Ok(())
}

/// Captures the pane content of a remote tmux session via SSH.
pub fn capture_remote_pane_content(
    host: &str,
    session: &str,
    lines: u32,
) -> anyhow::Result<String> {
    let cmd = format!(
        "tmux capture-pane -t {} -p -J -e -S -{}",
        session, lines
    );
    let out = ssh_exec(host, &cmd)?;
    Ok(out.trim_end_matches('\n').to_string())
}

/// Removes the remmy session registry file for the given session name on the
/// remote host.
pub fn remove_remote_registry_entry(host: &str, name: &str) -> anyhow::Result<()> {
    ssh_exec(host, &format!("rm -f ~/.remmy/sessions/{}.json", name))?;
    Ok(())
}
