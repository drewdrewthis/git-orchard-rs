Feature: Ensure a main tmux session always exists at the worktree origin
  As a developer using git-orchard
  I want a permanent tmux session at the worktree origin (main checkout)
  So that I always have a session for orchestration, triage, reviews, and non-feature work

  # Design decisions (informed by devil's advocate review):
  #
  # 1. Worktree origin = first entry from `git worktree list` (documented stable behavior).
  #    NOT "first non-bare" — git guarantees the main worktree is listed first.
  #
  # 2. Repo name is derived from the worktree origin path, NOT from `get_repo_name()`
  #    which uses `git rev-parse --show-toplevel` and returns different values per worktree.
  #    The canonical repo name comes from the origin worktree's directory name.
  #
  # 3. Session creation is a SEPARATE pipeline stage (`ensure_main_session`), called
  #    BEFORE `merge_tmux_sessions`. The merge function stays pure (no side effects).
  #    The newly created session is included in the session list passed to merge.
  #
  # 4. Session creation only happens in TUI mode, NOT in JSON mode. JSON mode is a
  #    read-only interface — creating tmux sessions as a side effect would surprise
  #    CI pipelines and scripts piping output to jq.
  #
  # 5. Dots in repo names are replaced with underscores in session names to avoid
  #    tmux target-session parsing issues (`.` is a window/pane separator in tmux).

  Background:
    Given a git repository with worktree origin at "/home/user/myrepo" on branch "main"
    And the canonical repo name is "myrepo" (derived from the origin worktree path)

  # -------------------------------------------------------------------------
  # Happy path: session auto-created on TUI startup
  # -------------------------------------------------------------------------

  @e2e
  Scenario: Creates main session on first TUI startup
    Given no tmux session named "myrepo_main" exists
    When the TUI starts up
    Then a tmux session named "myrepo_main" is created at "/home/user/myrepo"

  # -------------------------------------------------------------------------
  # JSON mode: read-only, no session creation
  # -------------------------------------------------------------------------

  @integration
  Scenario: JSON mode does not create a main session
    Given no tmux session named "myrepo_main" exists
    When git-orchard runs in JSON mode
    Then no tmux session is created
    And the JSON output includes the main worktree without a tmux_session

  # -------------------------------------------------------------------------
  # Idempotency: session already exists
  # -------------------------------------------------------------------------

  @unit
  Scenario: Skips session creation when main session already exists
    Given a session list containing "myrepo_main"
    When ensure_main_session checks whether to create
    Then it returns without creating a new session

  # -------------------------------------------------------------------------
  # Canonical repo name: derived from worktree origin path
  # -------------------------------------------------------------------------

  @unit
  Scenario: Derives repo name from worktree origin path regardless of invocation worktree
    Given the worktree origin is at "/home/user/my-project"
    And orchard is invoked from a feature worktree at "/home/user/my-project/.worktrees/issue42"
    When the canonical repo name is derived
    Then the repo name is "my-project"
    And the main session name is "my-project_main"

  @unit
  Scenario: Sanitizes dots in repo name to avoid tmux parsing issues
    Given the worktree origin is at "/home/user/my.repo-v2"
    When the main session name is derived
    Then the session name is "my_repo-v2_main"

  # -------------------------------------------------------------------------
  # Pipeline architecture: separate stage, before merge
  # -------------------------------------------------------------------------

  @integration
  Scenario: Creates main session in a separate stage before tmux merge
    Given no tmux session named "myrepo_main" exists
    When the ensure_main_session stage runs
    Then a tmux session "myrepo_main" is created at "/home/user/myrepo"
    And the session is included in the session list passed to merge_tmux_sessions

  # -------------------------------------------------------------------------
  # TUI visibility: merge maps session to worktree
  # -------------------------------------------------------------------------

  @unit
  Scenario: Merge maps main session to the origin worktree row
    Given a session list containing "myrepo_main" at "/home/user/myrepo"
    And a worktree at "/home/user/myrepo" on branch "main"
    When merge_tmux_sessions runs
    Then the main worktree row has tmux_session "myrepo_main"

  # -------------------------------------------------------------------------
  # Worktree origin identification
  # -------------------------------------------------------------------------

  @unit
  Scenario: Identifies worktree origin as first entry from git worktree list
    Given worktrees exist at:
      | path                               | branch         | is_bare |
      | /home/user/myrepo                  | main           | false   |
      | /home/user/myrepo/.worktrees/f     | feature/login  | false   |
    When the worktree origin is identified
    Then "/home/user/myrepo" is selected as the worktree origin

  @unit
  Scenario: Derives session name from non-main default branch
    Given the worktree origin is at "/home/user/myrepo" on branch "develop"
    When the main session name is derived
    Then the session name is "myrepo_develop"

  @unit
  Scenario: Uses HEAD as branch identifier when worktree origin is detached
    Given the worktree origin is at "/home/user/myrepo" in detached HEAD state
    When the main session name is derived
    Then the session name is "myrepo_HEAD"

  # -------------------------------------------------------------------------
  # Edge cases
  # -------------------------------------------------------------------------

  @integration
  Scenario: Creates main session when tmux has no existing sessions
    Given no tmux sessions exist at all
    When the ensure_main_session stage runs in TUI mode
    Then a tmux session named "myrepo_main" is created at "/home/user/myrepo"

  @unit
  Scenario: Preserves main session across refresh cycles
    Given a session list containing "myrepo_main" at "/home/user/myrepo"
    And a worktree at "/home/user/myrepo" on branch "main"
    When merge_tmux_sessions runs again after a refresh
    Then the main worktree row still has tmux_session "myrepo_main"

  @integration
  Scenario: Shows persistent error when tmux new-session fails
    Given no tmux session named "myrepo_main" exists
    And tmux new-session will fail with an error
    When the ensure_main_session stage runs
    Then a persistent red error is displayed with the failure message
    And the error remains visible until the user dismisses it

  @integration
  Scenario: TUI continues functioning after main session creation failure
    Given no tmux session named "myrepo_main" exists
    And tmux new-session will fail with an error
    When the ensure_main_session stage runs
    Then the main worktree appears in the list without a session indicator

  @unit
  Scenario: Skips creation when session name already exists at different path
    Given a session list containing "myrepo_main" at "/tmp/other-path"
    When ensure_main_session checks whether to create
    Then it returns without creating a new session
