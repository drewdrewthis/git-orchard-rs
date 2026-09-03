//! The side-effect port used by [`crate::heal::apply_fixes_with`].
//!
//! `heal.rs` splits into a pure functional core (`diagnose`) and an imperative
//! shell (`apply_fixes`). This module is the seam between them: the shell's
//! external-world effects — killing tmux sessions, deleting files — behind a
//! trait so unit tests observe what *would* have happened instead of doing it.
//!
//! Resolves issue #372.

use std::sync::Mutex;

use crate::tmux;

// ---------------------------------------------------------------------------
// Port
// ---------------------------------------------------------------------------

/// The external-world effects [`crate::heal::apply_fixes_with`] performs.
///
/// [`ProcessFixExecutor`] is the production implementation;
/// [`RecordingFixExecutor`] is the test double.
///
/// Errors are `String` rather than `anyhow::Error` so the trait stays
/// object-safe and matches the shape of [`crate::heal::FixResult::error`].
pub trait FixExecutor {
    /// Name of the tmux session the calling process runs inside, if any.
    ///
    /// Feeds the self-kill guard; `None` means "not inside tmux".
    fn current_session(&self) -> Option<String>;

    /// Kills tmux session `name`, refusing when it equals `current_session`.
    ///
    /// Implementations MUST honour that refusal — it is the invariant from
    /// issue #369 that stops orchard killing its own host session.
    fn kill_session(&self, name: &str, current_session: Option<&str>) -> Result<(), String>;

    /// Deletes the file at `path`.
    ///
    /// The caller has already checked that `path` is inside an allowed
    /// directory; implementations do not re-check.
    fn remove_file(&self, path: &str) -> Result<(), String>;
}

// ---------------------------------------------------------------------------
// Production executor
// ---------------------------------------------------------------------------

/// Production [`FixExecutor`]: real tmux kills and real filesystem deletes.
pub struct ProcessFixExecutor;

impl FixExecutor for ProcessFixExecutor {
    fn current_session(&self) -> Option<String> {
        tmux::current_session_name()
    }

    fn kill_session(&self, name: &str, current_session: Option<&str>) -> Result<(), String> {
        tmux::kill_tmux_session_safe(name, current_session).map_err(|e| e.to_string())
    }

    fn remove_file(&self, path: &str) -> Result<(), String> {
        std::fs::remove_file(path).map_err(|e| e.to_string())
    }
}

// ---------------------------------------------------------------------------
// Recording test double
// ---------------------------------------------------------------------------

/// One effect recorded by [`RecordingFixExecutor`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum FixCall {
    /// A `kill_session` call.
    KillSession {
        /// Target session name.
        name: String,
        /// The `current_session` argument the call was made with — asserting
        /// on it proves the self-kill guard was actually wired through.
        current_session: Option<String>,
    },
    /// A `remove_file` call.
    RemoveFile {
        /// Target path.
        path: String,
    },
}

/// Recording [`FixExecutor`] test double.
///
/// Executes nothing: every call is appended to an in-memory log that tests
/// assert on ("kill was / was-not invoked"), so heal tests never reach a real
/// tmux server or the filesystem.
///
/// Defaults to "not inside tmux, everything succeeds"; use the builders to
/// change either.
#[derive(Default)]
pub struct RecordingFixExecutor {
    current_session: Option<String>,
    kill_error: Option<String>,
    calls: Mutex<Vec<FixCall>>,
}

impl RecordingFixExecutor {
    /// Creates a recorder that reports "not inside tmux" and succeeds at
    /// every effect.
    pub fn new() -> Self {
        Self::default()
    }

    /// Sets the value [`FixExecutor::current_session`] returns.
    pub fn with_current_session(mut self, name: impl Into<String>) -> Self {
        self.current_session = Some(name.into());
        self
    }

    /// Makes every `kill_session` call fail with `msg`.
    ///
    /// The real tmux path cannot produce this: `tmux kill-session` on a
    /// missing session still exits via `Command::status()` as `Ok`, so the
    /// `Failed` outcome was untestable before the seam existed.
    pub fn failing_kill(mut self, msg: impl Into<String>) -> Self {
        self.kill_error = Some(msg.into());
        self
    }

    /// Returns every recorded call, in invocation order.
    pub fn calls(&self) -> Vec<FixCall> {
        self.calls.lock().unwrap_or_else(|e| e.into_inner()).clone()
    }

    /// Returns the session names passed to `kill_session`, in order.
    pub fn killed_sessions(&self) -> Vec<String> {
        self.calls()
            .into_iter()
            .filter_map(|c| match c {
                FixCall::KillSession { name, .. } => Some(name),
                FixCall::RemoveFile { .. } => None,
            })
            .collect()
    }

    /// Returns the paths passed to `remove_file`, in order.
    pub fn removed_files(&self) -> Vec<String> {
        self.calls()
            .into_iter()
            .filter_map(|c| match c {
                FixCall::RemoveFile { path } => Some(path),
                FixCall::KillSession { .. } => None,
            })
            .collect()
    }

    fn record(&self, call: FixCall) {
        self.calls
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(call);
    }
}

impl FixExecutor for RecordingFixExecutor {
    fn current_session(&self) -> Option<String> {
        self.current_session.clone()
    }

    fn kill_session(&self, name: &str, current_session: Option<&str>) -> Result<(), String> {
        self.record(FixCall::KillSession {
            name: name.to_string(),
            current_session: current_session.map(str::to_string),
        });
        match &self.kill_error {
            Some(msg) => Err(msg.clone()),
            None => Ok(()),
        }
    }

    fn remove_file(&self, path: &str) -> Result<(), String> {
        self.record(FixCall::RemoveFile {
            path: path.to_string(),
        });
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn recording_executor_defaults_to_no_current_session_and_no_calls() {
        let exec = RecordingFixExecutor::new();
        assert_eq!(exec.current_session(), None);
        assert!(exec.calls().is_empty());
    }

    #[test]
    fn recording_executor_captures_calls_in_order() {
        let exec = RecordingFixExecutor::new();
        exec.kill_session("alpha", Some("host")).unwrap();
        exec.remove_file("/tmp/orchard-claude-x").unwrap();
        exec.kill_session("beta", None).unwrap();

        assert_eq!(
            exec.calls(),
            vec![
                FixCall::KillSession {
                    name: "alpha".to_string(),
                    current_session: Some("host".to_string()),
                },
                FixCall::RemoveFile {
                    path: "/tmp/orchard-claude-x".to_string(),
                },
                FixCall::KillSession {
                    name: "beta".to_string(),
                    current_session: None,
                },
            ]
        );
        assert_eq!(exec.killed_sessions(), vec!["alpha", "beta"]);
        assert_eq!(exec.removed_files(), vec!["/tmp/orchard-claude-x"]);
    }

    #[test]
    fn failing_kill_records_the_call_and_returns_the_error() {
        let exec = RecordingFixExecutor::new().failing_kill("tmux: no server");
        let err = exec.kill_session("alpha", None).unwrap_err();
        assert_eq!(err, "tmux: no server");
        assert_eq!(
            exec.killed_sessions(),
            vec!["alpha"],
            "a failing kill must still be recorded as attempted"
        );
    }

    /// The production executor must honour the issue #369 self-kill refusal
    /// without reaching tmux — the guard short-circuits before any exec.
    #[test]
    fn process_executor_refuses_to_kill_its_own_session() {
        let err = ProcessFixExecutor
            .kill_session("orchardist", Some("orchardist"))
            .unwrap_err();
        assert!(
            err.contains("refusing to kill"),
            "must surface the self-kill guard message: {err}"
        );
    }
}
