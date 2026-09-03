# orchardist#797 — outer tmux wrapper: clipboard
Feature: outer shell clipboard
  As an orchard user copying text out of a pane
  I want OSC 52 from pane programs to reach my real terminal clipboard
  So that Claude Code's copy and the inner tmux session's copy-mode both work

  @unit
  Scenario: outer.conf sets set-clipboard on in both copies
    Given "scripts/outer-shell/outer.conf" and "cmd/orchard-shell/outer.conf"
    Then both contain "set -s set-clipboard on"
    And the two files are byte-identical

  @e2e
  Scenario: A pane program's OSC 52 reaches the system clipboard
    Given a throwaway outer tmux socket has loaded outer.conf
    When a program in a pane emits an OSC 52 sequence base64-encoding "hi"
    Then the system clipboard contains "hi"

  @integration
  Scenario: Warp denies OSC 52 by default
    Given the outer terminal is Warp with default settings
    When a pane program emits an OSC 52 sequence
    Then Warp denies the clipboard write because "osc52_clipboard_access" is "deny"
    And docs/outer-shell.md explains how to set it to "allow" in Warp's settings
