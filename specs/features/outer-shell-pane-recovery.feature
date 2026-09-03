# orchardist#796 — outer shell pane recovery
Feature: outer shell pane recovery
  As an orchard user running `orchard shell`
  I want a pane that dies for any reason to self-heal instead of sitting there dead
  So that killing an inner session, restarting the inner server, or a sidebar crash never strands me

  Background:
    Given the orchard daemon is running and reachable
    And an outer session "shell" exists with both panes present, on a throwaway tmux socket pair
    And detach-on-destroy is set to off on the inner server

  @integration
  Scenario: Detach-on-destroy off prevents the common case
    Given pane 0.1's inner client is attached to the inner session I am currently viewing
    When that inner session is killed
    Then my client is switched to another inner session instead of being detached
    And pane 0.1 stays alive, still running its inner attach process

  @integration
  Scenario: Inner session killed while detach-on-destroy could not save it reattaches in the same pane
    Given pane 0.1's inner attach process has exited because its session was killed
    And the inner server still has at least one other session
    When orchard-shell's pane-died hook fires for pane 0.1
    Then pane 0.1 is reattached to the inner server's most-recently-attached session within about 1 second
    And the pane layout is unchanged
    And the pane shows a status line explaining "inner tmux exited (status 0) — reattached"

  @integration
  Scenario: After reattach, sidebar clicks resolve the client at click time (relies on #787)
    Given pane 0.1 has been reattached to the inner server, giving that pane a new pty
    When I click a card in the sidebar
    Then the sidebar resolves the inner client at click time (#787) rather than from a tty captured when it launched
    And "sidebar.log" shows no "can't find client" error

  @integration
  Scenario: Inner server killed creates a new session instead of leaving a dead pane
    Given the inner tmux server itself has exited, so pane 0.1's attach process has exited too
    And the inner server currently has no sessions
    When orchard-shell's pane-died hook fires for pane 0.1
    Then a new inner session is created via "new-session -A"
    And pane 0.1 attaches to it within about 1 second
    And no user action was required

  @integration
  Scenario: Sidebar crash is respawned and logged
    Given the sidebar process in pane 0.0 crashes
    When orchard-shell's pane-died hook fires for pane 0.0
    Then pane 0.0 is respawned with fresh env within about 1 second
    And the exit status and reason are appended to "sidebar.log"

  @integration
  Scenario: A sidebar crash loop is bounded and recoverable with M-r
    Given the sidebar has crashed and been respawned 6 times within the last 60 seconds
    When orchard-shell's pane-died hook fires for pane 0.0 again
    Then no further automatic respawn is attempted
    And pane 0.0 shows "sidebar keeps crashing — see sidebar.log; press M-r to retry"
    When I press "M-r"
    Then pane 0.0 is respawned on demand

  @integration
  Scenario: An inner-pane crash loop is bounded and recoverable with M-r
    Given pane 0.1's inner client has exited and been reattached 6 times within the last 60 seconds
    When orchard-shell's pane-died hook fires for pane 0.1 again
    Then no further automatic reattach is attempted
    And pane 0.1 shows "inner tmux keeps exiting — press M-r to retry"
    When I press "M-r"
    Then pane 0.1 is reattached on demand

  @integration
  Scenario: Doctor reports dead panes and the last recovery event
    Given pane 0.0 is currently dead
    And the most recent recovery event was "sidebar exited (status 1) — respawned"
    When I run "orchard shell doctor"
    Then the report lists pane 0.0 as dead
    And the report shows the last recovery event "sidebar exited (status 1) — respawned"

  @unit
  Scenario: The two outer.conf copies stay byte-identical after adding recovery hooks
    Given "cmd/orchard-shell/outer.conf" now contains a pane-died hook and an M-r bind
    Then "scripts/outer-shell/outer.conf" is byte-identical to it
