# orchardist#787 — sidebar resolves the inner client at click time and warns on a stale outer-shell launcher
Feature: click-time inner-client resolution and stale-launcher defence
  As an orchard user whose outer shell was restarted under a stale launcher
  I want the sidebar to validate its tmux client at click time and tell me when it cannot
  So that a click switches the session it can, and never fails silently

  Background:
    Given the sidebar is wrapped by the outer shell (an inner socket is set)

  @unit
  Scenario: A healthy env switches with the wrapper's own client unchanged
    Given ORCHARD_TMUX_CLIENT is a live inner client
    When I click a session row
    Then the switch is scoped to ORCHARD_TMUX_CLIENT unchanged
    And no footer error is shown

  @unit
  Scenario: Outer shell restarted, click still switches
    Given ORCHARD_TMUX_CLIENT is a tty that no longer belongs to any inner client
    And outer pane 0.1's tty is a live inner client
    When I click a session row
    Then the switch is scoped to outer pane 0.1's tty instead
    And no footer error is shown

  @unit
  Scenario: No inner client can be resolved, the click shows a footer error
    Given ORCHARD_TMUX_CLIENT is not a live inner client
    And outer pane 0.1's tty is also not a live inner client
    When I click a session row
    Then the click shows the footer error "inner client not found — restart the outer shell"
    And no switch-client is run

  @unit
  Scenario: list-clients errors or returns no clients, the click shows a footer error
    Given the inner server's list-clients errors or returns no clients
    When I click a session row
    Then the click shows the footer error "inner client not found — restart the outer shell"
    And the resolver never targets a tty it did not confirm equal to outer pane 0.1's tty

  @unit
  Scenario: The hand-back pane guard rejects a bad outer pane and falls back to pane 0.1
    Given ORCHARD_OUTER_PANE is not a %N pane id or equals the sidebar's own pane
    When the switch hands focus back
    Then the guard refuses the bad pane and falls back to outer pane 0.1's %N id
    And the refusal is logged once per process, not per click

  @unit
  Scenario: A stale launcher env shows a one-time outdated hint at startup
    Given the env shape is wrong (a non-%N outer pane, or a client tty not attached)
    When the sidebar starts
    Then it logs one drift line
    And it shows a one-time footer hint that the outer-shell launcher is outdated
    And a healthy env shows no hint and changes no behaviour

  @unit
  Scenario: doctor fails when orchard-shell and orchard-sidebar revisions differ
    Given orchard-shell and orchard-sidebar report different vcs.revision values
    When I run `orchard shell doctor`
    Then the suite-revision check FAILs naming both revisions
    And it reports OK when the two revisions are equal
