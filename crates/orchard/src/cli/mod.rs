//! Domain subcommand wiring for `orchard <domain> <op> ...`.
//!
//! Each domain module wraps the corresponding `scripts/<domain>-<op>.sh` script
//! per **L1** (operations live as scripts) and **L6** (CLI is standalone — no
//! daemon required). Scripts return the **L2** envelope:
//!
//! ```json
//! { "ok": true,  "data": <op-specific> }
//! { "ok": false, "error": { "code": "<str>", "message": "<str>" } }
//! ```
//!
//! The CLI parses stdout only; stderr is free-form for human readers (L2).

use std::path::{Path, PathBuf};
use std::process::{Command, ExitStatus};

use serde::Deserialize;
use serde_json::Value;

pub mod claude_account;
pub mod daemon_cmd;
pub mod gh;
pub mod git;
pub mod host_services;
pub mod ps;
pub mod tmux_cmd;

// ---------------------------------------------------------------------------
// L2 envelope
// ---------------------------------------------------------------------------

/// The L2 script output envelope.
///
/// Every script that the CLI execs must emit this shape on stdout when called
/// with `--json`. `ok: true` implies `data` is present (or explicitly null);
/// `ok: false` implies `error` is present.
#[derive(Debug, Deserialize)]
pub struct ScriptEnvelope {
    /// Whether the script operation succeeded.
    pub ok: bool,
    /// Arbitrary operation-specific result on success.
    pub data: Option<Value>,
    /// Error payload on failure.
    pub error: Option<ScriptError>,
}

/// The error sub-object in a failed L2 envelope.
#[derive(Debug, Deserialize)]
pub struct ScriptError {
    /// Machine-readable code (e.g. `"not_found"`, `"permission_denied"`).
    pub code: String,
    /// Human-readable message.
    pub message: String,
}

// ---------------------------------------------------------------------------
// Script resolution + exec
// ---------------------------------------------------------------------------

/// Resolves the absolute path to a domain script.
///
/// Thin env-reading wrapper over [`resolve_script_inner`]: reads
/// `$ORCHARD_SCRIPTS_DIR` and delegates. See the inner function for the search
/// order and return contract.
pub fn resolve_script(name: &str) -> Option<PathBuf> {
    let override_dir = std::env::var_os("ORCHARD_SCRIPTS_DIR").map(PathBuf::from);
    resolve_script_inner(name, override_dir.as_deref())
}

/// Inner implementation, accepting the `$ORCHARD_SCRIPTS_DIR` value directly.
/// Extracted for hermetic unit testing without env-var manipulation — the
/// same seam-via-injection shape as [`crate::tmux::current_session_name`].
///
/// Search order (first hit wins):
/// 1. `<scripts_dir>/<name>` — test / packaging override, when `Some`.
/// 2. `<cwd>/scripts/<name>` — running from a repo checkout.
/// 3. `~/.orchard/scripts/<name>` — user-installed scripts.
///
/// Returns `None` when the script cannot be found at any location. The caller
/// prints a helpful error and exits non-zero.
///
/// Resolves issue #690.
pub(crate) fn resolve_script_inner(name: &str, scripts_dir: Option<&Path>) -> Option<PathBuf> {
    // 1. Explicit override (test harness, CI, custom packaging).
    if let Some(dir) = scripts_dir {
        let p = dir.join(name);
        if p.exists() {
            return Some(p);
        }
    }

    // 2. Repo-local scripts/ (covers running from a checkout of orchardist).
    let cwd_candidate = std::env::current_dir()
        .map(|d| d.join("scripts").join(name))
        .unwrap_or_default();
    if cwd_candidate.exists() {
        return Some(cwd_candidate);
    }

    // 3. ~/.orchard/scripts/ (user-installed).
    if let Some(home) = dirs::home_dir() {
        let p = home.join(".orchard").join("scripts").join(name);
        if p.exists() {
            return Some(p);
        }
    }

    None
}

/// Outcome of [`exec_script`].
pub struct ScriptOutcome {
    /// Decoded L2 envelope.
    pub envelope: ScriptEnvelope,
    /// Raw OS exit status (useful for pass-through).
    pub status: ExitStatus,
    /// Raw stdout bytes for callers that want them verbatim.
    pub stdout: Vec<u8>,
    /// Raw stderr bytes.
    pub stderr: Vec<u8>,
}

/// Execs a script at `path` with `args` and `--json`, waits for it to exit,
/// and decodes the L2 envelope from stdout.
///
/// # Errors
///
/// Returns an error string when:
/// - The script cannot be spawned (permission denied, interpreter missing, …).
/// - Stdout is not valid UTF-8.
/// - Stdout does not decode as a valid [`ScriptEnvelope`].
///
/// Script-level failures (`ok: false`) are returned inside an `Ok(ScriptOutcome)`
/// so callers can surface the typed `error.code` and `error.message`.
pub fn exec_script(path: &PathBuf, args: &[&str]) -> Result<ScriptOutcome, String> {
    let mut cmd = Command::new("bash");
    cmd.arg(path);
    for a in args {
        cmd.arg(a);
    }
    cmd.arg("--json");

    let output = cmd
        .output()
        .map_err(|e| format!("failed to exec {}: {e}", path.display()))?;

    let stdout_str = String::from_utf8(output.stdout.clone())
        .map_err(|e| format!("script stdout is not UTF-8: {e}"))?;

    let envelope: ScriptEnvelope = serde_json::from_str(stdout_str.trim())
        .map_err(|e| format!("failed to parse L2 envelope: {e}\nraw stdout: {stdout_str}"))?;

    Ok(ScriptOutcome {
        envelope,
        status: output.status,
        stdout: output.stdout,
        stderr: output.stderr,
    })
}

/// Prints the L2 envelope or error, then exits with the appropriate code.
///
/// On `ok: true`: pretty-prints `data` as JSON and exits 0.
/// On `ok: false`: prints `error.code: error.message` to stderr and exits 1.
pub fn emit_or_die(outcome: ScriptOutcome) -> ! {
    if outcome.envelope.ok {
        let data = outcome.envelope.data.unwrap_or(Value::Null);
        println!(
            "{}",
            serde_json::to_string_pretty(&data).unwrap_or_else(|_| "null".to_string())
        );
        std::process::exit(0);
    } else {
        let err = outcome.envelope.error.unwrap_or(ScriptError {
            code: "unknown".to_string(),
            message: "(no error message)".to_string(),
        });
        eprintln!("error [{}]: {}", err.code, err.message);
        std::process::exit(1);
    }
}

/// Emits a "script not found" error and exits 1.
pub fn script_not_found(name: &str) -> ! {
    eprintln!(
        "error: script `{}` not found.\n\
         Search order:\n  \
         1. $ORCHARD_SCRIPTS_DIR/{}\n  \
         2. <cwd>/scripts/{}\n  \
         3. ~/.orchard/scripts/{}",
        name, name, name, name
    );
    std::process::exit(1);
}

// ---------------------------------------------------------------------------
// Shared pass-through helper
// ---------------------------------------------------------------------------

/// Runs a pass-through escape hatch per **S16b**: execs the underlying tool
/// with arbitrary args and prints its stdout verbatim. Not `--json`-wrapped;
/// the raw tool output is the intent.
///
/// `tool` is the binary to exec (e.g. `"tmux"`, `"git"`).
/// `extra_args` are the remaining args after `--`.
pub fn run_passthrough(tool: &str, extra_args: &[String]) {
    let status = Command::new(tool)
        .args(extra_args)
        .status()
        .unwrap_or_else(|e| {
            eprintln!("error: failed to exec `{tool}`: {e}");
            std::process::exit(1);
        });

    std::process::exit(status.code().unwrap_or(1));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn envelope_ok_deserializes() {
        let raw = r#"{"ok": true, "data": {"foo": "bar"}}"#;
        let e: ScriptEnvelope = serde_json::from_str(raw).unwrap();
        assert!(e.ok);
        assert!(e.data.is_some());
        assert!(e.error.is_none());
    }

    #[test]
    fn envelope_err_deserializes() {
        let raw = r#"{"ok": false, "error": {"code": "not_found", "message": "worktree gone"}}"#;
        let e: ScriptEnvelope = serde_json::from_str(raw).unwrap();
        assert!(!e.ok);
        assert!(e.data.is_none());
        let err = e.error.unwrap();
        assert_eq!(err.code, "not_found");
        assert_eq!(err.message, "worktree gone");
    }

    /// The override dir is passed as an argument, so this test mutates no
    /// process-global state and cannot race a sibling test that reads or
    /// clears `ORCHARD_SCRIPTS_DIR`. Regression for issue #690.
    #[test]
    fn resolve_script_env_override() {
        let dir = tempfile::tempdir().unwrap();
        let script = dir.path().join("test-script.sh");
        std::fs::write(&script, "#!/bin/sh\necho ok").unwrap();

        // Make it executable.
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mut perms = std::fs::metadata(&script).unwrap().permissions();
            perms.set_mode(0o755);
            std::fs::set_permissions(&script, perms).unwrap();
        }

        let found = resolve_script_inner("test-script.sh", Some(dir.path()));
        assert_eq!(found, Some(script));
    }

    /// An override dir that exists but does not contain the script falls
    /// through to the later search locations rather than short-circuiting.
    #[test]
    fn resolve_script_override_dir_without_script_falls_through() {
        let dir = tempfile::tempdir().unwrap();
        let found =
            resolve_script_inner("__nonexistent_orchard_test_script__.sh", Some(dir.path()));
        assert!(found.is_none());
    }

    #[test]
    fn resolve_script_missing_returns_none() {
        // No override, and this name won't exist in <cwd>/scripts or
        // ~/.orchard/scripts either.
        let found = resolve_script_inner("__nonexistent_orchard_test_script__.sh", None);
        assert!(found.is_none());
    }
}
