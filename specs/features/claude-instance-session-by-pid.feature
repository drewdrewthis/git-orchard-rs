Feature: ClaudeInstance sessionUuid resolved by pid, not cwd
  # Issue #743. Root cause: cwdToSession (internal/server/resolvers/pane_claude.go,
  # built ~line 62, applied ~line 166) keys sessionUuid by cwd, explicitly last-wins
  # on duplicate cwds -> every ClaudeInstance sharing a worktree cwd collapses onto
  # one shared sessionUuid, even though each has a distinct live pid and transcript.
  #
  # Fix shape: new claudesessions provider exposing a SessionByPid axis over
  # ~/.claude/sessions/<pid>.json (liveness-filtered, same as claudeinstance's
  # LivenessChecker), wired provider -> resolver per ADR-022. Resolution order per
  # pane: (1) registry entry for the pane's own live pid, cwd-cross-checked against
  # the registry entry's recorded cwd; (2) cwd fallback (existing cwdToSession); (3)
  # nil when neither is unambiguous. Node ID stays pid-based (#711); only sessionUuid
  # resolution changes.

  Background:
    Given the daemon serves a GraphQL schema at 127.0.0.1:7777
    And the ClaudeInstance type exposes fields (id, sessionUuid, ...)
    And ClaudeInstance.id remains pid-based per #711
    And a claudesessions provider exposes SessionByPid(host, pid) over ~/.claude/sessions/<pid>.json

  # ===================================================================
  # AC 1 — Registry-first resolution
  # ===================================================================

  @unit
  Scenario: Resolver prefers the registry entry over cwdToSession for a pane's own pid
    Given a fake registry root with an entry for pid 8164 with sessionId "reg-uuid" and matching cwd
    And a cwdToSession map with a different value "cwd-uuid" for that pane's cwd
    When the resolver builds the ClaudeInstance for the pane with pid 8164
    Then the resolved sessionUuid equals "reg-uuid"
    And the resolved sessionUuid does not equal "cwd-uuid"

  # ===================================================================
  # AC 2 — Collision eliminated (gated unit)
  # ===================================================================

  @unit
  Scenario: Two panes sharing one cwd resolve two distinct sessionUuids from two registry entries
    Given two panes in the same cwd with live pids 8164 and 8165
    And a fake registry with entry pid 8164 -> sessionId "uuid-a" and pid 8165 -> sessionId "uuid-b"
    When the resolver builds ClaudeInstances for both panes
    Then instance A's sessionUuid equals "uuid-a"
    And instance B's sessionUuid equals "uuid-b"
    And instance A's sessionUuid does not equal instance B's sessionUuid

  # ===================================================================
  # AC 3 — Collision eliminated (ungated live proof)
  # ===================================================================

  @e2e
  Scenario: Two live claude REPLs in one worktree cwd report two distinct sessionUuids over real GraphQL
    Given a throwaway orchard daemon started on a non-7777 port
    And a tmux server on socket "-L i743"
    And two "claude" REPLs launched in that tmux server, both in the same worktree cwd
    When a GraphQL query selects { claudeInstances { id sessionUuid } }
    Then the response contains at least two rows
    And at least two rows have distinct non-null sessionUuid values
    And those two rows have distinct pid-based id values

  # ===================================================================
  # AC 4 — Ambiguous cwd -> nil
  # ===================================================================

  @unit
  Scenario: No registry entry and multiple conversations share the cwd yields nil sessionUuid
    Given a pane whose pid has no entry in the fake registry
    And the pane's cwd matches 2 or more conversations in claudeprojects
    When the resolver builds the ClaudeInstance for that pane
    Then the resolved sessionUuid is nil
    And the resolver returns no error

  # ===================================================================
  # AC 5 — Stale pid excluded
  # ===================================================================

  @unit
  Scenario: A registry entry for a dead pid is never attributed to a live instance on another pid
    Given a fake registry entry for pid 9999 which has no live process
    And a live pane on pid 8164 with no colliding registry entry
    When the resolver builds the ClaudeInstance for the pane on pid 8164
    Then the resolved sessionUuid is not the stale entry's value
    And the resolved sessionUuid comes from pid 8164's own registry entry or is nil

  # ===================================================================
  # AC 6 — Malformed registry degrades to fallback
  # ===================================================================

  @unit
  Scenario: A garbage registry file falls back to the cwd/nil path without error
    Given a fake registry root containing a non-JSON file for the pane's pid
    When the resolver builds the ClaudeInstance for that pane
    Then the resolved sessionUuid equals the cwd-fallback value
    And the resolver returns no error

  # ===================================================================
  # AC 7 — #711 identity unchanged (regression)
  # ===================================================================

  @unit
  Scenario: ClaudeInstance.ID stays pid-based after the sessionUuid fix
    Given the existing pane_claude_identity_test.go and tmux_pane_claude_instance_test.go suites
    When those tests are run in the same test run as this change
    Then both suites still pass
    And ClaudeInstance.ID is formatted as "ClaudeInstance:<host>:<pid>"

  # ===================================================================
  # AC 8 — Downstream consumer un-regressed
  # ===================================================================

  @integration
  Scenario: subscription.resolvers.go still resolves sessionUuid for a single-REPL cwd
    Given exactly one live claude REPL in a worktree cwd with a matching registry entry
    When the GraphQL subscription path in subscription.resolvers.go resolves that instance
    Then it returns the same sessionUuid as before this change
    And the existing test over that path passes

  # ===================================================================
  # AC 9 — ADR-022 gate documented
  # ===================================================================

  @unit
  Scenario: Provider package header names the node, the SessionByPid axis, and the wiring
    Given the claudesessions provider package doc comment
    When the comment is inspected
    Then it names the node "ClaudeInstance" (or "ClaudeSession")
    And it names the axis "SessionByPid"
    And it describes the provider -> resolver wiring per ADR-022

  # ===================================================================
  # AC 10 — Value-equivalence
  # ===================================================================

  @unit
  Scenario: SessionByPid's sessionId matches the jsonl-derived sessionUuid for the same live session
    Given a fake registry entry for pid 8164 with sessionId "abc-123"
    And a fake claudeprojects jsonl conversation for the same session with ID.SessionUUID "abc-123"
    When both values are compared for the pane on pid 8164
    Then SessionByPid(8164).sessionId is byte-equal to the jsonl ID.SessionUUID

  # ===================================================================
  # AC 11 — Single-pane path
  # ===================================================================

  @unit
  Scenario: tmuxPane.claudeInstance resolver resolves sessionUuid from the registry when cwdToSession is nil
    Given the cwdToSession == nil branch of tmuxPane.claudeInstance
    And a fake registry entry for the pane's pid with sessionId "solo-uuid"
    When the resolver is called directly for that pane
    Then the returned sessionUuid equals "solo-uuid"

  # ===================================================================
  # AC 12 — Pid reuse rejected
  # ===================================================================

  @unit
  Scenario: A live pid whose registry cwd mismatches the pane's cwd is not attributed
    Given a fake registry entry for pid 8164 with a recorded cwd different from the pane's resolved cwd
    And pid 8164 is alive
    When the resolver builds the ClaudeInstance for that pane
    Then the resolved sessionUuid is the cwd-fallback value or nil
    And the resolved sessionUuid is not the mismatched registry entry's value
    And the resolver returns no error

  # ===================================================================
  # AC 13 — Remote host uses fallback
  # ===================================================================

  @unit
  Scenario: A pane on a non-local host never attributes a local registry file
    Given a pane with host "remote-box" and a live pid that happens to match a local registry entry
    When the resolver builds the ClaudeInstance for that pane
    Then no local registry value is attributed
    And the resolved sessionUuid resolves via the cwd/nil fallback
    And the resolver returns no error

  # --- AC Coverage Map ---
  # AC 1: "Registry-first resolution"
  #   -> @unit "Resolver prefers the registry entry over cwdToSession for a pane's own pid"
  #
  # AC 2: "Collision eliminated (gated unit)"
  #   -> @unit "Two panes sharing one cwd resolve two distinct sessionUuids from two registry entries"
  #
  # AC 3: "Collision eliminated (ungated live proof)"
  #   -> @e2e "Two live claude REPLs in one worktree cwd report two distinct sessionUuids over real GraphQL"
  #
  # AC 4: "Ambiguous cwd -> nil"
  #   -> @unit "No registry entry and multiple conversations share the cwd yields nil sessionUuid"
  #
  # AC 5: "Stale pid excluded"
  #   -> @unit "A registry entry for a dead pid is never attributed to a live instance on another pid"
  #
  # AC 6: "Malformed registry degrades to fallback"
  #   -> @unit "A garbage registry file falls back to the cwd/nil path without error"
  #
  # AC 7: "#711 identity unchanged (regression)"
  #   -> @unit "ClaudeInstance.ID stays pid-based after the sessionUuid fix"
  #
  # AC 8: "Downstream consumer un-regressed"
  #   -> @integration "subscription.resolvers.go still resolves sessionUuid for a single-REPL cwd"
  #
  # AC 9: "ADR-022 gate documented"
  #   -> @unit "Provider package header names the node, the SessionByPid axis, and the wiring"
  #
  # AC 10: "Value-equivalence"
  #   -> @unit "SessionByPid's sessionId matches the jsonl-derived sessionUuid for the same live session"
  #
  # AC 11: "Single-pane path"
  #   -> @unit "tmuxPane.claudeInstance resolver resolves sessionUuid from the registry when cwdToSession is nil"
  #
  # AC 12: "Pid reuse rejected"
  #   -> @unit "A live pid whose registry cwd mismatches the pane's cwd is not attributed"
  #
  # AC 13: "Remote host uses fallback"
  #   -> @unit "A pane on a non-local host never attributes a local registry file"
