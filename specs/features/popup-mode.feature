Feature: Tmux popup mode
  As an orchard user
  I want orchard to run as a tmux popup instead of a dedicated session
  So that session switching works reliably and the keybinding setup is simple

  Background:
    Given the user has tmux >= 3.2
    And the user has a wrapper script installed by "git-orchard init --install"

  # ===================================================================
  # Startup — no more dedicated session
  # ===================================================================

  @integration
  Scenario: Binary launch does not create a dedicated tmux session
    When I run "git-orchard" from inside tmux
    Then the TUI launches directly in the current terminal
    And no dedicated "_orchard" tmux session is created

  @e2e
  Scenario: Running git-orchard via popup keybinding
    Given I am in tmux session "api-server_main"
    When I press "prefix + o"
    Then a tmux popup appears with the orchard TUI
    And I can see the task/worktree list

  @e2e
  Scenario: Running git-orchard outside tmux
    When I run "git-orchard" outside of tmux
    Then the TUI launches in the current terminal
    And session switching is disabled (no tmux client)
    And a hint shows "run inside tmux for full functionality"

  @e2e
  Scenario: Running git-orchard in a directory with no git repo
    Given I am in ~/Downloads (not a git repo)
    When I press "prefix + o"
    Then the popup shows an error: "not inside a git repository"
    And pressing q closes the popup

  # ===================================================================
  # Session switching — popup closes, then switch
  # ===================================================================

  @e2e
  Scenario: Enter on a task switches to its session and closes the popup
    Given the popup TUI is showing tasks
    And task #47 has session "git-orchard-rs_47_main"
    When I press Enter on task #47
    Then the popup closes
    And my tmux client switches to session "git-orchard-rs_47_main"

  @e2e
  Scenario: Enter creates local worktree and session, then switches
    Given task #52 has status "ready" with no worktree or session
    When I press Enter on task #52
    Then the TUI shows a progress indicator "creating worktree..."
    And a git worktree is created
    And a tmux session is created in the worktree
    And the popup closes
    And my tmux client switches to the new session

  @e2e
  Scenario: Enter creates remote worktree session, then switches
    Given task #35 has a remote worktree on host "devbox"
    And no remote tmux session exists for the worktree
    When I press Enter on task #35
    Then the TUI shows staged progress:
      | step                                    |
      | creating remote session on devbox...    |
      | creating local proxy session...         |
      | connecting...                           |
    And a remote tmux session is created on devbox via SSH
    And a local proxy session is created (ssh -tt or mosh)
    And the popup closes
    And my tmux client switches to the local proxy session

  @e2e
  Scenario: Pressing q closes the popup
    Given the popup TUI is showing
    When I press "q"
    Then the popup closes
    And I am back in my original tmux session
    And no session switch occurs

  @e2e
  Scenario: Pressing Escape closes the popup
    Given the popup TUI is showing
    When I press "Escape"
    Then the popup closes

  # ===================================================================
  # Error handling — errors stay visible in the TUI
  # ===================================================================

  @e2e
  Scenario: Worktree creation fails — error shown in TUI
    Given task #52 has status "ready" with no worktree
    And there is an index.lock file preventing worktree creation
    When I press Enter on task #52
    Then the TUI shows a progress indicator "creating worktree..."
    And when creation fails, the TUI shows the error message in a dialog
    And the popup does NOT close
    And the user can press q to dismiss the error and return to the task list
    And an event "error" is logged to events.jsonl

  @integration
  Scenario: Remote session creation fails — error shown in TUI
    Given task #35 has a remote worktree on host "devbox"
    And SSH to devbox times out
    When I press Enter on task #35
    Then the TUI shows a progress indicator "connecting to devbox..."
    And when SSH fails, the TUI shows: "SSH connection timed out to devbox"
    And the popup does NOT close
    And the user can retry with Enter or dismiss with q

  @integration
  Scenario: User dismisses popup during creation — partial state cleaned up
    Given task #52 is in the process of creating a worktree
    And the TUI shows "creating worktree..."
    When the user presses Escape (or tmux kills the popup)
    Then the process receives SIGTERM
    And any partially-created worktree is cleaned up on next startup
    And the state file does NOT record the worktree as created

  @integration
  Scenario: Startup reconciliation cleans up partial state
    Given the state file records task #52 has worktree "/path/to/wt"
    But the worktree does not exist on disk
    When git-orchard starts
    Then the worktree path is cleared from the task in state.json
    And an event "worktree.orphaned_ref" is logged

  # ===================================================================
  # Switch mechanism — wrapper script
  # ===================================================================

  @integration
  Scenario: Binary exits with switch target on stdout
    Given the user selected task #47 with session "git-orchard-rs_47_main"
    When the TUI exits after selection
    Then it prints "git-orchard-rs_47_main" to stdout
    And exits with code 0

  @integration
  Scenario: Binary exits with empty stdout on quit
    When the user presses q to close the popup
    Then stdout is empty
    And the exit code is 0
    And no switch-client runs

  @integration
  Scenario: Binary exits with non-zero code on fatal error
    When the binary fails to start (e.g., can't read state file)
    Then it exits with code 1
    And stderr contains the error message
    And the wrapper script shows the error via tmux display-message

  @integration
  Scenario: Wrapper script handles the switch and errors
    Given git-orchard init --install created the wrapper script:
      """bash
      #!/bin/sh
      errfile=$(mktemp /tmp/orchard-err.XXXXXX)
      session=$(git-orchard 2>"$errfile")
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
      fi
      """
    And the tmux keybinding is:
      """
      bind-key o display-popup -E -w 90% -h 80% "$HOME/.local/bin/orchard-popup"
      """
    When the popup closes after selecting a task
    Then the wrapper runs switch-client with the session name
    And on error, tmux display-message shows the error

  # ===================================================================
  # Init command
  # ===================================================================

  @integration
  Scenario: git-orchard init checks tmux version
    Given tmux version is 3.0a
    When I run "git-orchard init"
    Then it warns: "tmux >= 3.2 required for popup mode (you have 3.0a)"
    And it suggests upgrading tmux

  @unit
  Scenario: Tmux version parsing handles letter suffixes
    Then version "3.2" is >= 3.2
    And version "3.2a" is >= 3.2
    And version "3.4" is >= 3.2
    And version "3.0a" is NOT >= 3.2
    And version "3.1c" is NOT >= 3.2
    And version "next-3.5" is >= 3.2
    And unparseable versions produce a warning, not a crash

  @integration
  Scenario: git-orchard init prints setup instructions
    Given tmux version is 3.4
    When I run "git-orchard init"
    Then it prints the wrapper script content
    And it prints the tmux.conf keybinding using full path: $HOME/.local/bin/orchard-popup
    And it does NOT modify any files automatically

  @integration
  Scenario: git-orchard init --install writes wrapper script and tmux binding
    When I run "git-orchard init --install"
    Then it creates ~/.local/bin/orchard-popup with the wrapper script
    And it makes the script executable
    And it appends the tmux binding to the detected tmux.conf between markers
    And existing tmux.conf content is preserved
    And the operation is idempotent (safe to run again)

  @integration
  Scenario: init --install detects XDG tmux.conf location
    Given ~/.config/tmux/tmux.conf exists
    And ~/.tmux.conf does not exist
    When I run "git-orchard init --install"
    Then it appends the binding to ~/.config/tmux/tmux.conf

  @integration
  Scenario: init --install warns if ~/.local/bin is not in PATH
    Given ~/.local/bin is not in the current PATH
    When I run "git-orchard init --install"
    Then it warns: "~/.local/bin is not in your PATH — add it or the keybinding won't work"
    And the wrapper script is still created (user may fix PATH separately)

  @integration
  Scenario: init --install warns about existing keybinding conflict
    Given ~/.tmux.conf contains "bind-key o" for a different command
    When I run "git-orchard init --install"
    Then it warns: "prefix + o is already bound — orchard will override it"
    And proceeds with installation (user can change the key)

  @integration
  Scenario: init detects old shell function and suggests removal
    Given the user's ~/.zshrc contains the orchard shell function between markers
    When they run "git-orchard init"
    Then it suggests removing the old shell function
    And shows how to clean up: "Remove the block between '# >>> git-orchard >>>' markers in ~/.zshrc"

  # ===================================================================
  # Adaptive header
  # ===================================================================

  @integration
  Scenario: Full logo shown when terminal is tall enough
    Given the terminal height is >= 30 rows
    When the TUI renders
    Then the full ASCII art logo is displayed

  @integration
  Scenario: Compact header when terminal is short
    Given the terminal height is < 30 rows
    When the TUI renders
    Then a compact one-line header is shown: "🌳 Git Orchard" styled in green
    And no ASCII art is displayed
    And the task list gets the recovered vertical space

  @integration
  Scenario: Header adapts on terminal resize
    Given the TUI is showing the full logo
    When the terminal is resized to < 30 rows
    Then the header switches to the compact one-liner
    When the terminal is resized back to >= 30 rows
    Then the full logo returns

  # ===================================================================
  # Tmux status bar integration
  # ===================================================================

  # NOTE: status.txt is written during TUI refresh. It goes stale when the
  # popup is not open. This is a known limitation — the status bar shows
  # "last known state" not "live state." A future background daemon could
  # keep it fresh, but that is out of scope for this feature.

  @integration
  Scenario: Status file is written on every refresh
    When a collector refresh completes inside the TUI
    Then ~/.local/state/git-orchard/status.txt is written
    And it contains tmux-formatted status text
    And it includes a timestamp comment on the first line

  @integration
  Scenario: Status bar shows active work summary
    Given 3 tasks are in_progress, 2 have active claude sessions, 1 has failing CI
    When the status file is written
    Then it contains: "#[fg=green]🌳 ORCHARD#[fg=default]: 3 active · ⚡ 2 claude · ✗ 1 failing"

  @integration
  Scenario: Status bar shows idle state
    Given no tasks are in_progress
    When the status file is written
    Then it contains: "#[fg=green]🌳 ORCHARD#[fg=default]"

  @integration
  Scenario: Status bar shows recent error
    Given the last event in events.jsonl is an error
    And it occurred less than 5 minutes ago
    When the status file is written
    Then it contains: "#[fg=green]🌳 ORCHARD#[fg=red] ✗ <error summary>#[fg=default]"

  @integration
  Scenario: Error clears from status bar after 5 minutes
    Given the last error occurred more than 5 minutes ago
    When the status file is written
    Then the error is no longer shown
    And the status bar shows normal active work summary

  @integration
  Scenario: Stale status file shows last-known state
    Given the popup was last open 30 minutes ago
    And status.txt was written at that time
    Then the tmux status bar shows the 30-minute-old data
    And opening the popup triggers a fresh refresh and status.txt update

  @integration
  Scenario: User adds status bar segment to tmux.conf
    Given git-orchard init suggests the line:
      """
      set -g status-right "#(cat ~/.local/state/git-orchard/status.txt) | %H:%M"
      """
    When the user adds it to their tmux.conf
    Then the orchard status appears in their tmux status bar in green
    And it updates whenever tmux refreshes status (default every 15 seconds)

  # ===================================================================
  # Cache for fast startup
  # ===================================================================

  @integration
  Scenario: TUI renders from cached state immediately
    Given ~/.local/state/git-orchard/state.json has cached task data
    And the cache is less than 5 minutes old
    When the popup opens
    Then the task list renders from cache in < 100ms
    And a background refresh starts to enrich with live data
    And a subtle "refreshing..." indicator is shown

  @integration
  Scenario: Stale cache is shown with indicator
    Given the cache is more than 5 minutes old
    When the popup opens
    Then the task list renders from cache immediately
    And a "stale — refreshing..." indicator is shown
    And the background refresh runs

  @integration
  Scenario: No cache on first run
    Given no state.json exists
    When the popup opens
    Then a loading spinner is shown
    And data is fetched fresh from git, tmux, and GitHub

  # ===================================================================
  # Removed: things that go away
  # (These are verified by the "Binary launch does not create a
  # dedicated tmux session" integration scenario above, plus
  # the init command scenarios. Listed here for documentation.)
  # ===================================================================

  # - No dedicated *_orchard tmux session
  # - No restart loop (exit code 130 has no special meaning)
  # - No runtime tmux bind-key/unbind-key/set-hook calls
  # - No orchard() shell function in .zshrc/.bashrc

  # ===================================================================
  # Backward compatibility
  # ===================================================================

  @integration
  Scenario: Old shell function still works during transition
    Given the user still has the orchard() shell function installed
    When they run "orchard"
    Then the TUI launches (the function just calls git-orchard)
    And a deprecation notice suggests switching to the popup binding

  # ===================================================================
  # Non-tmux usage
  # ===================================================================

  @e2e
  Scenario: Works as a standalone TUI without tmux
    When I run "git-orchard" outside tmux
    Then the TUI shows the task list
    And session switching is unavailable (greyed out or hint shown)
    And all other features work (viewing tasks, PR status, etc.)
    And q exits the process
    And stdout is empty (no session to switch to)

  @e2e
  Scenario: Works inside tmux without the popup
    When I run "git-orchard" directly in a tmux pane (not via popup)
    Then the TUI works normally
    And Enter prints session name to stdout and exits
    And the user can manually run: tmux switch-client -t <session>
    And q exits the process

  # ===================================================================
  # Visual ownership boundaries (documentation, not testable scenarios)
  # ===================================================================

  # Orchard's UI surfaces:
  # - Popup TUI (logo, task list, dialogs) — the primary interface
  # - Tmux status bar segment via status.txt — ambient dashboard
  # - Tmux display-message — transient error/event notifications
  # Worktree tmux sessions are plain tmux — Orchard does not style them.
