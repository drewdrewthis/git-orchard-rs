Feature: Sidecar janitor deletes only provably-dead sidecars (pid-liveness, never tmux-view inference)
  As an orchardist running one or more orchard daemons that share a host heartbeat directory
  I want the startup sidecar janitor to delete a sidecar only when its recorded process is provably dead
  So that a daemon whose tmux view is empty, partial, or on a different socket can never sweep the live sidecars owned by another daemon or session (issue #826, a data-loss bug)

  # Issue #826 [P1] — data-loss bug. Root cause: the janitor decided "orphan"
  # by "session name absent from THIS daemon's single-socket tmux snapshot",
  # and the sidecar carried no pid, so it had no ground-truth liveness signal.
  # When tmux was unreachable, FetchAll returned an empty snapshot with nil
  # error, so liveSessions={} and every sidecar was swept. Reproduced: a
  # throwaway daemon with an empty tmux view swept 3 live-session sidecars.
  #
  # Fix (from the ## Plan planning comment): the hook (orchard-state.sh)
  # records the Claude pid in each sidecar; the janitor deletes a sidecar IFF
  # it records a same-host pid>0 that is NOT alive (signal-0 via the existing
  # OSLivenessChecker). The delete decision no longer consults the tmux
  # snapshot at all, so empty/partial/cross-socket views are irrelevant by
  # construction. Every uncertainty (no pid, parse error, reused pid) → KEEP.
  #
  # Test-tree note: the janitor scenarios bind to
  # internal/server/providers/claudeinstance/*_test.go (Go); the hook writer
  # scenario (AC4) binds to the orchard-state.sh hook test (bats). This file
  # is the create-issue chain's contract; coders bind each @scenario in the
  # matching test tree.

  Background:
    Given the orchard daemon runs a startup-only sidecar janitor over the resolved heartbeat dir (ORCHARD_HEARTBEAT_DIR > TMPDIR > /tmp)
    And each sidecar is named "orchard-claude-<tmux-session>.json"
    And the janitor deletes a sidecar only when it records a same-host pid > 0 that is not alive
    And "not deleted" means the file is still present after the startup sweep completes

  # =======================================================================
  # AC1 — empty / unreachable tmux view must not sweep a live-pid sidecar
  # =======================================================================
  @integration
  Scenario: Empty or unreachable tmux snapshot never deletes a live-pid sidecar
    Given a daemon whose tmux fetch returns an empty snapshot, and separately whose tmux server is down so FetchAll returns empty with a nil error
    And the heartbeat dir holds a sidecar whose recorded pid is a currently-live process
    When the startup janitor sweeps
    Then the sidecar is not deleted
    And the sweep log records no removal for it

  # =======================================================================
  # AC2 — cross-socket live session (the central reported scenario)
  # =======================================================================
  @integration
  Scenario: A live session's sidecar on a different tmux socket is never deleted
    Given a daemon running against tmux socket A with a non-empty successful snapshot that does not list session S
    And session S is live on tmux socket B with a sidecar whose recorded pid is alive in the shared heartbeat dir
    When the startup janitor sweeps
    Then S's sidecar is not deleted
    And the sweep log records zero removals for S

  # =======================================================================
  # AC3 — delete decision matrix: dead pid deletes, live/reused pid keeps
  # =======================================================================
  @unit
  Scenario Outline: Janitor deletes only when the recorded pid is not alive
    Given a well-formed sidecar recording pid <pid> which the liveness checker reports as <liveness>
    When Sweep runs with the injected liveness checker
    Then the sidecar is <outcome>
    And a removal is <counted> in the returned count

    Examples:
      | pid  | liveness            | outcome     | counted     |
      | 4242 | dead                | deleted     | counted     |
      | 4242 | alive               | kept        | not counted |
      | 4242 | alive (pid reused)  | kept        | not counted |

  # =======================================================================
  # AC4 — hook writer contract: sidecar carries the real pid, round-trips
  # =======================================================================
  @integration
  Scenario: The hook writes the session's real pid and the janitor reads the same field
    Given orchard-state.sh fires on a PreToolUse event inside a tmux pane
    When it writes the sidecar
    Then the sidecar JSON "pid" field is numeric and equals the actual pid of the session's Claude process
    And a subsequent janitor run with that pid alive keeps the sidecar

  # =======================================================================
  # AC5 — regression: genuine orphan swept AND present live session kept
  # =======================================================================
  @integration
  Scenario: A dead-pid orphan is removed while a live-pid sidecar in the same dir is kept
    Given the heartbeat dir holds one sidecar with a dead recorded pid and one with a live recorded pid
    When the startup janitor sweeps on a correctly-socketed daemon
    Then the dead-pid sidecar is deleted
    And the live-pid sidecar is not deleted
    And the returned swept count is exactly one

  # =======================================================================
  # AC6 — backward-compat: legacy sidecar with no pid is never deleted
  # =======================================================================
  @unit
  Scenario: A legacy sidecar with no pid field is kept regardless of tmux view
    Given a legacy sidecar with no "pid" field
    And any tmux view (empty, partial, or full)
    When Sweep runs
    Then the sidecar is kept

  # =======================================================================
  # AC7 — failure-mode: unparseable sidecar is skipped, sweep continues
  # =======================================================================
  @unit
  Scenario: An invalid or truncated sidecar is skipped and the sweep processes the rest
    Given the heartbeat dir holds a malformed (invalid or truncated JSON) sidecar and a well-formed dead-pid sidecar
    When Sweep runs
    Then the malformed sidecar is kept
    And a warning is logged for it
    And the well-formed dead-pid sidecar is still deleted

  # =======================================================================
  # AC8 — rollback: janitor errors never block daemon startup
  # =======================================================================
  @unit
  Scenario: A janitor error is logged and swallowed, startup continues
    Given the janitor hits an error reading the heartbeat dir or probing liveness
    When Sweep runs
    Then Sweep logs the error, returns 0, and does not panic
    And the daemon proceeds to bind its listener

# --- AC Coverage Map ---
# AC1: "empty/unreachable tmux view never deletes a live-pid sidecar" → Scenario: Empty or unreachable tmux snapshot never deletes a live-pid sidecar
# AC2: "cross-socket live session's sidecar never deleted" → Scenario: A live session's sidecar on a different tmux socket is never deleted
# AC3: "delete iff recorded pid not alive; live/reused pid kept" → Scenario Outline: Janitor deletes only when the recorded pid is not alive
# AC4: "hook writes real pid, janitor round-trips it" → Scenario: The hook writes the session's real pid and the janitor reads the same field
# AC5: "genuine orphan swept AND present live session kept" → Scenario: A dead-pid orphan is removed while a live-pid sidecar in the same dir is kept
# AC6: "legacy sidecar (no pid) never deleted" → Scenario: A legacy sidecar with no pid field is kept regardless of tmux view
# AC7: "unparseable sidecar skipped, sweep continues" → Scenario: An invalid or truncated sidecar is skipped and the sweep processes the rest
# AC8: "janitor error non-blocking to startup" → Scenario: A janitor error is logged and swallowed, startup continues
