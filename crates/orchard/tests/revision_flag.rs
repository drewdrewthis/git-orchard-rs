//! `orchard-tui --revision` must answer with exactly one line so orchard-shell's
//! doctor can compare the build revision across the suite (orchardist#807). The
//! value is build-tree dependent, so this asserts the SHAPE (exit 0, one line),
//! never the value.

use std::process::{Command, Stdio};

fn orchard_bin() -> Command {
    Command::new(env!("CARGO_BIN_EXE_orchard-tui"))
}

/// `--revision` exits 0 and writes exactly one line: one trailing newline and no
/// other newlines, mirroring the Go suite binaries' `HandleRevisionFlag`.
#[test]
fn orchard_revision_prints_exactly_one_line() {
    let output = orchard_bin()
        .arg("--revision")
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .output()
        .expect("orchard-tui --revision must exec");

    assert!(
        output.status.success(),
        "orchard-tui --revision exited non-zero: stderr={}",
        String::from_utf8_lossy(&output.stderr),
    );

    let stdout = String::from_utf8(output.stdout).expect("stdout must be UTF-8");
    assert!(
        stdout.ends_with('\n'),
        "stdout must end with a newline: {stdout:?}"
    );
    assert_eq!(
        stdout.matches('\n').count(),
        1,
        "stdout must be exactly one line: {stdout:?}"
    );
}
