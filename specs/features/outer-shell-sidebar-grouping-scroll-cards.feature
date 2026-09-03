# orchardist#747 · PR #748 — outer tmux wrapper: sidebar grouping, scroll, card shape
Feature: outer shell sidebar grouping, scrolling, and card shape
  As an orchard user scanning many sessions in the sidebar
  I want sessions grouped by what needs me, at a constant card height, with a stable scroll
  So that the list answers "what needs me?" at a glance and never jumps under me

  Background:
    Given the sidebar is showing sessions across three sections: "Needs attention", "Sessions", "Done"

  @unit
  Scenario: A blocked session sorts into Needs attention
    Given a session whose state is "input" or "stalled"
    When the sidebar groups rows
    Then the session appears under "Needs attention"

  @unit
  Scenario: An idle unattached session sorts into Done
    Given a session whose state is "idle", hooked, and has no attached client
    When the sidebar groups rows
    Then the session appears under "Done"

  @unit
  Scenario: An attached idle session does not sort into Done
    Given a session whose state is "idle" but a client is attached to it
    When the sidebar groups rows
    Then the session appears under "Sessions", not "Done"

  @unit
  Scenario: Rows within a section sort by most recent activity first
    Given three sessions in "Sessions" with different last-activity timestamps
    And one session with no activity timestamp
    When the sidebar renders the section
    Then rows are ordered most-recent-first
    And the row with no timestamp sorts last

  @unit
  Scenario: Every card renders at a fixed height
    Given sessions with varying amounts of mission text and metadata
    When the sidebar renders the list
    Then every card occupies exactly 4 lines
    And a card with no mission still renders a blank line in its place

  @unit
  Scenario: Below the minimum width, cards render compact
    Given the sidebar pane is narrower than its minimum readable width
    When the sidebar renders the list
    Then each card renders as 2 lines: name and a blank line

  @e2e
  Scenario: Scrolling with the wheel never changes the selection
    Given the list has more rows than fit in the viewport
    When I scroll the wheel over the list
    Then the viewport moves
    And the currently selected session is unchanged

  @unit
  Scenario: The viewport moves the minimum distance to reveal an off-screen selection
    Given the selection moves one card below the visible viewport via "j"
    When the sidebar re-renders
    Then the viewport scrolls down by exactly one card's height

  @unit
  Scenario: A click never moves the viewport
    Given a card is partially clipped at the bottom edge of the viewport but still clickable
    When I click that card
    Then the card is selected
    And the viewport does not move

  @unit
  Scenario: Selection is compared by session identity, not cursor index
    Given the cursor sits on row index 2
    And a background refresh re-sorts the list so a different session now occupies index 2
    When the sidebar re-renders
    Then the previously selected session remains selected
    And the viewport does not jump

  @unit
  Scenario: A background refresh never moves the viewport
    Given the user has scrolled partway down the list
    When rows arrive, disappear, or re-sort on the next poll
    Then the viewport stays anchored to the card that was on top before the refresh

  @unit
  Scenario: A new row arriving above the anchor is visible when scrolled to the top
    Given the viewport is scrolled to offset 0
    When a new "Needs attention" session and its section header arrive above the anchored card
    Then the new session and its header are visible without further scrolling

  @unit
  Scenario: The git footer box always renders 4 body rows
    Given the selected session has only a branch and a directory, no issue or PR
    When the sidebar renders the footer
    Then the footer box renders 4 body rows, padding unused rows blank

  @unit
  Scenario: The filter narrows the cards by any fact the card shows
    Given the sidebar is showing sessions whose names, missions, directories, branches and issue or PR refs differ
    When I press "/" and type a query
    Then only cards with that query as a case-insensitive substring of one of those facts are drawn
    And the header shows the query and the number of cards that survived it

  @unit
  Scenario: The filter never changes which session is attached
    Given a session is selected and attached
    When I open the filter and type a query
    Then no switch-client is run
    And the selection's identity is unchanged

  @unit
  Scenario: The rail falls back to the first visible card when the filter hides the selection
    Given the selected session does not match the query
    When the filter narrows the list
    Then the selection rail is drawn on the first visible card
    And the footer git box describes that same card
    And pressing Esc restores the full list with the original session selected

  @unit
  Scenario: A filter matching nothing says so
    Given a query no session matches
    When the sidebar renders the list
    Then an explicit no-match line is drawn instead of an empty band
    And the footer's fixed furniture still draws

  @unit
  Scenario: Enter keeps the filter and gives the keys back to the list
    Given the filter field has focus with a query typed
    When I press Enter
    Then the field loses focus and the query still narrows the list
    And "j" moves the selection to the second VISIBLE card

  @unit
  Scenario: A burst that opens the filter keeps the rest of itself
    Given a paste or a fast repeat arrives as one key message beginning with "/"
    When the sidebar handles it
    Then the runes after the slash are typed into the filter, not read as list commands

  @unit
  Scenario: The first nine visible cards carry jump ordinals
    Given the sidebar is showing more than nine cards
    When the sidebar renders the list
    Then the first nine carry the superscript markers ¹ to ⁹ in the one-cell selection gutter
    And the tenth card carries no marker
    And no card shifts to make room for a marker

  @unit
  Scenario: The selection rail covers the ordinal on the card it lands on
    Given the third visible card carries the ³ marker
    When that card becomes the selection
    Then the rail is drawn in that cell instead
    And no other cell of the list changes

  @e2e
  Scenario: The filter narrows the list under real keystrokes
    Given a wrapper with 30 synthetic rows on screen
    When I type "/" and then "payments" into the sidebar pane
    Then only the payments cards are drawn and the header shows the count
    When I press Esc
    Then the full list comes back
