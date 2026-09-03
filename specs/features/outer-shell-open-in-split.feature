# orchardist#777 — outer shell: open a session in a split (two inner clients side by side)
Feature: outer shell open session in split
  As an orchard user comparing two sessions
  I want to open a second session in a split beside the first
  So that both are visible at once while the sidebar keeps driving the one I focused last

  Background:
    Given an outer tmux session "shell" is attached with pane 0.0 (sidebar) and pane 0.1 (inner client)
    And the sidebar pane is 40 columns wide

  @e2e
  Scenario: M-Enter opens the selected session in a new work pane
    Given the selected card is a session with no client attached
    When I press "M-Enter"
    Then a new outer pane opens to the right of "shell:0.1"
    And it runs an inner "tmux -L default attach" client on the selected session

  @unit
  Scenario: The split command is the outer split-window plus the inner attach
    When the sidebar opens session "beta" in a split beside pane "%1"
    Then it runs outer "split-window -h -t %1 ... TMUX= tmux -L default attach -t beta"
    And it prints the new pane's id and tty so the sidebar can track it

  @integration
  Scenario: The sidebar width is unchanged after the split
    Given the sidebar pane is 40 columns wide
    When I open a session in a split
    Then "main-vertical" is applied so the sidebar stays 40 columns
    And the two work panes stack in the right column, sharing its height

  @e2e
  Scenario: The attached bar follows the last-focused work pane without a click
    # Focus is inferred from tmux client_activity (most-recent activity, not true
    # focus — tmux has no per-client focus signal).
    Given two work panes each attached to a different session
    When I move focus from one work pane to the other
    Then the attached-bar indicator moves to the newly focused pane's session
    And no card had to be clicked for it to move

  @e2e
  Scenario: Clicking a card switches only the last-focused work pane's client
    Given two work panes and the right-hand one was focused last
    When I click a different card in the sidebar
    Then the client in the last-focused work pane switches to that session
    And the other work pane's client is untouched

  @unit
  Scenario: M-w closes the split and restores the two-pane layout
    Given a split is open
    When I press "M-w"
    Then the split pane's inner client is detached and its outer pane closes
    And "main-vertical" is re-applied so the sidebar keeps its width

  @unit
  Scenario: M-w on the sole work pane is refused with a status message
    Given no split is open
    When I press "M-w"
    Then nothing is detached
    And the sidebar shows "sole work pane — nothing to detach"

  @unit
  Scenario: Two clients on the same session is refused
    # Decision (#777): refuse a second client on an already-attached session — tmux would mirror it into both panes.
    Given the selected card is a session that already has a client attached
    When I try to open it in a split
    Then no split pane is opened
    And the sidebar shows that the session is already attached

  @unit
  Scenario: A synthetic scroll-test row cannot be opened in a split
    Given the selected card is a synthetic "fake-" row
    When I press "M-Enter"
    Then no split pane is opened
    And the sidebar shows there is nothing to open

  @future
  Scenario: Open in split is reachable from the right-click menu
    # The reusable openInSplit entry point is shared; the menu item itself lands
    # with #776 (menu.go / menuops.go / menuview.go), which this issue does not touch.
    Given the right-click row menu offers "Open in split"
    When I choose it on a card
    Then the same split opens as pressing M-Enter on that card

  # FOLLOW-UP (#777, soft dependency on #775): dragging a card onto the work
  # area to open it in a split reuses the pinned-sessions drag infra and is out
  # of scope here — it lands as a separate change.
