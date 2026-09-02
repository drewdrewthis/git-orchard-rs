//! Thin GraphQL client over the orchard daemon (`http://127.0.0.1:7777/graphql`).
//!
//! The TUI is a consumer, not a duplicator. Every fact about hosts, sessions,
//! peers, and Claude instances comes through this module. The daemon's schema
//! (see `schema.graphql`) is the contract; this module exposes the narrow
//! slice of it the TUI actually uses.
//!
//! Federation: peers are reached over their own daemon (`https://graphql.<addr>/graphql`)
//! the same way the local one is. The peerproxy is intentionally NOT used for
//! read-path discovery here — fan-out from the TUI keeps the failure mode
//! per-peer and surfaces directly in the dashboard.
//!
//! Daemon-down handling: callers receive [`DaemonError::Unreachable`] with the
//! probed URL. The TUI surfaces this in the status line; no silent fallback
//! to legacy shell-discovery happens.

pub mod client;
pub mod federated;
pub mod types;
pub mod work_view_adapter;
pub mod work_view_cache;

pub use client::Client;
pub use federated::{FederatedFanout, FederatedSession, PeerFetchResult, fan_out};
pub use types::*;

// ---------------------------------------------------------------------------
// WorkViewSource — injectable abstraction for `work_view()` calls
// ---------------------------------------------------------------------------

/// Abstraction over the daemon's `workView` query.
///
/// The default implementation is [`Client`], which issues a real GraphQL request.
/// In tests, inject a fake that returns a canned snapshot and records calls.
///
/// Only used by `tui::App::start_full_refresh` (Phase 3) and
/// `tui::App::start_local_refresh` (Phase 4).
pub trait WorkViewSource: Send + Sync {
    /// Fetches the current [`WorkViewSnapshot`] from the daemon.
    ///
    /// # Errors
    ///
    /// Returns [`DaemonError::Unreachable`] when the daemon is not reachable.
    fn work_view(&self) -> Result<WorkViewSnapshot, DaemonError>;
}

impl WorkViewSource for Client {
    fn work_view(&self) -> Result<WorkViewSnapshot, DaemonError> {
        self.work_view()
    }
}

use std::env;
use std::fmt;

/// Default URL for the local daemon's GraphQL endpoint.
pub const DEFAULT_DAEMON_URL: &str = "http://127.0.0.1:7777/graphql";

/// Environment variable that overrides [`DEFAULT_DAEMON_URL`].
pub const DAEMON_URL_ENV: &str = "ORCHARD_DAEMON_URL";

/// Resolves the daemon URL. Reads `ORCHARD_DAEMON_URL` if set, otherwise the
/// hardcoded local default.
pub fn resolve_daemon_url() -> String {
    env::var(DAEMON_URL_ENV).unwrap_or_else(|_| DEFAULT_DAEMON_URL.to_string())
}

/// Errors that can occur talking to a daemon.
#[derive(Debug)]
pub enum DaemonError {
    /// The daemon refused or timed out the connection at the given URL.
    Unreachable {
        /// The URL we tried to reach.
        url: String,
        /// Underlying error description.
        cause: String,
    },
    /// Network or HTTP-level failure that wasn't a clean refusal.
    Transport(String),
    /// The daemon returned a non-2xx status.
    HttpStatus {
        /// HTTP status code.
        status: u16,
        /// Response body, truncated.
        body: String,
    },
    /// The response was 2xx but didn't parse as expected.
    Parse(String),
    /// The GraphQL response carried `errors`.
    GraphQl(Vec<String>),
}

impl fmt::Display for DaemonError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            DaemonError::Unreachable { url, cause } => write!(
                f,
                "daemon not reachable at {url}: {cause}\n\
                 hint: start it with `orchard daemon start`"
            ),
            DaemonError::Transport(msg) => write!(f, "transport error: {msg}"),
            DaemonError::HttpStatus { status, body } => {
                write!(f, "daemon returned HTTP {status}: {body}")
            }
            DaemonError::Parse(msg) => write!(f, "failed to parse daemon response: {msg}"),
            DaemonError::GraphQl(errs) => write!(f, "GraphQL errors: {}", errs.join("; ")),
        }
    }
}

impl std::error::Error for DaemonError {}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_support::env_lock;

    #[test]
    fn default_url_is_local_daemon() {
        let _guard = env_lock();
        let prior = env::var(DAEMON_URL_ENV).ok();
        unsafe {
            env::remove_var(DAEMON_URL_ENV);
        }
        let actual = resolve_daemon_url();
        // Restore BEFORE asserting so a panic doesn't leak state to other tests.
        if let Some(v) = prior {
            unsafe {
                env::set_var(DAEMON_URL_ENV, v);
            }
        }
        assert_eq!(actual, DEFAULT_DAEMON_URL);
    }

    #[test]
    fn env_var_overrides_default() {
        let _guard = env_lock();
        let prior = env::var(DAEMON_URL_ENV).ok();
        unsafe {
            env::set_var(DAEMON_URL_ENV, "http://example.invalid:9999/graphql");
        }
        let actual = resolve_daemon_url();
        match prior {
            Some(v) => unsafe { env::set_var(DAEMON_URL_ENV, v) },
            None => unsafe { env::remove_var(DAEMON_URL_ENV) },
        }
        assert_eq!(actual, "http://example.invalid:9999/graphql");
    }
}
