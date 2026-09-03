# orchardist#747 · PR #748 — outer tmux wrapper: the + new-session launch modal
Feature: outer shell new-session launch modal
  As an orchard user wanting to start a new session without leaving the wrapper
  I want a directory picker and launch form over the whole window
  So that I can launch a named session in a chosen directory without shelling out myself

  Background:
    Given the outer shell sidebar is attached
    And a session is currently selected

  @e2e
  Scenario: Clicking + opens the launch modal over the selected session's directory
    When I click the "+" button in the sidebar header
    Then a popup opens showing a directory picker
    And the picker starts in the selected session's working directory
    And focus hands back to the inner pane the moment the click lands

  @integration
  Scenario: A removed worktree directory fails the popup instead of a fallback
    Given the selected session's working directory no longer exists on disk
    When I click "+"
    Then the popup fails to open
    And no fallback directory is silently substituted

  @e2e
  Scenario: Typing filters the directory listing
    Given the picker is open on a directory with entries "cmd", "docs", "internal"
    When I type "doc"
    Then only "docs" remains in the filtered listing
    And the cursor parks on the first match, not on ".."

  @unit
  Scenario: Enter descends, backspace goes up or clears the filter
    Given the picker is open with an empty filter
    When I press Enter on a highlighted directory
    Then the picker descends into that directory
    Given the filter is non-empty
    When I press Backspace
    Then the last filter character is removed
    Given the filter is empty
    When I press Backspace
    Then the picker navigates up one directory

  @unit
  Scenario: The command and name fields prefill from the last launch
    Given the last successful launch ran command "claude" in a directory named "git-orchard-rs"
    When the modal opens for a new session
    Then the command field is prefilled with "claude"
    And the name field is prefilled with the new selected directory's name

  @unit
  Scenario: The launch command is passed as one argument, not split on spaces
    Given the command field contains "claude --resume x"
    When I launch
    Then tmux receives "claude --resume x" as a single command argument

  @unit
  Scenario: A duplicate session name is deduplicated automatically
    Given a session named "api" already exists
    When I launch a new session named "api"
    Then the new session is created as "api-2"

  @unit
  Scenario: Dots and colons in a session name are sanitised
    Given I type the name "v1.2"
    When I launch
    Then the session is created as "v1-2"

  @e2e
  Scenario: A successful launch switches this client to the new session
    When I fill in a valid directory, command, and name and click "Launch"
    Then a new tmux session is created with that command and directory
    And the outer wrapper's client switches to the new session
    And the last-launch state file is updated for the next "+" prefill

  @integration
  Scenario: A failed launch keeps the modal open with the error shown
    Given the launch command fails to create a session
    When I click "Launch"
    Then the modal remains open
    And the tmux error is shown on the modal
