# orchardist#776 — sidebar right-click "Pane → own session…" (break-pane into a new inner session)
Feature: break a pane out into its own session from the row menu
  As an orchard user with a long-running pane inside a session's window
  I want a right-click menu item that breaks that pane into its own session
  So that it gets its own card, its own M-<n> slot, and can be attached to independently

  Background:
    Given the outer shell sidebar is attached with the attached session showing on a card

  @unit
  Scenario: The item is offered on the attached session's multi-pane active window
    Given the attached session's active window has 2 or more panes
    When I right-click the attached session's card
    Then the menu lists "Pane → own session…"
    And "Rename" is also listed

  @unit
  Scenario: A single-pane active window hides the item and keeps Rename
    Given the attached session's active window has exactly one pane
    When I right-click the attached session's card
    Then the menu does not list "Pane → own session…"
    And "Rename" is the only rename-shaped item
    And no new session is created

  @unit
  Scenario: A non-attached session never offers the item
    Given a session that is not the one this client is attached to
    When I right-click its card
    Then the menu does not list "Pane → own session…"

  @unit
  Scenario: The name prompt reuses the rename field prefilled with the session name
    When I choose "Pane → own session…"
    Then the menu body shows a text field prefilled with the session name

  @unit
  Scenario: Escape on the prompt cancels with no tmux mutations
    Given I chose "Pane → own session…" and the name prompt is open
    When I press "Escape"
    Then the menu closes
    And no tmux command is run

  @integration
  Scenario: Confirm breaks the active pane out, promotes it, and switches to it
    Given the active pane of the attached session
    When I choose "Pane → own session…", enter a name, and confirm
    Then a new detached session is created
    And the active pane is broken into "<name>:" and the placeholder window "<name>:0" is killed
    And the new session's card is row 0 of the sidebar list
    And the inner client is switched to it via a scoped switch-client

  @integration
  Scenario: A colliding name is made unique the same way a launch is
    Given a live session already holds the entered name
    When I confirm the break-out
    Then the new session takes the uniqueName suffix (name, name-2, …)
    And every tmux step targets the resolved unique name

  @integration
  Scenario: A break-pane failure undoes the empty session and reports the step
    Given the "new-session" step succeeded but "break-pane" fails
    When the break-out is attempted
    Then the empty session created by "new-session" is killed
    And the status line shows "pane → session failed: break-pane: <stderr>"
    And no session is switched to and nothing is pinned
