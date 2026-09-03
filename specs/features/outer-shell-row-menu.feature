# orchardist#747 · PR #748 — outer tmux wrapper: right-click row menu (rename/close)
Feature: outer shell right-click row menu
  As an orchard user wanting to rename or close a session without attaching to it
  I want a right-click menu on a sidebar card
  So that I can act on a session without the click also switching me into it

  Background:
    Given the outer shell sidebar is attached with multiple session rows

  @e2e
  Scenario: Right-clicking a card opens a Rename/Close menu
    When I right-click a session card
    Then a menu opens showing "Rename" and "Close"
    And it is drawn inside the sidebar pane, not as a native tmux menu

  @unit
  Scenario: Right-click does not select or hand back focus
    Given the sidebar is focused
    When I right-click a session card that is not currently selected
    Then the selection does not change
    And focus does not hand back to the inner pane
    And the sidebar keeps the keyboard

  @e2e
  Scenario: Rename swaps the menu body for a prefilled text field
    Given a session named "fake-01-payments"
    When I right-click it and choose "Rename"
    Then the menu body shows a text field prefilled with "fake-01-payments"
    When I edit the field and confirm
    Then the session is renamed

  @e2e
  Scenario: A successful rename carries selection and scroll state to the new name
    Given the renamed session was the currently selected row
    When the rename completes
    Then the same row stays selected under its new name
    And it does not vanish and reappear as a new card on the next poll

  @e2e
  Scenario: Close asks for confirmation before killing the session
    When I right-click a session and choose "Close"
    Then the menu body shows "Close <name>? y/N"
    When I press any key other than "y"
    Then the session is not closed and the menu closes

  @integration
  Scenario: Closing the session the wrapper's own client is on moves the client first
    Given the wrapper's outer client is currently attached to session "myrepo_main" via the inner pane
    And another session "myrepo_feature-x" exists
    When I right-click "myrepo_main" and confirm "Close"
    Then the client is switched to "myrepo_feature-x" before the kill
    And "myrepo_main" is killed

  @integration
  Scenario: Closing the client's own session with nowhere to move it is refused
    Given the wrapper's outer client is currently attached to session "myrepo_main" via the inner pane
    And no other real session exists
    When I right-click "myrepo_main" and confirm "Close"
    Then the close is refused
    And a message explains there is nowhere to move the client to first

  @integration
  Scenario: Closing any other session leaves the client where it is
    Given the wrapper's client is attached to session "myrepo_main"
    When I right-click a different session "myrepo_feature-x" and confirm "Close"
    Then the client is not moved
    And "myrepo_feature-x" is killed

  @unit
  Scenario: The menu holds its target by session name, not row index
    Given the menu is open on row index 2
    And a background refresh re-sorts the list so a different session now occupies index 2
    When I confirm the pending action
    Then the action targets the original session name, not whatever is now at index 2

  @unit
  Scenario: Escape or an outside click dismisses the menu without acting
    Given the menu is open
    When I press "Escape" or click outside the menu box
    Then the menu closes
    And no rename or close is performed

  @unit
  Scenario: A synthetic (fake) row declines both menu actions
    Given a synthetic row "fake-01-payments" used for scroll/grouping testing
    When I right-click it and choose "Rename" or "Close"
    Then the action is declined with a notice
    And no tmux session is targeted

  @integration
  Scenario: A failed rename or close keeps the menu open with tmux's own error
    Given a rename or close command fails
    When the action is attempted
    Then the menu stays open
    And the error tmux reported is shown on it
