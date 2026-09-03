//! Root library crate for Orchard.
//!
//! Re-exports all public modules that make up the functional core and imperative
//! shell of the application. Consumers should import from the top-level module
//! rather than reaching into sub-modules directly.
#![warn(missing_docs)]

mod browser;
pub mod build_state;
pub mod cache;
pub mod cache_sources;
pub mod chat;
pub mod ci_state;
pub mod classify;
pub mod claude_state;
pub mod cli;
pub mod config;
pub mod daemon;
pub mod derive;
pub mod events;
pub mod federation;
pub mod federation_topology;
pub mod git_parse;
pub mod github;
pub mod global_config;
pub mod heal;
pub mod join;
pub mod json_output;
pub mod logger;
pub mod merge_readiness;
pub mod merge_remote;
pub mod models;
mod navigation;
pub mod notify;
pub mod orchard_snapshot;
pub mod orchard_state;
pub mod paths;
pub mod priority;
pub mod refresh_parallel;
pub mod remote;
pub mod remote_adapter;
pub mod restore;
pub mod review_comment;
pub mod session;
pub mod sessions_cli;
pub mod sessions_index;
pub mod setup_remote;
pub mod shell;
pub mod signal;
pub mod sources;
pub mod swr;
#[cfg(test)]
mod test_support;
pub mod tmux;
pub mod transitive_walker;
pub mod tui;
pub mod types;
pub mod watch;
pub mod webhook;
