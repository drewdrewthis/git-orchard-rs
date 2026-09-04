# orchardist#734 — fix(orchard-sidebar): TPM auto-open is a silent no-op — session-created hook uses an unresolvable session format
Feature: auto-open the orchard sidebar in a newly created tmux session
  As an orchard user who installed orchard-sidebar via TPM
  I want a sidebar pane to open automatically in every new tmux session, whatever its name
  So that the sidebar is present without a manual toggle and the by-hand tmux.conf workaround can be retired

  # Root cause: the session-created hook substituted #{hook_session} (the "$N" id form).
  # run-shell hands its argument to /bin/sh, which expands the leading "$" as an empty
  # positional parameter, so scripts/sidebar-open.sh receives an EMPTY target and does an
  # untargeted split — a silent no-op. Fix: substitute #{q:hook_session_name} (the session
  # NAME, q-quoted so it survives the run-shell -> /bin/sh hop intact).
  # tmux sanitizes ':' and '.' out of session names at creation, so those cannot occur.

  Background:
    Given a throwaway tmux server on an isolated "-L" socket
    And a stub "orchard-sidebar" executable on PATH that records the pane it is launched in
    And the orchard-sidebar plugin loaded with "@orchard_sidebar_auto" at its default (on)

  @integration
  Scenario: A newly created session gets exactly one sidebar pane (AC1 — capability)
    When I create a new session "demo"
    Then "tmux list-panes -t demo:" shows exactly one pane whose start command is "orchard-sidebar"

  @integration
  Scenario: The hook passes the session name, not the unresolvable id form (AC2 — root cause)
    Given a capture stub is substituted for the sidebar target script
    When the session-created hook fires for a new session
    Then the capture stub records "$1" byte-for-byte equal to the created session's name
    And "$1" is never the empty string and never a bare "$N" id token

  @integration
  Scenario: The sidebar opens in the new session, not the attached one (AC3 — targeting)
    Given a client is attached to session "A"
    When I create a new session "B"
    Then "tmux list-panes -t B:" shows exactly one "orchard-sidebar" pane
    And "tmux list-panes -t A:" shows no "orchard-sidebar" pane

  @integration
  Scenario: A session name containing a space opens exactly one sidebar pane there (AC4 — spaced name)
    When I create a new session named "my session"
    Then "tmux list-panes -t 'my session:'" shows exactly one "orchard-sidebar" pane
    And an unrelated session shows no "orchard-sidebar" pane

  @integration
  Scenario: A session name with shell metacharacters opens exactly one sidebar pane there (AC5 — metachar name)
    Given a capture stub is substituted for the sidebar target script
    When I create a new session named "it's a$;x"
    Then the capture stub records "$1" byte-for-byte equal to "it's a$;x"
    And "tmux list-panes -t \"it's a$;x:\"" shows exactly one "orchard-sidebar" pane
    And no other session shows an "orchard-sidebar" pane

  @integration
  Scenario: An index-like session name resolves as a session, not a window index (AC6 — index-like name)
    When I create a new session named "1"
    Then "tmux list-panes -t '1:'" shows exactly one "orchard-sidebar" pane

  @integration
  Scenario: Re-running the open script for an already-open spaced session does not double-open (AC7 — idempotency)
    Given a sidebar is already open in session "my session"
    When I run "sidebar-open.sh 'my session'" again directly
    Then "tmux list-panes -t 'my session:'" still shows exactly one "orchard-sidebar" pane

  @integration
  Scenario: Auto-open disabled opens no sidebar pane (AC8 — auto disable)
    Given "@orchard_sidebar_auto" is set to "off"
    When I create a new session "demo"
    Then no "session-created" hook is registered
    And "tmux list-panes" shows no "orchard-sidebar" pane in the new session

  @integration
  Scenario: The prefix-key toggle still opens and closes the sidebar (AC9 — toggle regression)
    Given a client is attached to a session with no sidebar pane
    When I run "sidebar-toggle.sh" a first time
    Then the "orchard-sidebar" pane count in that session is 1
    When I run "sidebar-toggle.sh" a second time
    Then the "orchard-sidebar" pane count in that session is 0

  @integration
  Scenario: Auto-open seeds the sidebar width when unset (AC10 — width seeding #742)
    Given "@orchard_sidebar_width" is unset
    When a new session auto-opens the sidebar
    Then "tmux show-option -gqv @orchard_sidebar_width" returns "42"

# --- AC Coverage Map ---
# AC1 "New session opens exactly one orchard-sidebar pane in that session" → Scenario: A newly created session gets exactly one sidebar pane
# AC2 "Hook passes the session NAME (byte-equal), never the empty/$N id form" → Scenario: The hook passes the session name, not the unresolvable id form
# AC3 "Sidebar lands in the new session, not the attached one" → Scenario: The sidebar opens in the new session, not the attached one
# AC4 "Spaced name opens exactly one pane in that session, none elsewhere" → Scenario: A session name containing a space opens exactly one sidebar pane there
# AC5 "Shell-metachar name arrives intact, opens exactly one pane there" → Scenario: A session name with shell metacharacters opens exactly one sidebar pane there
# AC6 "Index-like name resolves as a session target" → Scenario: An index-like session name resolves as a session, not a window index
# AC7 "Re-running sidebar-open.sh for a spaced session does not double-open" → Scenario: Re-running the open script for an already-open spaced session does not double-open
# AC8 "@orchard_sidebar_auto off registers no hook and opens nothing" → Scenario: Auto-open disabled opens no sidebar pane
# AC9 "Prefix-key toggle still opens/closes in the current session" → Scenario: The prefix-key toggle still opens and closes the sidebar
# AC10 "Unset @orchard_sidebar_width is seeded to 42 on auto-open (#742)" → Scenario: Auto-open seeds the sidebar width when unset
