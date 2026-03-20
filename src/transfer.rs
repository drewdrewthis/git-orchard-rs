use std::path::Path;
use std::process::Command;

use anyhow::anyhow;
use regex::Regex;
use std::sync::OnceLock;

use crate::logger::LOG;
use crate::remote;
use crate::tmux;
use crate::types::{RemoteConfig, Worktree};

const WIP_MESSAGE: &str = "[orchard] WIP handoff";

// ---------------------------------------------------------------------------
// Slug helpers
// ---------------------------------------------------------------------------

fn non_slug_re() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new(r"[^a-zA-Z0-9.\-_]").unwrap())
}

/// Converts a branch name to a filesystem-safe slug by replacing `/` with `-`
/// and stripping all other non-alphanumeric characters except `.`, `-`, `_`.
pub fn sanitize_branch_slug(branch: &str) -> String {
    let replaced = branch.replace('/', "-");
    non_slug_re().replace_all(&replaced, "").into_owned()
}

/// Returns the conventional path for a remote worktree:
/// `parent(repo_path)/worktrees/worktree-SLUG`.
pub fn derive_remote_worktree_path(repo_path: &str, branch: &str) -> String {
    let slug = sanitize_branch_slug(branch);
    let parent = Path::new(repo_path)
        .parent()
        .unwrap_or_else(|| Path::new("."));
    parent
        .join("worktrees")
        .join(format!("worktree-{}", slug))
        .to_string_lossy()
        .into_owned()
}

/// Returns the absolute conventional path for a local worktree:
/// `parent(repo_root)/worktrees/worktree-SLUG`.
pub fn derive_local_worktree_path(repo_root: &str, branch: &str) -> String {
    let slug = sanitize_branch_slug(branch);
    let parent = Path::new(repo_root)
        .parent()
        .unwrap_or_else(|| Path::new("."));
    let joined = parent.join("worktrees").join(format!("worktree-{}", slug));
    match joined.canonicalize() {
        Ok(abs) => abs.to_string_lossy().into_owned(),
        // canonicalize fails if path doesn't exist yet; return the unresolved path.
        Err(_) => to_absolute(&joined),
    }
}

fn to_absolute(path: &Path) -> String {
    if path.is_absolute() {
        return path.to_string_lossy().into_owned();
    }
    std::env::current_dir()
        .map(|cwd| cwd.join(path))
        .unwrap_or_else(|_| path.to_path_buf())
        .to_string_lossy()
        .into_owned()
}

// ---------------------------------------------------------------------------
// Local command helpers
// ---------------------------------------------------------------------------

fn run_local(dir: Option<&str>, args: &[&str]) -> anyhow::Result<()> {
    let mut cmd = Command::new(args[0]);
    cmd.args(&args[1..]);
    if let Some(d) = dir {
        cmd.current_dir(d);
    }
    let out = cmd.output()?;
    if !out.status.success() {
        let combined = String::from_utf8_lossy(&out.stdout).into_owned()
            + &String::from_utf8_lossy(&out.stderr);
        return Err(anyhow!("{}: {}", args.join(" "), combined));
    }
    Ok(())
}

fn has_wip_commit(dir: &str) -> bool {
    Command::new("git")
        .args(["-C", dir, "log", "-1", "--format=%s"])
        .output()
        .map(|o| {
            String::from_utf8_lossy(&o.stdout)
                .trim()
                .eq(WIP_MESSAGE)
        })
        .unwrap_or(false)
}

fn commit_wip(dir: &str) -> anyhow::Result<()> {
    if has_wip_commit(dir) {
        return Ok(());
    }
    run_local(Some(dir), &["git", "add", "-u"])?;

    let status = Command::new("git")
        .args(["-C", dir, "status", "--porcelain"])
        .output()?;
    if String::from_utf8_lossy(&status.stdout).trim().is_empty() {
        return Ok(());
    }
    run_local(Some(dir), &["git", "commit", "-m", WIP_MESSAGE])
}

fn branch_name(wt: &Worktree) -> anyhow::Result<String> {
    wt.branch
        .as_ref()
        .filter(|b| !b.is_empty())
        .cloned()
        .ok_or_else(|| anyhow!("worktree at {:?} has no branch", wt.path))
}

// ---------------------------------------------------------------------------
// Push to remote
// ---------------------------------------------------------------------------

/// Transfers a local worktree to the remote machine.
///
/// Steps:
/// 1. Verify no conflicts and worktree has a branch.
/// 2. Commit WIP.
/// 3. `git push -u origin BRANCH`.
/// 4. SSH: `git fetch` + `git worktree add` (fallback: `git pull`).
/// 5. SSH: `tmux new-session`.
/// 6. Kill local tmux session.
/// 7. Remove local worktree.
pub fn push_to_remote(
    wt: &Worktree,
    remote: &RemoteConfig,
    on_step: &dyn Fn(&str),
) -> anyhow::Result<()> {
    if wt.has_conflicts {
        return Err(anyhow!("worktree at {:?} has unresolved conflicts", wt.path));
    }

    let branch = branch_name(wt)?;

    LOG.info(&format!("pushToRemote: transferring {} to {}", branch, remote.host));

    on_step("Committing changes...");
    commit_wip(&wt.path).map_err(|e| anyhow!("commit WIP: {}", e))?;

    on_step("Pushing branch...");
    run_local(Some(&wt.path), &["git", "push", "-u", "origin", &branch])
        .map_err(|e| anyhow!("git push: {}", e))?;

    on_step("Creating remote worktree...");
    let remote_path = derive_remote_worktree_path(&remote.repo_path, &branch);

    let fetch_cmd = format!(
        "cd {} && git fetch origin {}",
        remote::shell_escape(&remote.repo_path), remote::shell_escape(&branch)
    );
    remote::ssh_exec(&remote.host, &fetch_cmd)
        .map_err(|e| anyhow!("remote git fetch: {}", e))?;

    let add_cmd = format!(
        "cd {} && git worktree add {} {}",
        remote::shell_escape(&remote.repo_path),
        remote::shell_escape(&remote_path),
        remote::shell_escape(&branch)
    );
    if remote::ssh_exec(&remote.host, &add_cmd).is_err() {
        let pull_cmd = format!(
            "cd {} && git pull origin {}",
            remote::shell_escape(&remote_path), remote::shell_escape(&branch)
        );
        remote::ssh_exec(&remote.host, &pull_cmd)
            .map_err(|e| anyhow!("remote worktree add and pull both failed: {}", e))?;
    }

    on_step("Creating session...");
    let repo_basename = std::path::Path::new(&remote.repo_path)
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or("orchard");
    let session_name = tmux::derive_session_name(repo_basename, Some(&branch), &remote_path);
    remote::create_remote_session(&remote.host, &session_name, &remote_path)
        .map_err(|e| anyhow!("create remote session: {}", e))?;

    on_step("Cleaning up...");
    if let Some(ref sess) = wt.tmux_session {
        let _ = Command::new("tmux")
            .args(["kill-session", "-t", sess])
            .status();
    }

    on_step("Removing worktree...");
    run_local(None, &["git", "worktree", "remove", "--force", &wt.path])
        .map_err(|e| anyhow!("remove local worktree: {}", e))?;

    on_step("Done");
    Ok(())
}

// ---------------------------------------------------------------------------
// Pull to local
// ---------------------------------------------------------------------------

/// Transfers a remote worktree to the local machine.
///
/// Steps:
/// 1. Verify worktree has a branch.
/// 2. SSH: commit WIP.
/// 3. SSH: `git push origin BRANCH`.
/// 4. Local: `git fetch origin BRANCH`.
/// 5. Local: `git worktree add` (fallback: `git pull`).
/// 6. Copy `.env*` files from main checkout into the new worktree.
/// 7. SSH: kill tmux session.
/// 8. SSH: remove remote worktree.
pub fn pull_to_local(
    wt: &Worktree,
    remote: &RemoteConfig,
    repo_root: &str,
    on_step: &dyn Fn(&str),
) -> anyhow::Result<()> {
    let branch = branch_name(wt)?;

    LOG.info(&format!("pullToLocal: transferring {} from {}", branch, remote.host));

    on_step("Committing changes...");
    let commit_cmd = format!(
        "cd {} && git add -u && (git diff --cached --quiet || git commit -m {})",
        remote::shell_escape(&wt.path), remote::shell_escape(WIP_MESSAGE)
    );
    remote::ssh_exec(&remote.host, &commit_cmd)
        .map_err(|e| anyhow!("remote commit WIP: {}", e))?;

    on_step("Pushing branch...");
    let push_cmd = format!(
        "cd {} && git push origin {}",
        remote::shell_escape(&wt.path), remote::shell_escape(&branch)
    );
    remote::ssh_exec(&remote.host, &push_cmd)
        .map_err(|e| anyhow!("remote git push: {}", e))?;

    on_step("Creating local worktree...");
    let local_path = derive_local_worktree_path(repo_root, &branch);

    run_local(Some(repo_root), &["git", "fetch", "origin", &branch])
        .map_err(|e| anyhow!("local git fetch: {}", e))?;

    if run_local(
        Some(repo_root),
        &["git", "worktree", "add", &local_path, &branch],
    )
    .is_err()
    {
        run_local(Some(&local_path), &["git", "pull", "origin", &branch])
            .map_err(|e| anyhow!("local worktree add and pull both failed: {}", e))?;
    }

    on_step("Copying environment files...");
    copy_env_files(repo_root, &local_path);

    on_step("Cleaning up...");
    if let Some(ref sess) = wt.tmux_session {
        let _ = remote::kill_remote_tmux_session(&remote.host, sess);
    }

    on_step("Removing worktree...");
    remote::remove_remote_worktree(&remote.host, &remote.repo_path, &wt.path)
        .map_err(|e| anyhow!("remove remote worktree: {}", e))?;

    on_step("Done");
    Ok(())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/// Copies `.env*` files from `src` to `dst`, skipping files that already exist.
fn copy_env_files(src: &str, dst: &str) {
    let src_path = std::path::Path::new(src);
    let dst_path = std::path::Path::new(dst);

    let entries = match std::fs::read_dir(src_path) {
        Ok(e) => e,
        Err(_) => return,
    };

    for entry in entries.flatten() {
        let name = entry.file_name();
        let name_str = name.to_string_lossy();
        if name_str.starts_with(".env") && entry.file_type().map(|t| t.is_file()).unwrap_or(false) {
            let dst_file = dst_path.join(&name);
            if !dst_file.exists() {
                match std::fs::copy(entry.path(), &dst_file) {
                    Ok(_) => LOG.info(&format!("transfer: copied {} to worktree", name_str)),
                    Err(e) => LOG.warn(&format!("transfer: failed to copy {}: {}", name_str, e)),
                }
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    // --- sanitize_branch_slug ---

    #[test]
    fn sanitize_replaces_slash_with_dash() {
        assert_eq!(sanitize_branch_slug("feat/my-branch"), "feat-my-branch");
    }

    #[test]
    fn sanitize_strips_special_characters() {
        assert_eq!(sanitize_branch_slug("feat/hello world!"), "feat-helloworld");
    }

    #[test]
    fn sanitize_preserves_dots_dashes_underscores() {
        assert_eq!(sanitize_branch_slug("fix/v1.2_patch"), "fix-v1.2_patch");
    }

    #[test]
    fn sanitize_plain_branch_unchanged() {
        assert_eq!(sanitize_branch_slug("main"), "main");
    }

    // --- derive_remote_worktree_path ---

    #[test]
    fn remote_path_uses_parent_and_slug() {
        let result = derive_remote_worktree_path("/home/user/repo", "feat/my-feature");
        assert_eq!(result, "/home/user/worktrees/worktree-feat-my-feature");
    }

    #[test]
    fn remote_path_handles_main() {
        let result = derive_remote_worktree_path("/srv/repos/orchard", "main");
        assert_eq!(result, "/srv/repos/worktrees/worktree-main");
    }

    // --- derive_local_worktree_path ---

    #[test]
    fn local_path_uses_parent_and_slug() {
        let result = derive_local_worktree_path("/home/user/repo", "feat/my-feature");
        // The path may be canonicalized (if it exists) or constructed.
        // We verify the structure: ends with worktrees/worktree-feat-my-feature.
        assert!(
            result.ends_with("worktrees/worktree-feat-my-feature"),
            "got: {}",
            result
        );
    }

    #[test]
    fn local_path_parent_segment_correct() {
        let result = derive_local_worktree_path("/srv/repos/myrepo", "fix/bug-101");
        assert!(
            result.contains("worktrees/worktree-fix-bug-101"),
            "got: {}",
            result
        );
    }

    // --- extract_issue_number (re-exported via github module, tested here too) ---

    #[test]
    fn issue_number_from_keyword_branch() {
        assert_eq!(
            crate::github::extract_issue_number("issue/42"),
            Some(42)
        );
    }

    #[test]
    fn issue_number_from_feat_prefix() {
        assert_eq!(
            crate::github::extract_issue_number("feat/Issue-200-add-thing"),
            Some(200)
        );
    }

    // --- copy_env_files ---

    #[test]
    fn copy_env_files_copies_dotenv_files() {
        let src = tempfile::tempdir().unwrap();
        let dst = tempfile::tempdir().unwrap();

        std::fs::write(src.path().join(".env"), "KEY=val").unwrap();
        std::fs::write(src.path().join(".env.local"), "LOCAL=1").unwrap();

        copy_env_files(src.path().to_str().unwrap(), dst.path().to_str().unwrap());

        assert!(dst.path().join(".env").exists(), ".env should be copied");
        assert!(dst.path().join(".env.local").exists(), ".env.local should be copied");
    }

    #[test]
    fn copy_env_files_skips_existing_files() {
        let src = tempfile::tempdir().unwrap();
        let dst = tempfile::tempdir().unwrap();

        std::fs::write(src.path().join(".env"), "FROM_SRC=1").unwrap();
        std::fs::write(dst.path().join(".env"), "ORIGINAL=1").unwrap();

        copy_env_files(src.path().to_str().unwrap(), dst.path().to_str().unwrap());

        let content = std::fs::read_to_string(dst.path().join(".env")).unwrap();
        assert_eq!(content, "ORIGINAL=1", "existing dst .env must not be overwritten");
    }

    #[test]
    fn copy_env_files_skips_directories() {
        let src = tempfile::tempdir().unwrap();
        let dst = tempfile::tempdir().unwrap();

        std::fs::create_dir(src.path().join(".env_dir")).unwrap();

        copy_env_files(src.path().to_str().unwrap(), dst.path().to_str().unwrap());

        assert!(!dst.path().join(".env_dir").exists(), ".env_dir directory must not be copied");
    }

    #[test]
    fn copy_env_files_handles_missing_src() {
        let dst = tempfile::tempdir().unwrap();
        let missing = dst.path().join("nonexistent_src");

        // Must not panic
        copy_env_files(missing.to_str().unwrap(), dst.path().to_str().unwrap());
    }
}
