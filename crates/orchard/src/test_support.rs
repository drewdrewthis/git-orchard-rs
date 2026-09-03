//! Shared helpers for this crate's own unit tests.
//!
//! Compiled only under `cfg(test)`; nothing here ships in the binary.

use std::sync::{Mutex, MutexGuard};

/// Crate-wide lock for tests that mutate process-global environment variables.
///
/// `std::env::set_var` / `remove_var` mutate the whole process, and cargo runs
/// a crate's unit tests as parallel threads of **one** process — so an
/// env-mutating test must serialise against every other env-mutating test in
/// the crate, not merely against its module siblings. Per-module locks do not
/// compose: a `HOME` writer in `federation` and a `HOME` writer in
/// `watch::daemon` holding different mutexes still race.
///
/// Resolves issue #690 (and supersedes the per-module locks added for #347).
static ENV_LOCK: Mutex<()> = Mutex::new(());

/// Acquires [`ENV_LOCK`] for the remainder of the calling test.
///
/// Bind the guard (`let _guard = env_lock();`) — binding to `_` drops it
/// immediately and silently removes the serialisation.
///
/// Poisoning is recovered from deliberately: one panicking test must not
/// cascade into a failure in every other env-mutating test.
pub(crate) fn env_lock() -> MutexGuard<'static, ()> {
    ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner())
}
