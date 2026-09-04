# orchardist#801 — outer tmux wrapper: filter query survives the dev-build ident
Feature: sidebar filter header yields to the dev build ident
  As an orchard user filtering the sidebar on a dev build
  I want my typed query to stay fully visible in the header
  So that a wide "dev@<rev>" label never scrolls the query out from under the cursor

  Background:
    Given the running binary is a dev build with ident "dev@abc1234*"

  @unit
  Scenario: A narrow pane drops the ident so the query field keeps its width
    Given the sidebar pane is at the default width of 40 columns
    And the filter is open with the query "payments"
    When the sidebar header renders
    Then the header shows "/payments"
    And the header does not show the front-clipped "/yments"
    And the header omits the "dev@abc1234*" ident for that frame

  @unit
  Scenario: A wide pane seats both the ident and the query
    Given the sidebar pane is 80 columns wide
    And the filter is open with the query "payments"
    When the sidebar header renders
    Then the header shows both "dev@abc1234*" and "/payments"

  @unit
  Scenario: With the filter off the ident shows as before
    Given the sidebar pane is at the default width of 40 columns
    And the filter is off
    When the sidebar header renders
    Then the header shows "dev@abc1234*"
