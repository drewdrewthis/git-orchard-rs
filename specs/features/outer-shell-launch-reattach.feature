# orchardist#747 · PR #748 — outer tmux wrapper: boot, TMUX= clearing, reattach
Feature: orchard shell boot and reattach
  As an orchard user running `orchard shell`
  I want the outer wrapper to boot cleanly and reattach to a live session on rerun
  So that a second `orchard shell` never errors or double-boots the wrapper

  Background:
    Given the orchard daemon is running and reachable
    And an inner tmux server exists on socket "default"
    And no outer tmux server exists on socket "orchard-shell"

  @e2e
  Scenario: First run boots the outer session and attaches
    When I run "orchard shell"
    Then an outer tmux session named "shell" is created on socket "orchard-shell"
    And pane 0.0 runs the sidebar
    And pane 0.1 attaches to the inner server's most-recently-attached session
    And my terminal lands inside the outer session

  @integration
  Scenario: The inner attach clears TMUX before connecting
    When "orchard shell" sends the inner attach command to pane 0.1
    Then the command clears "$TMUX" before invoking "tmux attach"
    And the inner attach succeeds instead of refusing with "sessions should be nested with care"

  @e2e
  Scenario: Rerunning orchard shell reattaches instead of erroring
    Given an outer session "shell" already exists on socket "orchard-shell"
    When I run "orchard shell" again
    Then no second outer session is created
    And my terminal attaches to the existing outer session

  @integration
  Scenario: Reattach respawns a dead inner client
    Given an outer session "shell" exists
    And the inner tmux server has been restarted since the outer session booted
    And pane 0.1's inner client is dead
    When I run "orchard shell" again
    Then pane 0.1 is respawned with a fresh inner attach
    And I am not left attached to a dead pane

  @integration
  Scenario: Reattach respawns a dead sidebar pane
    Given an outer session "shell" exists with both panes present
    And pane 0.0's sidebar process has exited
    And remain-on-exit has kept pane 0.0 addressable instead of closing it
    When I run "orchard shell" again
    Then pane 0.0 is respawned with a fresh sidebar
    And pane 0.1 is also respawned
    And no pane is killed and no second outer session is created

  @integration
  Scenario: Reattach respawns when pane 0.1 itself (not just its inner client) is dead
    Given an outer session "shell" exists with both panes present
    And pane 0.1 itself has exited, not just its inner tmux client
    When I run "orchard shell" again
    Then pane 0.0 is respawned with a fresh sidebar
    And pane 0.1 is respawned with a fresh inner attach

  @integration
  Scenario: Reattach respawns when both panes are dead
    Given an outer session "shell" exists with both panes present
    And both pane 0.0 and pane 0.1 have exited
    When I run "orchard shell" again
    Then pane 0.0 is respawned with a fresh sidebar
    And pane 0.1 is respawned with a fresh inner attach

  @integration
  Scenario: Reattach rebuilds a collapsed one-pane window
    Given an outer session "shell" exists
    And the window has collapsed to a single surviving pane
    When I run "orchard shell" again
    Then the surviving pane is split to restore exactly two panes
    And pane 0.0 is respawned with a fresh sidebar
    And pane 0.1 is respawned with a fresh inner attach
    And my terminal lands back in the inner pane

  @integration
  Scenario: Reattach rebuilds a window with an extra third pane
    Given an outer session "shell" exists
    And the window has three panes instead of two
    When I run "orchard shell" again
    Then the extra panes are killed
    And the one kept pane is split to restore exactly two panes

  @integration
  Scenario: Reattach rebuilds when the outer session has no panes at all
    Given an outer session "shell" exists but its window reports no panes
    When I run "orchard shell" again
    Then the wrapper falls back to booting a fresh outer session as if none existed

  @integration
  Scenario: Requesting a missing inner session lists what exists
    Given the inner server has sessions "myrepo_main" and "myrepo_feature-x"
    When I run "orchard shell --session myrepo_nope"
    Then the command exits with status 2
    And the output lists "myrepo_main" and "myrepo_feature-x" as the available sessions
    And no outer session is created

  @integration
  Scenario: No inner server prints the orchard new hint
    Given no inner tmux server exists on socket "default"
    When I run "orchard shell"
    Then the command explains that no inner server was found
    And it suggests running "orchard new"
    And no outer session is created

  @integration
  Scenario: --detach boots the outer session without attaching
    When I run "orchard shell --detach"
    Then an outer session "shell" is created on socket "orchard-shell"
    And the command exits without attaching my terminal

  @unit
  Scenario: --width sets the sidebar pane's initial column count
    When I run "orchard shell --width 50"
    Then pane 0.0 is created at 50 columns
