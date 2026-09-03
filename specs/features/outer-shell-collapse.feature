# orchardist#747 · PR #748 — outer tmux wrapper: sidebar collapse/expand
Feature: outer shell sidebar collapse
  As an orchard user wanting more room for the inner pane
  I want to collapse the sidebar to a thin strip and expand it again
  So that the width choice persists across restarts and terminal resizes

  Background:
    Given an outer tmux session "shell" is attached with the sidebar expanded to 40 columns

  @e2e
  Scenario: Clicking the collapse button shrinks the sidebar to a strip
    When I click the "«" button in the sidebar header
    Then the sidebar pane resizes to 3 columns
    And the strip shows "»" and one state glyph per session

  @e2e
  Scenario: M-s is the keyboard equivalent of the collapse button
    When I press "M-s"
    Then the sidebar pane resizes to 3 columns
    When I press "M-s" again
    Then the sidebar pane resizes back to its remembered width

  @e2e
  Scenario: Clicking anywhere in the collapsed strip expands it
    Given the sidebar is collapsed to 3 columns
    When I click anywhere in the strip
    Then the sidebar pane resizes back to its remembered width

  @integration
  Scenario: Collapsing hands focus back to the inner pane
    Given the sidebar is expanded
    When I collapse it via the "«" button or "M-s"
    Then focus moves to the inner pane
    And my next keystroke goes to the shell

  @integration
  Scenario: Collapse state and pane width move together
    When I collapse the sidebar
    Then the outer window option "@sidebar_collapsed" is set to "1" before the pane is resized
    When I expand the sidebar
    Then "@sidebar_collapsed" is set to "0" before the pane is resized back

  @integration
  Scenario: A terminal resize re-pins a collapsed sidebar to 3 columns, not 40
    Given the sidebar is collapsed
    When my terminal is resized
    Then the outer resize hooks re-pin the sidebar to 3 columns
    And the sidebar does not pop back open to 40 columns

  @e2e
  Scenario: Collapsed state persists across restarts of orchard shell
    Given I collapsed the sidebar in a previous "orchard shell" session
    When I run "orchard shell" again
    Then the sidebar pane is created already collapsed, not resized after paint

  @e2e
  Scenario: A remembered width persists across restarts
    Given I previously widened the sidebar and it is not collapsed
    When I run "orchard shell" again
    Then the sidebar pane is created at the remembered width

  @unit
  Scenario: A collapsed width is never published as the shared sidebar width
    Given the sidebar is collapsed to 3 columns
    Then the shared "@orchard_sidebar_width" option is not overwritten with 3
    And other sessions' sidebars are unaffected by this collapse
