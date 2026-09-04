//! Build script for the `orchard` crate.
//!
//! Two jobs:
//!
//! 1. Generates `schema.json` at the crate root from the `JsonOutput`
//!    wire-format type definitions in `src/json_output_types.rs`. The schema is
//!    committed so agents and scripts can reference it without running the
//!    binary, and embedded into the binary via `include_str!` for `--schema`.
//!
//! 2. Stamps the VCS revision into `ORCHARD_REVISION` so `orchard-tui
//!    --revision` reports the commit it was built from. This mirrors the Go
//!    `internal/release.Revision()` semantics so orchard-shell's doctor can
//!    compare revisions across the whole suite (orchardist#787, #807).
//!
//! Idempotent: schema.json is only written when its contents change, preventing
//! spurious rebuild loops.

use schemars::schema_for;
use std::{env, fs, path::PathBuf, process::Command};

#[allow(dead_code)]
mod types {
    use schemars::JsonSchema;
    use serde::{Deserialize, Serialize};
    use std::collections::HashMap;
    include!("src/json_output_types.rs");
}

fn main() {
    println!("cargo:rerun-if-changed=src/json_output_types.rs");
    println!("cargo:rerun-if-changed=build.rs");

    emit_revision();
    generate_schema();
}

fn generate_schema() {
    let schema = schema_for!(types::JsonOutput);
    let pretty = serde_json::to_string_pretty(&schema).expect("schema serialization failed");

    // Write next to Cargo.toml (crate root = CARGO_MANIFEST_DIR).
    let crate_dir = PathBuf::from(env::var("CARGO_MANIFEST_DIR").unwrap());
    let out = crate_dir.join("schema.json");

    // Only write if contents differ to avoid spurious rebuild loops.
    let changed = match fs::read_to_string(&out) {
        Ok(existing) => existing != pretty,
        Err(_) => true,
    };
    if changed {
        fs::write(&out, &pretty).expect("failed to write schema.json");
    }
}

/// Emits `cargo:rustc-env=ORCHARD_REVISION=<value>` plus the rerun triggers so
/// a new commit (or a changed `ORCHARD_REVISION`) forces a rebuild.
fn emit_revision() {
    println!("cargo:rerun-if-env-changed=ORCHARD_REVISION");
    emit_git_rerun();
    println!("cargo:rustc-env=ORCHARD_REVISION={}", revision_value());
}

/// Resolves this build's revision, mirroring Go `release.Revision()`:
///   - `ORCHARD_REVISION` when set and non-empty (tarball builds pass it in);
///   - else `git rev-parse HEAD`, with a `+dirty` suffix when the working tree
///     has uncommitted changes (Go's `vcs.modified`);
///   - else empty (no git, no override).
fn revision_value() -> String {
    if let Ok(v) = env::var("ORCHARD_REVISION") {
        if !v.is_empty() {
            return v;
        }
    }
    let head = match git(&["rev-parse", "HEAD"]) {
        Some(h) if !h.is_empty() => h,
        _ => return String::new(),
    };
    if git_dirty() {
        format!("{head}+dirty")
    } else {
        head
    }
}

/// Runs a git command from the crate dir, returning trimmed stdout on success.
fn git(args: &[&str]) -> Option<String> {
    let out = Command::new("git").args(args).output().ok()?;
    if !out.status.success() {
        return None;
    }
    Some(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

/// True when the working tree has changes — matches Go `vcs.modified`, which
/// treats any `git status --porcelain` output (tracked or untracked) as dirty.
fn git_dirty() -> bool {
    git(&["status", "--porcelain"]).is_some_and(|s| !s.is_empty())
}

/// Tells cargo to rebuild when HEAD moves or the checked-out branch ref updates,
/// so a fresh commit re-stamps the revision.
///
/// Limitation: `+dirty` is only re-sampled on a rerun trigger (HEAD/ref move,
/// or a build.rs edit), so a local edit made between reruns can leave the
/// stamp stale (missing `+dirty`) until something above fires again — the same
/// caveat vergen/built carry for their dirty-tracking.
fn emit_git_rerun() {
    if let Some(head_path) = git(&["rev-parse", "--git-path", "HEAD"]) {
        println!("cargo:rerun-if-changed={head_path}");
    }
    // `symbolic-ref` exits non-zero (→ None) on a detached HEAD, which has no
    // ref file to watch.
    if let Some(ref_name) = git(&["symbolic-ref", "--quiet", "HEAD"]) {
        if !ref_name.is_empty() {
            if let Some(ref_path) = git(&["rev-parse", "--git-path", &ref_name]) {
                println!("cargo:rerun-if-changed={ref_path}");
            }
        }
    }
}
