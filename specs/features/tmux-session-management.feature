Feature: Tmux session management
  SUPERSEDED sections are marked with @superseded. These scenarios describe the
  old dedicated-session model and are replaced by popup-mode.feature.
  Active scenarios cover: session name derivation, session matching,
  remote proxy sessions, transfer, and stale proxy detection.

  As an orchard user
  I want seamless switching between worktree tmux sessions
  So that I can navigate my worktrees without losing context

  Background:
    Given a git repository named "myrepo" with worktrees for branches "main" and "feature/login"
    And the orchard binary is available as "git-orchard"

  # ===================================================================
  # Session name derivation (ACTIVE)
  # ===================================================================

  @unit
  Scenario: Session name for branch with slashes replaces slashes with dashes
    Given a repository named "myrepo"
    And a worktree with branch "feature/my-work"
    When the session name is derived
    Then the session name is "myrepo_feature-my-work"

  @unit
  Scenario: Session name falls back to directory name when branch is absent
    Given a repository named "myrepo"
    And a worktree at path "/home/user/my-worktree" with no branch
    When the session name is derived
    Then the session name is "myrepo_my-worktree"

  # ===================================================================
  # Session matching (ACTIVE)
  # ===================================================================

  @unit
  Scenario: Find session by exact path match
    Given tmux sessions exist with paths "/home/user/myrepo-feature-x"
    When finding a session for worktree at "/home/user/myrepo-feature-x"
    Then the session "myrepo_feature-x" is returned

  @unit
  Scenario: Find session by branch slug when path does not match
    Given a tmux session named "myrepo_feature-x"
    When finding a session for worktree with branch "feature/x" at an unmatched path
    Then the session "myrepo_feature-x" is returned

  @unit
  Scenario: Find session by directory name
    Given a tmux session named "orchard" at path "/home/user/orchard"
    When finding a session for worktree at "/different/path/orchard"
    Then the session "orchard" is returned

  @unit
  Scenario: No session found returns None
    Given tmux sessions with unrelated names and paths
    When finding a session for worktree at "/no/match" with branch "no-match-branch"
    Then no session is returned

  # ===================================================================
  # Local worktree session creation (ACTIVE — switching mechanism
  # changed in popup-mode.feature, but session creation logic remains)
  # ===================================================================

  @integration
  Scenario: Worktree session is created in the worktree directory
    Given a worktree at "/home/user/myrepo-feature-login" with branch "feature/login"
    When a session is created for this worktree
    Then the session is named "myrepo_feature-login"
    And the session's working directory is "/home/user/myrepo-feature-login"

  @integration
  Scenario: Existing session for worktree is reused
    Given a tmux session named "myrepo_feature-login" already exists
    When a session is requested for the "feature/login" worktree
    Then the existing session is reused
    And no new session is created

  # ===================================================================
  # Remote worktree session switching (ACTIVE)
  # ===================================================================

  @e2e
  Scenario: Remote worktree creates a local proxy session
    Given the orchard TUI is showing a remote worktree on host "devbox"
    And the cursor is on a remote worktree with branch "feature/api" on "devbox"
    When I press Enter
    Then a remote tmux session is created on "devbox" if it does not exist
    And a local tmux session named "remote_myrepo_feature-api" is created
    And the local session connects to "devbox" via SSH

  @integration
  Scenario: Remote session uses mosh when configured
    Given the orchard config has remote shell set to "mosh"
    And the cursor is on a remote worktree on host "devbox"
    When I press Enter on the remote worktree
    Then the local proxy session connects to "devbox" via mosh

  @integration
  Scenario: Dead remote proxy session is replaced on reconnect
    Given a local tmux session "remote_myrepo_feature-api" exists with a dead pane
    When I press Enter on the remote "feature/api" worktree
    Then the dead session is killed
    And a new local proxy session "remote_myrepo_feature-api" is created

  # ===================================================================
  # Stale remote proxy detection (ACTIVE)
  # ===================================================================

  @integration
  Scenario: Stuck mosh proxy session is detected and replaced
    Given a local proxy session "remote_myrepo_feature-api" exists
    And the proxy's mosh client shows "mosh: Last contact" in the pane
    When I press Enter on the remote "feature/api" worktree
    Then the stuck proxy session is killed
    And a new proxy session is created with a fresh connection

  @integration
  Scenario: Healthy remote proxy session is reused
    Given a local proxy session "remote_myrepo_feature-api" exists
    And the proxy pane is alive and not stuck
    When I press Enter on the remote "feature/api" worktree
    Then the existing proxy session is reused

  # ===================================================================
  # Transfer (ACTIVE)
  # ===================================================================

  @e2e
  Scenario: Pull to local does not auto-switch to the new session
    Given the orchard TUI is running with a remote worktree on host "devbox"
    When I pull a remote worktree to local
    Then the transfer completes
    And I remain in the orchard TUI
    And the worktree list refreshes to show the new local worktree
    And no tmux session is created for the new worktree

  @integration
  Scenario: Pull to local copies .env files from main checkout
    Given the main checkout at "/home/user/myrepo" has .env files
    When I pull a remote worktree to local at "/home/user/myrepo/.worktrees/feature-x"
    Then all top-level .env* files from the main checkout are copied to the new worktree
    And existing .env files in the destination are not overwritten

  # ===================================================================
  # SUPERSEDED — dedicated orchard session
  # (replaced by popup-mode.feature)
  # ===================================================================

  @superseded
  Scenario: Running orchard creates a repo-specific tmux session
    # REPLACED BY: popup-mode.feature "Running git-orchard directly launches the TUI"

  @superseded
  Scenario: Running orchard from inside tmux switches to the orchard session
    # REPLACED BY: popup-mode.feature "Running git-orchard via popup keybinding"

  @superseded
  Scenario: Orchard session already exists — reuses it
    # NO LONGER APPLICABLE: no dedicated orchard session

  @superseded
  Scenario: Session name is derived from repo name
    # NO LONGER APPLICABLE: no dedicated orchard session naming

  @superseded
  Scenario: Pressing ctrl-b o from a worktree session returns to orchard
    # REPLACED BY: popup-mode.feature keybinding opens popup

  @superseded
  Scenario: The "o" keybinding is refreshed every time the TUI starts
    # NO LONGER APPLICABLE: no runtime keybinding manipulation

  @superseded
  Scenario: The "o" keybinding is unbound when the orchard session is destroyed
    # NO LONGER APPLICABLE: no runtime keybinding manipulation

  @superseded
  Scenario: Pressing q from orchard TUI switches to previous tmux session
    # REPLACED BY: popup-mode.feature "Pressing q closes the popup"

  @superseded
  Scenario: Pressing Ctrl+C exits with code 130 to break restart loop
    # NO LONGER APPLICABLE: no restart loop

  @superseded
  Scenario: Orchard session has distinctive status bar styling
    # REPLACED BY: popup-mode.feature status bar integration

  @superseded
  Scenario: Shell function uses repo-specific session name
    # NO LONGER APPLICABLE: no shell function

  @superseded
  Scenario: Shell function registers keybinding cleanup hook
    # NO LONGER APPLICABLE: no shell function

  @superseded
  Scenario: Binary restarts after non-130 exit without crash-looping
    # NO LONGER APPLICABLE: no restart loop

  @superseded
  Scenario: Exit code 130 breaks the restart loop
    # NO LONGER APPLICABLE: no restart loop

  @superseded
  Scenario: git-orchard init installs shell function into RC file
    # REPLACED BY: popup-mode.feature init command

  @superseded
  Scenario: git-orchard init is idempotent
    # REPLACED BY: popup-mode.feature init --install idempotency
