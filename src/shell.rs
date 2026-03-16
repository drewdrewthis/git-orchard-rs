/// Returns the bash/zsh `orchard()` shell function as a string.
pub fn get_shell_function() -> String {
    r#"# git-orchard - git worktree manager
orchard() {
  case "$1" in
    init|upgrade|--json|--help|-h) git-orchard "$@"; return ;;
  esac
  for arg in "$@"; do
    case "$arg" in
      --json|--help|-h) git-orchard "$@"; return ;;
    esac
  done

  local session="orchard"
  local cmd='while true; do git-orchard "$@"; done'

  if ! tmux has-session -t "$session" 2>/dev/null; then
    local cheatsheet='#[fg=colour8]prefix: ctrl-b | o: orchard | (/): prev/next | %%: split-v | ": split-h | arrows: pane | z: zoom | x: close | d: detach'
    local status_left='#[fg=colour2,bold] orchard #[fg=colour248,nobold]'
    tmux new-session -d -s "$session" /bin/zsh -c "$cmd"
    tmux set-option -t "$session" status on
    tmux set-option -t "$session" status-style 'bg=colour235,fg=colour248'
    tmux set-option -t "$session" status-left-length 60
    tmux set-option -t "$session" status-right-length 120
    tmux set-option -t "$session" status-left "$status_left"
    tmux set-option -t "$session" status-right "$cheatsheet"

    # Save the current "o" keybinding before overwriting (only on first session creation)
    local _orchard_prev_bind
    _orchard_prev_bind=$(tmux list-keys 2>/dev/null | grep -E '\bbind-key\s+(-T\s+prefix\s+)?o\b' | head -1 || true)

    # Set up cleanup hook to restore/unbind when the orchard session is destroyed.
    # Uses array index [99] to avoid overwriting user hooks at lower indices (tmux 3.2+).
    if [ -n "$_orchard_prev_bind" ]; then
      local _orchard_restore_cmd
      _orchard_restore_cmd=$(echo "$_orchard_prev_bind" | sed 's/.*bind-key \(-T [^ ]* \)\{0,1\}o //')
      tmux set-hook -g session-closed[99] "if-shell '! tmux has-session -t orchard 2>/dev/null' 'bind-key o $_orchard_restore_cmd; set-hook -gu session-closed[99]'"
    else
      tmux set-hook -g session-closed[99] "if-shell '! tmux has-session -t orchard 2>/dev/null' 'unbind-key o; set-hook -gu session-closed[99]'"
    fi
  fi

  tmux bind-key o switch-client -t orchard

  if [ -n "$TMUX" ]; then
    tmux switch-client -t "$session"
  else
    tmux attach-session -t "$session"
  fi
}"#
    .to_string()
}

/// Returns instructions for adding the orchard shell function to the user's shell RC file.
/// Detects the shell from the `SHELL` environment variable.
pub fn get_init_instructions() -> String {
    let shell = std::env::var("SHELL").unwrap_or_else(|_| "/bin/zsh".to_string());
    let rc_file = if shell.contains("zsh") {
        "~/.zshrc"
    } else {
        "~/.bashrc"
    };

    format!(
        "Add this to your {rc_file}:\n\n{fn}\n\nThen reload your shell:\n  source {rc_file}\n\nThis creates an \"orchard\" command that launches git-orchard in a persistent tmux session.",
        rc_file = rc_file,
        fn = get_shell_function(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn shell_function_contains_orchard_function_definition() {
        let s = get_shell_function();
        assert!(s.contains("orchard()"));
    }

    #[test]
    fn shell_function_contains_tmux_session_name() {
        let s = get_shell_function();
        assert!(s.contains("session=\"orchard\""));
    }

    #[test]
    fn shell_function_handles_passthrough_flags() {
        let s = get_shell_function();
        assert!(s.contains("--json"));
        assert!(s.contains("--help"));
    }

    #[test]
    fn shell_function_contains_keybinding_hook() {
        let s = get_shell_function();
        assert!(s.contains("session-closed[99]"));
        assert!(s.contains("bind-key o"));
    }

    #[test]
    fn init_instructions_contains_source_command() {
        let s = get_init_instructions();
        assert!(s.contains("source"));
    }

    #[test]
    fn init_instructions_contains_rc_file() {
        // At minimum one of the supported rc files is mentioned.
        let s = get_init_instructions();
        assert!(s.contains(".zshrc") || s.contains(".bashrc"));
    }
}
