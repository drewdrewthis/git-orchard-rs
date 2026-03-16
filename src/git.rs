use std::path::Path;
use std::process::Command;

use anyhow::{anyhow, Context, Result};

use crate::logger::LOG;
use crate::types::Worktree;

/// Returns the absolute path of the git repository root, or an empty string on failure.
pub fn find_repo_root() -> String {
    Command::new("git")
        .args(["rev-parse", "--show-toplevel"])
        .output()
        .ok()
        .filter(|o| o.status.success())
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
        .unwrap_or_default()
}

/// Returns the directory name of the git repository root.
pub fn get_repo_name() -> String {
    let root = find_repo_root();
    Path::new(&root)
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or("")
        .to_string()
}

/// Returns all git worktrees for the current repository with conflict status populated.
pub fn list_worktrees() -> Result<Vec<Worktree>> {
    let root = find_repo_root();
    let out = Command::new("git")
        .args(["worktree", "list", "--porcelain"])
        .current_dir(&root)
        .output()
        .context("running git worktree list")?;

    if !out.status.success() {
        return Err(anyhow!(
            "git worktree list failed: {}",
            String::from_utf8_lossy(&out.stderr)
        ));
    }

    let raw = parse_porcelain(&String::from_utf8_lossy(&out.stdout));
    let mut trees = Vec::with_capacity(raw.len());

    for mut wt in raw {
        // Resolve paths that live inside a .git directory (bare-repo worktrees).
        if wt.path.contains("/.git/") {
            wt.path = resolve_main_worktree_path(&wt.path);
        }
        if !wt.is_bare {
            wt.has_conflicts = worktree_has_conflicts(&wt.path);
        }
        trees.push(wt);
    }

    LOG.info(&format!("listWorktrees: {} trees", trees.len()));
    Ok(trees)
}

/// Parses the output of `git worktree list --porcelain` into a `Vec<Worktree>`.
/// Blocks are separated by blank lines.
pub fn parse_porcelain(output: &str) -> Vec<Worktree> {
    let mut worktrees = Vec::new();

    for block in output.trim().split("\n\n") {
        let block = block.trim();
        if block.is_empty() {
            continue;
        }

        let mut path = String::new();
        let mut head = String::new();
        let mut branch: Option<String> = None;
        let mut is_bare = false;

        for line in block.lines() {
            if let Some(rest) = line.strip_prefix("worktree ") {
                path = rest.to_string();
            } else if let Some(rest) = line.strip_prefix("HEAD ") {
                head = rest.to_string();
            } else if let Some(rest) = line.strip_prefix("branch ") {
                let name = rest.strip_prefix("refs/heads/").unwrap_or(rest);
                branch = Some(name.to_string());
            } else if line == "bare" {
                is_bare = true;
            } else if line == "detached" {
                branch = None;
            }
        }

        if path.is_empty() {
            continue;
        }

        worktrees.push(Worktree {
            path,
            head,
            branch,
            is_bare,
            has_conflicts: false,
            ..Default::default()
        });
    }

    worktrees
}

/// Reports whether the worktree at `path` has unmerged (conflicted) files.
pub fn worktree_has_conflicts(path: &str) -> bool {
    Command::new("git")
        .args(["diff", "--name-only", "--diff-filter=U"])
        .current_dir(path)
        .output()
        .ok()
        .filter(|o| o.status.success())
        .map(|o| !o.stdout.trim_ascii().is_empty())
        .unwrap_or(false)
}

/// Removes the worktree at `path`. If `force` is true, passes `--force` to git.
/// Falls back to `rm -rf` + `git worktree prune` if git worktree remove fails,
/// but only after verifying the path is a known worktree.
pub fn remove_worktree(path: &str, force: bool) -> Result<()> {
    let mut args = vec!["worktree", "remove", path];
    if force {
        args.push("--force");
    }

    if Command::new("git").args(&args).status().map(|s| s.success()).unwrap_or(false) {
        LOG.info(&format!("removeWorktree: removed {}", path));
        return Ok(());
    }

    // Fallback: worktree may be in a broken state.
    LOG.warn(&format!("removeWorktree: git remove failed for {}, falling back to rm + prune", path));
    let resolved = std::fs::canonicalize(path)
        .with_context(|| format!("canonicalizing path: {path}"))?;
    let resolved_str = resolved.to_string_lossy();

    if !is_known_worktree(&resolved_str) {
        return Err(anyhow!(
            "refusing to rm path not listed as a git worktree: {path}"
        ));
    }

    Command::new("rm")
        .args(["-rf", &resolved_str])
        .status()
        .context("rm -rf worktree")?;

    Command::new("git")
        .args(["worktree", "prune"])
        .status()
        .context("git worktree prune")?;

    Ok(())
}

// Resolves the actual worktree path by consulting core.worktree config with GIT_DIR set.
fn resolve_main_worktree_path(git_dir: &str) -> String {
    let out = Command::new("git")
        .args(["config", "--get", "core.worktree"])
        .env("GIT_DIR", git_dir)
        .output();

    match out {
        Ok(o) if o.status.success() => {
            let rel = String::from_utf8_lossy(&o.stdout).trim().to_string();
            let joined = Path::new(git_dir).join(&rel);
            // Try filesystem canonicalize first, fall back to logical normalization.
            joined.canonicalize()
                .unwrap_or_else(|_| normalize_path(&joined))
                .to_string_lossy()
                .into_owned()
        }
        _ => git_dir.to_string(),
    }
}

/// Normalize a path by resolving `.` and `..` components without hitting the filesystem.
fn normalize_path(path: &Path) -> std::path::PathBuf {
    use std::path::Component;
    let mut components: Vec<Component> = Vec::new();
    for component in path.components() {
        match component {
            Component::ParentDir => {
                if !components.is_empty() {
                    components.pop();
                }
            }
            Component::CurDir => {}
            other => components.push(other),
        }
    }
    components.iter().collect()
}

// Reports whether `path` appears in `git worktree list --porcelain`.
fn is_known_worktree(path: &str) -> bool {
    Command::new("git")
        .args(["worktree", "list", "--porcelain"])
        .output()
        .ok()
        .filter(|o| o.status.success())
        .map(|o| {
            let s = String::from_utf8_lossy(&o.stdout);
            s.contains(&format!("worktree {path}"))
        })
        .unwrap_or(false)
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE: &str = "\
worktree /home/user/project
HEAD abc123def456
branch refs/heads/main

worktree /home/user/project-feature
HEAD 111222333444
branch refs/heads/feature/my-work

worktree /home/user/project-detached
HEAD deadbeef1234
detached

worktree /home/user/bare.git
HEAD 0000000000000
bare";

    #[test]
    fn parse_porcelain_main_worktree() {
        let wts = parse_porcelain(SAMPLE);
        assert_eq!(wts[0].path, "/home/user/project");
        assert_eq!(wts[0].head, "abc123def456");
        assert_eq!(wts[0].branch.as_deref(), Some("main"));
        assert!(!wts[0].is_bare);
    }

    #[test]
    fn parse_porcelain_strips_refs_heads_prefix() {
        let wts = parse_porcelain(SAMPLE);
        assert_eq!(wts[1].branch.as_deref(), Some("feature/my-work"));
    }

    #[test]
    fn parse_porcelain_detached_head() {
        let wts = parse_porcelain(SAMPLE);
        assert!(wts[2].branch.is_none());
        assert!(!wts[2].is_bare);
    }

    #[test]
    fn parse_porcelain_bare_worktree() {
        let wts = parse_porcelain(SAMPLE);
        assert!(wts[3].is_bare);
    }

    #[test]
    fn parse_porcelain_returns_four_entries() {
        let wts = parse_porcelain(SAMPLE);
        assert_eq!(wts.len(), 4);
    }

    #[test]
    fn parse_porcelain_empty_input() {
        assert!(parse_porcelain("").is_empty());
    }

    #[test]
    fn parse_porcelain_skips_empty_blocks() {
        let input = "\n\nworktree /tmp/x\nHEAD abc\nbranch refs/heads/main\n\n\n";
        let wts = parse_porcelain(input);
        assert_eq!(wts.len(), 1);
    }

    #[test]
    fn normalize_path_resolves_parent_components() {
        use std::path::PathBuf;
        let p = Path::new("/a/b/c/../../../d");
        assert_eq!(normalize_path(p), PathBuf::from("/d"));
    }

    #[test]
    fn normalize_path_preserves_clean_path() {
        use std::path::PathBuf;
        let p = Path::new("/a/b/c");
        assert_eq!(normalize_path(p), PathBuf::from("/a/b/c"));
    }

    #[test]
    fn normalize_path_resolves_dot_components() {
        use std::path::PathBuf;
        let p = Path::new("/a/./b/./c");
        assert_eq!(normalize_path(p), PathBuf::from("/a/b/c"));
    }

    #[test]
    fn normalize_path_resolves_git_modules_path() {
        use std::path::PathBuf;
        // .git/modules/sub is 3 components deep from repo root, so 3 x ".." = repo root
        let p = Path::new("/home/user/repo/.git/modules/sub/../../../actual");
        assert_eq!(normalize_path(p), PathBuf::from("/home/user/repo/actual"));
    }
}
