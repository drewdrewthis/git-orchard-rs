use std::io::Write;
use std::path::Path;

/// Returns the orchard popup wrapper script content.
pub fn get_wrapper_script() -> &'static str {
    r#"#!/bin/sh
errfile=$(mktemp /tmp/orchard-err.XXXXXX)
session=$(orchard 2>"$errfile")
rc=$?
if [ $rc -ne 0 ]; then
  msg=$(head -1 "$errfile" 2>/dev/null)
  rm -f "$errfile"
  tmux display-message "orchard: ${msg:-unknown error}"
elif [ -n "$session" ]; then
  rm -f "$errfile"
  tmux switch-client -t "$session"
else
  rm -f "$errfile"
fi"#
}

/// Returns the tmux.conf keybinding line for the popup, using the given key.
pub fn get_tmux_binding(key: &str) -> String {
    format!(r#"bind-key {key} display-popup -E -w 90% -h 80% "$HOME/.local/bin/orchard-popup""#)
}

const MARKER_START: &str = "# >>> orchard >>>";
const MARKER_END: &str = "# <<< orchard <<<";

/// Runs the interactive `orchard init` wizard.
///
/// Walks the user through:
///  1. Checking tmux version
///  2. Removing old shell function markers from RC files
///  3. Installing the wrapper script
///  4. Configuring the tmux keybinding
///  5. Optionally adding a status bar segment
///  6. Reloading tmux config
pub fn run_init_wizard() -> Result<(), String> {
    let home = dirs::home_dir()
        .ok_or_else(|| "Could not determine home directory".to_string())?;

    // Step 1: Check tmux version.
    let tmux_version = check_tmux_version_step()?;

    // Step 2: Check for old shell function.
    remove_old_shell_function_step(&home)?;

    // Step 3: Create wrapper script.
    install_wrapper(&home)?;

    // Steps 4 & 5: Configure tmux keybinding (and optionally status bar).
    let key = configure_tmux_binding_step(&home, &tmux_version)?;

    // Step 6: Reload tmux config.
    reload_tmux_config_step(&home);

    eprintln!();
    eprintln!("Setup complete. Press prefix + {key} to open orchard.");
    Ok(())
}

/// Step 1: Check tmux version. Returns the version string (e.g. "3.4") on success.
fn check_tmux_version_step() -> Result<String, String> {
    eprintln!("Step 1: Checking tmux version...");
    let out = std::process::Command::new("tmux")
        .args(["-V"])
        .output()
        .map_err(|e| format!("Could not run tmux: {e}"))?;

    let version_str = String::from_utf8_lossy(&out.stdout).trim().to_string();
    let version_part = version_str.trim_start_matches("tmux").trim().to_string();

    match parse_tmux_version(&version_part) {
        Some(v) if v >= (3, 2) => {
            eprintln!("  tmux {version_part} detected");
        }
        Some(_) => {
            return Err(format!(
                "tmux >= 3.2 is required for popup mode (you have {version_part}). Please upgrade tmux."
            ));
        }
        None => {
            return Err(format!(
                "Could not parse tmux version from {:?}",
                version_str
            ));
        }
    }

    Ok(version_part)
}

/// Step 2: Check RC files for old shell function markers and optionally remove them.
fn remove_old_shell_function_step(home: &Path) -> Result<(), String> {
    eprintln!("Step 2: Checking for old shell function...");
    for rc in &[".zshrc", ".bashrc"] {
        let rc_path = home.join(rc);
        if let Ok(content) = std::fs::read_to_string(&rc_path) {
            if content.contains(MARKER_START) {
                eprintln!("  Found old orchard shell function in ~/{rc}");
                if prompt_yn("  Remove it? [Y/n]", true) {
                    remove_old_shell_function(&rc_path)?;
                    eprintln!("  Removed old shell function from ~/{rc}");
                } else {
                    eprintln!(
                        "  Old shell function left in place — it may conflict with popup mode"
                    );
                }
            }
        }
    }
    Ok(())
}

/// Removes the marker block from the given RC file.
pub fn remove_old_shell_function(rc_path: &Path) -> Result<(), String> {
    let content = std::fs::read_to_string(rc_path)
        .map_err(|e| format!("reading {}: {e}", rc_path.display()))?;
    let cleaned = remove_marker_block(&content);
    std::fs::write(rc_path, cleaned)
        .map_err(|e| format!("writing {}: {e}", rc_path.display()))?;
    Ok(())
}

/// Strips all content between (and including) marker lines from the given string.
fn remove_marker_block(content: &str) -> String {
    let mut result = String::new();
    let mut inside = false;
    for line in content.lines() {
        if line.contains(MARKER_START) {
            inside = true;
            continue;
        }
        if line.contains(MARKER_END) {
            inside = false;
            continue;
        }
        if !inside {
            result.push_str(line);
            result.push('\n');
        }
    }
    result
}

/// Step 3: Install the wrapper script to ~/.local/bin/orchard-popup.
fn install_wrapper(home: &Path) -> Result<(), String> {
    eprintln!("Step 3: Creating wrapper script...");
    let bin_dir = home.join(".local/bin");
    std::fs::create_dir_all(&bin_dir)
        .map_err(|e| format!("creating ~/.local/bin: {e}"))?;

    let script_path = bin_dir.join("orchard-popup");
    std::fs::write(&script_path, get_wrapper_script())
        .map_err(|e| format!("writing {}: {e}", script_path.display()))?;

    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mut perms = std::fs::metadata(&script_path)
            .map_err(|e| format!("stat {}: {e}", script_path.display()))?
            .permissions();
        perms.set_mode(0o755);
        std::fs::set_permissions(&script_path, perms)
            .map_err(|e| format!("chmod {}: {e}", script_path.display()))?;
    }

    // Warn if ~/.local/bin is not in PATH.
    let path_var = std::env::var("PATH").unwrap_or_default();
    let bin_str = bin_dir.to_string_lossy();
    if !path_var.split(':').any(|p| p == bin_str.as_ref() || p == "~/.local/bin") {
        eprintln!("  Warning: ~/.local/bin is not in your PATH");
        eprintln!(r#"    Add: export PATH="$HOME/.local/bin:$PATH""#);
    }

    eprintln!("  Created ~/.local/bin/orchard-popup");
    Ok(())
}

/// Steps 4 & 5: Prompt for key, inject tmux binding (and optionally status bar) into tmux.conf.
/// Returns the key chosen by the user.
fn configure_tmux_binding_step(home: &Path, _tmux_version: &str) -> Result<String, String> {
    eprintln!("Step 4: Configuring tmux keybinding...");
    let tmux_conf = detect_tmux_conf(home);

    let existing = std::fs::read_to_string(&tmux_conf).unwrap_or_default();

    // Warn about existing bind-key o conflict (only if it's not our binding).
    if (existing.contains("bind-key o") || existing.contains("bind o "))
        && !existing.contains("orchard-popup")
    {
        eprintln!("  Warning: prefix + o is already bound in tmux.conf");
    }

    let key = prompt_key("  Bind popup to which key? [o]", "o");

    // Step 5: Optionally add status bar segment.
    eprintln!("Step 5: Status bar...");
    let add_status = prompt_yn("  Add orchard status to tmux status bar? [Y/n]", true);

    let state_dir = home.join(".local/state/orchard");
    let status_line = format!(
        "set -g status-right \"#(cat {}/status.txt) | %H:%M\"",
        state_dir.display()
    );

    let binding = get_tmux_binding(&key);
    let block_content = if add_status {
        format!("{binding}\n{status_line}")
    } else {
        binding
    };

    let updated = inject_config_block(&existing, &block_content);
    std::fs::write(&tmux_conf, &updated)
        .map_err(|e| format!("writing {}: {e}", tmux_conf.display()))?;

    eprintln!("  Added keybinding to {}", tmux_conf.display());
    if add_status {
        eprintln!("  Added status bar segment");
    }

    Ok(key)
}

/// Detects the tmux.conf path: prefers XDG location, falls back to ~/.tmux.conf.
fn detect_tmux_conf(home: &Path) -> std::path::PathBuf {
    let xdg = home.join(".config/tmux/tmux.conf");
    if xdg.exists() {
        return xdg;
    }
    home.join(".tmux.conf")
}

/// Step 6: Reload tmux config if inside a tmux session.
fn reload_tmux_config_step(home: &Path) {
    eprintln!("Step 6: Cleaning up and reloading...");
    let tmux_conf = detect_tmux_conf(home);

    if std::env::var("TMUX").is_ok() {
        // Remove the old session-closed hook that would unbind 'o' when
        // the legacy orchard session dies.
        let _ = std::process::Command::new("tmux")
            .args(["set-hook", "-gu", "session-closed[99]"])
            .status();

        // Kill any legacy *_orchard sessions so they don't interfere.
        if let Ok(output) = std::process::Command::new("tmux")
            .args(["list-sessions", "-F", "#{session_name}"])
            .output()
        {
            let sessions = String::from_utf8_lossy(&output.stdout);
            for session in sessions.lines() {
                if session.ends_with("_orchard") {
                    eprintln!("  Killing legacy session: {session}");
                    let _ = std::process::Command::new("tmux")
                        .args(["kill-session", "-t", session])
                        .status();
                }
            }
        }

        // Unbind the old 'o' key first — runtime bindings from the old
        // orchard binary take precedence over config-file bindings, so
        // source-file alone won't override them.
        let _ = std::process::Command::new("tmux")
            .args(["unbind-key", "o"])
            .status();

        // Reload the tmux config to activate the new popup binding.
        let result = std::process::Command::new("tmux")
            .args(["source-file", &tmux_conf.to_string_lossy()])
            .status();
        match result {
            Ok(s) if s.success() => {
                eprintln!("  ✓ Tmux config reloaded");
            }
            _ => {
                eprintln!("  Warning: could not reload tmux config");
                eprintln!("  Run: tmux source-file {}", tmux_conf.display());
            }
        }
    } else {
        eprintln!(
            "  Run `tmux source-file {}` to activate, or restart tmux",
            tmux_conf.display()
        );
    }
}

/// Prompts the user with a Y/n question and returns true for yes, false for no.
/// Loops until a valid response is given. Empty line returns `default_yes`.
pub fn prompt_yn(question: &str, default_yes: bool) -> bool {
    loop {
        eprint!("{question} ");
        std::io::stderr().flush().ok();

        let mut line = String::new();
        if std::io::stdin().read_line(&mut line).is_err() {
            return default_yes;
        }

        match line.trim().to_lowercase().as_str() {
            "" => return default_yes,
            "y" => return true,
            "n" => return false,
            _ => {
                eprintln!("  Please enter 'y' or 'n'");
            }
        }
    }
}

/// Prompts the user for a single alphanumeric key character.
/// Loops until a valid response is given. Empty line returns `default`.
pub fn prompt_key(question: &str, default: &str) -> String {
    loop {
        eprint!("{question} ");
        std::io::stderr().flush().ok();

        let mut line = String::new();
        if std::io::stdin().read_line(&mut line).is_err() {
            return default.to_string();
        }

        let trimmed = line.trim();
        if trimmed.is_empty() {
            return default.to_string();
        }

        let chars: Vec<char> = trimmed.chars().collect();
        if chars.len() == 1 && chars[0].is_ascii_alphanumeric() {
            return trimmed.to_string();
        }

        eprintln!("  Please enter a single alphanumeric character");
    }
}

/// Parses a tmux version string like "3.2", "3.2a", "next-3.5" into (major, minor).
/// Returns `None` for unparseable strings.
pub fn parse_tmux_version(s: &str) -> Option<(u32, u32)> {
    // Strip leading "next-" or similar alphabetic prefix.
    let s = s.trim();
    let s = if let Some(pos) = s.rfind(|c: char| c == '-') {
        &s[pos + 1..]
    } else {
        s
    };
    // Now s looks like "3.2" or "3.2a" or "3.4".
    let dot = s.find('.')?;
    let major: u32 = s[..dot].parse().ok()?;
    // Minor part may have trailing letters: "2a" → 2.
    let minor_str = &s[dot + 1..];
    let minor_end = minor_str
        .find(|c: char| !c.is_ascii_digit())
        .unwrap_or(minor_str.len());
    let minor: u32 = minor_str[..minor_end].parse().ok()?;
    Some((major, minor))
}

/// Injects the tmux binding block between markers into `existing`, replacing any existing block.
/// Safe to run multiple times (idempotent).
pub fn inject_config_block(existing: &str, content: &str) -> String {
    let new_block = format!("{MARKER_START}\n{content}\n{MARKER_END}");

    if let Some(start) = existing.find(MARKER_START) {
        if let Some(end_offset) = existing[start..].find(MARKER_END) {
            let end = start + end_offset + MARKER_END.len();
            return format!("{}{}{}", &existing[..start], new_block, &existing[end..]);
        }
    }

    if existing.is_empty() || existing.ends_with('\n') {
        format!("{existing}{new_block}\n")
    } else {
        format!("{existing}\n{new_block}\n")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wrapper_script_contains_git_orchard_call() {
        assert!(get_wrapper_script().contains("orchard"));
    }

    #[test]
    fn wrapper_script_captures_stdout_as_session() {
        assert!(get_wrapper_script().contains("session=$(orchard"));
    }

    #[test]
    fn wrapper_script_calls_switch_client() {
        assert!(get_wrapper_script().contains("tmux switch-client -t"));
    }

    #[test]
    fn wrapper_script_shows_error_on_failure() {
        assert!(get_wrapper_script().contains("tmux display-message"));
    }

    #[test]
    fn tmux_binding_uses_display_popup() {
        assert!(get_tmux_binding("o").contains("display-popup"));
    }

    #[test]
    fn tmux_binding_binds_key_o() {
        assert!(get_tmux_binding("o").contains("bind-key o"));
    }

    #[test]
    fn tmux_binding_references_orchard_popup_script() {
        assert!(get_tmux_binding("o").contains("orchard-popup"));
    }

    #[test]
    fn tmux_binding_custom_key_g() {
        let binding = get_tmux_binding("g");
        assert!(binding.contains("bind-key g"));
        assert!(!binding.contains("bind-key o"));
    }

    #[test]
    fn inject_into_empty_file() {
        let result = inject_config_block("", "bind-key o display-popup");
        assert!(result.contains(MARKER_START));
        assert!(result.contains(MARKER_END));
        assert!(result.contains("bind-key o display-popup"));
    }

    #[test]
    fn inject_appends_to_existing_content() {
        let result = inject_config_block("set -g status on\n", "bind-key o display-popup");
        assert!(result.starts_with("set -g status on\n"));
        assert!(result.contains(MARKER_START));
    }

    #[test]
    fn inject_replaces_existing_block() {
        let existing = format!(
            "before\n{}\nold binding\n{}\nafter\n",
            MARKER_START, MARKER_END
        );
        let result = inject_config_block(&existing, "new binding");
        assert!(result.contains("new binding"));
        assert!(!result.contains("old binding"));
        assert!(result.starts_with("before\n"));
        assert!(result.ends_with("after\n"));
    }

    #[test]
    fn inject_is_idempotent() {
        let first = inject_config_block("", "bind-key o display-popup");
        let second = inject_config_block(&first, "bind-key o display-popup");
        assert_eq!(first, second);
    }

    #[test]
    fn parse_version_plain() {
        assert_eq!(parse_tmux_version("3.2"), Some((3, 2)));
    }

    #[test]
    fn parse_version_with_letter_suffix() {
        assert_eq!(parse_tmux_version("3.2a"), Some((3, 2)));
    }

    #[test]
    fn parse_version_higher_minor() {
        assert_eq!(parse_tmux_version("3.4"), Some((3, 4)));
    }

    #[test]
    fn parse_version_lower_than_required() {
        assert_eq!(parse_tmux_version("3.0a"), Some((3, 0)));
    }

    #[test]
    fn parse_version_next_prefix() {
        // "next-3.5" → (3, 5)
        let v = parse_tmux_version("next-3.5").unwrap();
        assert!(v >= (3, 2));
    }

    #[test]
    fn parse_version_unparseable_returns_none() {
        assert_eq!(parse_tmux_version("garbage"), None);
    }

    #[test]
    fn parse_version_ordering() {
        // 3.2 >= 3.2
        assert!(parse_tmux_version("3.2").unwrap() >= (3, 2));
        // 3.4 >= 3.2
        assert!(parse_tmux_version("3.4").unwrap() >= (3, 2));
        // 3.0a NOT >= 3.2
        assert!(parse_tmux_version("3.0a").unwrap() < (3, 2));
        // 3.1c NOT >= 3.2
        assert!(parse_tmux_version("3.1c").unwrap() < (3, 2));
    }

    #[test]
    fn remove_old_shell_function_strips_marker_block() {
        let content = format!(
            "# before\n{MARKER_START}\norchard() {{ echo old; }}\n{MARKER_END}\n# after\n"
        );
        let result = remove_marker_block(&content);
        assert!(!result.contains("orchard()"));
        assert!(!result.contains(MARKER_START));
        assert!(!result.contains(MARKER_END));
        assert!(result.contains("# before"));
        assert!(result.contains("# after"));
    }

    #[test]
    fn remove_old_shell_function_leaves_content_without_markers_unchanged() {
        let content = "# just some config\nexport FOO=bar\n";
        let result = remove_marker_block(content);
        assert_eq!(result, content);
    }

    #[test]
    fn remove_old_shell_function_handles_multiple_blocks() {
        let content = format!(
            "{MARKER_START}\nfirst\n{MARKER_END}\nmiddle\n{MARKER_START}\nsecond\n{MARKER_END}\n"
        );
        let result = remove_marker_block(&content);
        assert!(!result.contains("first"));
        assert!(!result.contains("second"));
        assert!(result.contains("middle"));
    }
}
