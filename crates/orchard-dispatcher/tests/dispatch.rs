//! Integration tests for the orchard dispatcher.
//!
//! Exercises real-process dispatch: builds a temporary `orchard-foo` shell-script
//! "helper" next to a copy of the dispatcher, runs the dispatcher, and asserts
//! on the helper's observed behavior (stdout, exit code, argv echo).

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::Command;

/// Path to the built dispatcher binary.
fn dispatcher_path() -> PathBuf {
    // CARGO sets CARGO_BIN_EXE_<name> for binaries in the package.
    PathBuf::from(env!("CARGO_BIN_EXE_orchard"))
}

/// Creates a temp dir, copies the dispatcher into it, and writes
/// `orchard-<name>` shell-script helpers that echo their argv to stdout and
/// exit with `exit_code`. Returns the temp dir path; the caller drops it.
fn with_helpers(helpers: &[(&str, i32)]) -> tempfile::TempDir {
    let dir = tempfile::TempDir::with_prefix("orchard-dispatcher-test")
        .expect("failed to create temp dir");
    let dispatcher_dest = dir.path().join("orchard");
    fs::copy(dispatcher_path(), &dispatcher_dest).expect("copy dispatcher");
    let mut perms = fs::metadata(&dispatcher_dest).unwrap().permissions();
    perms.set_mode(0o755);
    fs::set_permissions(&dispatcher_dest, perms).unwrap();

    for (name, exit_code) in helpers {
        let helper_path = dir.path().join(format!("orchard-{name}"));
        let script = format!("#!/bin/sh\necho \"[helper:{name}] argv=$*\"\nexit {exit_code}\n");
        fs::write(&helper_path, script).expect("write helper");
        fs::set_permissions(&helper_path, fs::Permissions::from_mode(0o755)).expect("chmod helper");
    }
    dir
}

fn run_dispatcher(dir: &Path, args: &[&str]) -> (String, String, i32) {
    let dispatcher = dir.join("orchard");
    let output = Command::new(&dispatcher)
        .args(args)
        // Empty PATH so the dispatcher must find helpers via its sibling-dir lookup.
        .env("PATH", "")
        .output()
        .expect("run dispatcher");
    (
        String::from_utf8_lossy(&output.stdout).into_owned(),
        String::from_utf8_lossy(&output.stderr).into_owned(),
        output.status.code().unwrap_or(-1),
    )
}

#[test]
fn dispatcher_prints_help_on_dash_h() {
    let dir = with_helpers(&[]);
    let (stdout, _stderr, code) = run_dispatcher(dir.path(), &["--help"]);
    assert_eq!(code, 0);
    assert!(
        stdout.contains("orchard — Git worktree"),
        "stdout: {stdout}"
    );
}

#[test]
fn dispatcher_prints_version_on_dash_v() {
    let dir = with_helpers(&[]);
    let (stdout, _stderr, code) = run_dispatcher(dir.path(), &["--version"]);
    assert_eq!(code, 0);
    assert!(stdout.starts_with("orchard "), "stdout: {stdout}");
}

#[test]
fn no_args_dispatches_to_orchard_tui() {
    let dir = with_helpers(&[("tui", 0)]);
    let (stdout, _stderr, code) = run_dispatcher(dir.path(), &[]);
    assert_eq!(code, 0);
    assert!(stdout.contains("[helper:tui]"), "stdout: {stdout}");
}

#[test]
fn namespace_verb_dispatches_with_remaining_args() {
    let dir = with_helpers(&[("daemon", 0)]);
    let (stdout, _stderr, code) = run_dispatcher(dir.path(), &["daemon", "start", "--foo"]);
    assert_eq!(code, 0);
    assert!(
        stdout.contains("[helper:daemon] argv=start --foo"),
        "stdout: {stdout}"
    );
}

#[test]
fn bare_verb_new_dispatches_to_orchard_worktree_new() {
    let dir = with_helpers(&[("worktree", 0)]);
    let (stdout, _stderr, code) = run_dispatcher(dir.path(), &["new", "412"]);
    assert_eq!(code, 0);
    assert!(
        stdout.contains("[helper:worktree] argv=new 412"),
        "stdout: {stdout}"
    );
}

#[test]
fn bare_verb_ls_dispatches_to_orchard_worktree_ls() {
    let dir = with_helpers(&[("worktree", 0)]);
    let (stdout, _stderr, code) = run_dispatcher(dir.path(), &["ls", "--json"]);
    assert_eq!(code, 0);
    assert!(
        stdout.contains("[helper:worktree] argv=ls --json"),
        "stdout: {stdout}"
    );
}

#[test]
fn bare_verb_send_dispatches_to_orchard_chat_send() {
    let dir = with_helpers(&[("chat", 0)]);
    let (stdout, _stderr, code) = run_dispatcher(dir.path(), &["send", "#general", "hello"]);
    assert_eq!(code, 0);
    assert!(
        stdout.contains("[helper:chat] argv=send #general hello"),
        "stdout: {stdout}"
    );
}

#[test]
fn tui_verb_heal_routes_to_orchard_tui_with_verb_prepended() {
    // `orchard heal --fix` must exec `orchard-tui heal --fix`. The verb
    // itself stays as the first argv entry so orchard-tui's argv parser
    // recognises it.
    let dir = with_helpers(&[("tui", 0)]);
    let (stdout, _stderr, code) = run_dispatcher(dir.path(), &["heal", "--fix"]);
    assert_eq!(code, 0);
    assert!(
        stdout.contains("[helper:tui] argv=heal --fix"),
        "stdout: {stdout}"
    );
}

#[test]
fn tui_verb_setup_remote_routes_to_orchard_tui() {
    let dir = with_helpers(&[("tui", 0)]);
    let (stdout, _stderr, code) = run_dispatcher(dir.path(), &["setup-remote", "drew-mac"]);
    assert_eq!(code, 0);
    assert!(
        stdout.contains("[helper:tui] argv=setup-remote drew-mac"),
        "stdout: {stdout}"
    );
}

#[test]
fn tui_verb_refresh_routes_to_orchard_tui() {
    let dir = with_helpers(&[("tui", 0)]);
    let (stdout, _stderr, code) = run_dispatcher(dir.path(), &["refresh"]);
    assert_eq!(code, 0);
    assert!(
        stdout.contains("[helper:tui] argv=refresh"),
        "stdout: {stdout}"
    );
}

#[test]
fn child_exit_code_is_propagated() {
    let dir = with_helpers(&[("daemon", 7)]);
    let (_stdout, _stderr, code) = run_dispatcher(dir.path(), &["daemon", "status"]);
    assert_eq!(code, 7);
}

#[test]
fn signal_killed_child_returns_128_plus_signum() {
    // SIGTERM = 15 → expected exit code 143 (POSIX convention).
    let dir = tempfile::TempDir::with_prefix("orchard-signal-test").expect("dir");
    let dispatcher_dest = dir.path().join("orchard");
    fs::copy(dispatcher_path(), &dispatcher_dest).expect("copy dispatcher");
    fs::set_permissions(&dispatcher_dest, fs::Permissions::from_mode(0o755)).unwrap();

    // Helper that kills itself with SIGTERM.
    let helper = dir.path().join("orchard-signal");
    fs::write(&helper, "#!/bin/sh\nkill -15 $$\n").unwrap();
    fs::set_permissions(&helper, fs::Permissions::from_mode(0o755)).unwrap();

    let output = Command::new(&dispatcher_dest)
        .args(["signal"])
        .env("PATH", "")
        .output()
        .expect("run dispatcher");
    let code = output.status.code().unwrap_or(-1);
    assert_eq!(
        code, 143,
        "SIGTERM-killed child should return 128+15=143; got {code}"
    );
}

#[test]
fn unknown_verb_returns_127_when_no_helper_exists() {
    let dir = with_helpers(&[]); // no helpers at all
    let (_stdout, stderr, code) = run_dispatcher(dir.path(), &["nonexistent"]);
    assert_eq!(code, 127);
    assert!(
        stderr.contains("unknown command 'nonexistent'"),
        "stderr: {stderr}"
    );
}

#[test]
fn unknown_verb_dispatches_to_third_party_plugin_on_path() {
    // Plugin lives in a SEPARATE dir from the dispatcher; only PATH points to it.
    let plugin_dir = tempfile::TempDir::with_prefix("orchard-plugin-test").expect("plugin dir");
    let plugin = plugin_dir.path().join("orchard-myplugin");
    fs::write(
        &plugin,
        "#!/bin/sh\necho \"[plugin:myplugin] argv=$*\"\nexit 0\n",
    )
    .unwrap();
    fs::set_permissions(&plugin, fs::Permissions::from_mode(0o755)).unwrap();

    let dispatcher_dir =
        tempfile::TempDir::with_prefix("orchard-dispatcher-test").expect("disp dir");
    let dispatcher_dest = dispatcher_dir.path().join("orchard");
    fs::copy(dispatcher_path(), &dispatcher_dest).expect("copy dispatcher");
    fs::set_permissions(&dispatcher_dest, fs::Permissions::from_mode(0o755)).unwrap();

    let output = Command::new(&dispatcher_dest)
        .args(["myplugin", "arg1"])
        .env("PATH", plugin_dir.path())
        .output()
        .expect("run");
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert_eq!(output.status.code(), Some(0));
    assert!(
        stdout.contains("[plugin:myplugin] argv=arg1"),
        "stdout: {stdout}"
    );
}

#[test]
fn sibling_dir_takes_precedence_over_path() {
    // Same verb resolves to a helper in BOTH the sibling dir and PATH;
    // dispatcher must pick the sibling-dir one.
    let path_dir = tempfile::TempDir::with_prefix("path-dir").unwrap();
    let path_helper = path_dir.path().join("orchard-tui");
    fs::write(&path_helper, "#!/bin/sh\necho FROM_PATH\n").unwrap();
    fs::set_permissions(&path_helper, fs::Permissions::from_mode(0o755)).unwrap();

    let dir = with_helpers(&[("tui", 0)]); // sibling dir helper echoes [helper:tui]

    let dispatcher = dir.path().join("orchard");
    let output = Command::new(&dispatcher)
        .args(["tui"])
        .env("PATH", path_dir.path())
        .output()
        .unwrap();
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert!(
        stdout.contains("[helper:tui]"),
        "sibling-dir helper must win; stdout: {stdout}"
    );
    assert!(
        !stdout.contains("FROM_PATH"),
        "PATH helper must not be invoked; stdout: {stdout}"
    );
}

/// Extracts the `[package]` `version` field from raw Cargo.toml text.
///
/// Deliberately minimal (not a full TOML parse): returns the first line's
/// quoted value for a line of the exact shape `version = "..."`, which for
/// a manifest laid out like `orchard`'s and this crate's own Cargo.toml is
/// always `[package].version` (dependency version pins use inline-table
/// syntax and never start a line with `version = `). Mirrors the Makefile's
/// own extraction (`awk -F'"' '/^version = /'`) so both sides read the same
/// shape contract.
fn extract_package_version(cargo_toml: &str) -> String {
    cargo_toml
        .lines()
        .find_map(|line| {
            line.strip_prefix("version = \"")
                .and_then(|rest| rest.strip_suffix('"'))
        })
        .unwrap_or_else(|| panic!("no `version = \"...\"` line found in:\n{cargo_toml}"))
        .to_string()
}

#[test]
fn dispatcher_version_matches_orchard_tui_crate_version() {
    // The suite ships one version (docs/plans/747-product-plan.md D2):
    // orchard-tui's crate (crates/orchard/Cargo.toml) is release-please's
    // sole tracked package, and release-please-config.json links this
    // crate's Cargo.toml to it via an `extra-files` entry so both bump
    // together. If they drift, `orchard --version` disagrees with
    // `orchard-tui --version` and the release tag — this test fails CI
    // before that ships.
    let orchard_cargo_toml = Path::new(env!("CARGO_MANIFEST_DIR")).join("../orchard/Cargo.toml");
    let content = fs::read_to_string(&orchard_cargo_toml)
        .unwrap_or_else(|e| panic!("read {}: {e}", orchard_cargo_toml.display()));
    let orchard_version = extract_package_version(&content);

    assert_eq!(
        env!("CARGO_PKG_VERSION"),
        orchard_version,
        "crates/orchard-dispatcher/Cargo.toml version must match crates/orchard/Cargo.toml \
         (release-please-config.json's extra-files entry keeps them linked)"
    );
}
