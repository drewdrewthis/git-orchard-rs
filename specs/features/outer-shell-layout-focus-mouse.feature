# orchardist#747 · PR #748 — outer tmux wrapper: layout pinning, focus hand-back, mouse
Feature: outer shell layout, focus hand-back, and mouse forwarding
  As an orchard user with the outer shell attached
  I want the sidebar pinned at a fixed width with clean keyboard/mouse routing
  So that switching sessions never leaves my keystrokes stuck on the sidebar

  Background:
    Given an outer tmux session "shell" is attached with pane 0.0 (sidebar) and pane 0.1 (inner client)
    And the outer server has no prefix key bound

  @e2e
  Scenario: Sidebar width survives an inner session churning windows and panes
    Given the sidebar pane is 40 columns wide
    When the inner session splits, kills, and zooms panes repeatedly
    Then the sidebar pane remains 40 columns wide throughout

  @e2e
  Scenario: Sidebar width survives an outer terminal resize
    Given the sidebar pane is 40 columns wide and not collapsed
    When my terminal is resized (SIGWINCH)
    Then the outer "client-resized" hook re-pins the sidebar to 40 columns

  @integration
  Scenario: A scripted resize with no attached client is also corrected
    Given the sidebar pane is 40 columns wide and not collapsed
    And no client is currently attached to the outer session
    When "resize-window" is run against the outer session
    Then the outer "window-resized" hook re-pins the sidebar to 40 columns

  @e2e
  Scenario: The outer layer has no prefix and swallows no keystrokes
    When I press Ctrl-A inside the inner pane's shell
    Then readline receives Ctrl-A as start-of-line
    And no outer key table consumes the keystroke first

  @e2e
  Scenario: Selecting a row hands keyboard focus to the inner pane
    Given the sidebar has multiple session rows
    When I click a row in the sidebar
    Then the inner server switches its active session to that row
    And outer pane 0.1 becomes the outer session's active pane
    And my next keystroke goes to the shell, not the sidebar

  @unit
  Scenario: Cursor movement alone does not hand back focus
    Given the sidebar is focused
    When I press "j" or "k" to move the cursor without selecting
    Then the inner session does not switch
    And focus does not hand back to pane 0.1

  @integration
  Scenario: A failed switch never hands back focus
    Given selecting a row triggers an inner switch-client that fails
    When I select that row
    Then focus remains on the sidebar pane
    And the failure is logged, not surfaced as a fatal error

  @e2e
  Scenario: M-Left and M-Right move focus between panes from the keyboard
    Given focus is on the inner pane
    When I press "M-Left"
    Then focus moves to the sidebar pane (0.0)
    When I press "M-Right"
    Then focus moves to the inner pane (0.1)

  @e2e
  Scenario: A mouse click focuses and forwards to the pane under the cursor
    Given the sidebar is focused
    When I click inside the inner pane
    Then the inner pane becomes focused
    And the click is forwarded into the inner pane's own program

  @e2e
  Scenario: The mouse wheel scrolls the sidebar list without selecting
    Given the sidebar list has more rows than fit on screen
    When I scroll the wheel over the sidebar
    Then the list viewport moves
    And the selected session does not change

  @integration
  Scenario: Wheel scroll over the inner pane is forwarded, not swallowed
    Given focus is anywhere in the outer session
    When I scroll the wheel over the inner pane
    Then the raw scroll event is forwarded into the inner pane
    And it lands in the inner session's own copy-mode/scrollback, not the outer server's

  @e2e
  Scenario: A popup from the sidebar renders over the inner pane
    Given the sidebar's launch modal is open as a display-popup
    Then the popup is composited over the whole outer window
    And the inner pane's content underneath is not disturbed by the popup closing

  @unit
  Scenario: M-1 to M-9 select the nth visible card
    Given the sidebar has more than three cards
    When I press "M-3"
    Then the third VISIBLE card is selected
    And focus is handed back to pane 0.1, as a click would

  @unit
  Scenario: A jump chord past the end of the list does nothing
    Given only four cards are visible
    When I press "M-9"
    Then no session is switched and the selection is unchanged

  @unit
  Scenario: A jump chord counts filtered cards
    Given a filter is narrowing the list
    When I press "M-2"
    Then the second card of the FILTERED list is selected, not the second row of the model

  @unit
  Scenario: Every other Alt chord stays the wrapper's
    Given the sidebar has focus
    When I press "M-s", "M-p", "M-d" or "M-0"
    Then the sidebar selection does not move and no session is switched

  @e2e
  Scenario: M-3 drives the jump from a real terminal through the outer binding
    Given a real client is attached to the outer session
    When that terminal writes "M-3"
    Then the outer root key table forwards it to pane 0.0
    And the sidebar's rail lands on the card that was carrying the ³ marker
    And outer pane 0.1 is still the active pane

  @unit
  Scenario: The Needs-attention badge counts the bucket, not the visible cards
    Given three sessions are in "Needs attention" and a filter hides two of them
    When the sidebar renders the header
    Then the badge still reads 3

  @unit
  Scenario: No badge is drawn when nothing needs attention
    Given no session is in "Needs attention"
    When the sidebar renders the header
    Then no badge is drawn, not a badge reading 0

  @unit
  Scenario: The collapsed strip carries the count under its expand button
    Given the sidebar is collapsed to the 3-column strip
    And two sessions need attention
    Then the count is drawn on the line under the "»" button

  @unit
  Scenario: The bell rings once for a session ENTERING Needs attention
    Given the bell is enabled and the sidebar has already seen one list
    When a session that was not in "Needs attention" enters it
    Then exactly one terminal bell is emitted
    When the count then falls, or holds, or a synthetic row enters the bucket
    Then no bell is emitted

  @unit
  Scenario: The bell is silent on startup and across a lane failure
    Given sessions are already in "Needs attention" when the sidebar starts
    Then no bell is emitted for that first list
    When every row disappears on a failed poll and the same sessions come back
    Then no bell is emitted for their return

  @unit
  Scenario: The bell setting is remembered
    Given the bell is off
    When I press "b"
    Then the bell turns on and is written to the sidebar state file alongside the width and collapse state
    And a state file written before the bell existed loads with the bell off
