# orchardist#747 · PR #748 — outer tmux wrapper: install, upgrade, and doctor
Feature: orchard install, upgrade, and doctor
  As an orchard user installing or maintaining the suite on a box
  I want a curl-able installer, a self-upgrade, and a doctor that proves the install is sound
  So that I can get orchard shell running and keep it healthy without hand-built steps

  Background:
    Given a release is published on GitHub with SHA256SUMS for all built targets

  @e2e
  Scenario: Fresh install on a clean box
    Given no orchard binaries are present
    When I run "curl -fsSL <installer-url> | bash"
    Then the suite binaries are installed to the resolved prefix
    And the command exits 0

  @e2e
  Scenario: Idempotent re-run installs nothing new
    Given the current version is already installed
    When I run the installer again for the same version
    Then no files are rewritten
    And the command still exits 0

  @integration
  Scenario: Checksum mismatch aborts and leaves existing binaries untouched
    Given a downloaded tarball whose checksum does not match SHA256SUMS
    When the installer verifies the download
    Then the installer aborts with a non-zero exit
    And any previously installed binaries are left unmodified

  @integration
  Scenario: --version pins a specific release
    When I run the installer with "--version v1.2.0"
    Then the installer resolves and installs exactly v1.2.0, not the latest release

  @integration
  Scenario: --json prints the envelope on stdout, progress on stderr
    When I run the installer with "--json"
    Then stdout contains exactly one JSON object with "ok" and "data" or "error"
    And progress lines are written to stderr, not stdout

  @e2e
  Scenario: Linux arm64 is a supported install target
    Given the target box reports OS "linux" and arch "aarch64"
    When the installer detects the platform
    Then it resolves the "aarch64-unknown-linux-gnu" (or equivalent linux/arm64) release asset
    And installation proceeds without a cross-compilation step

  @integration
  Scenario: upgrade --check reports without mutating anything
    Given a newer version than the running one is published
    When I run "orchard upgrade --check"
    Then it reports the current and latest versions
    And no binary on disk is modified

  @e2e
  Scenario: orchard upgrade replaces all binaries and doctor stays green
    Given a newer version is published
    When I run "orchard upgrade"
    Then every binary beside the running "orchard" is atomically replaced
    And "orchard shell doctor" reports all checks passing afterward

  @integration
  Scenario: upgrade refuses when the install directory is not writable
    Given the install directory has no write permission for the current user
    When I run "orchard upgrade"
    Then the upgrade is refused
    And the message names the unwritable directory

  @unit
  Scenario: doctor requires tmux 3.4 or newer
    Given "tmux -V" reports "tmux 3.3"
    When I run "orchard shell doctor"
    Then the tmux version check fails
    And the remedy names "tmux 3.4" as the minimum supported version

  @unit
  Scenario: doctor passes the tmux version check at the minimum supported version
    Given "tmux -V" reports "tmux 3.4"
    When I run "orchard shell doctor"
    Then the tmux version check passes

  @e2e
  Scenario: doctor --json reports every check with pass/warn/fail and a remedy
    When I run "orchard shell doctor --json"
    Then the output is a JSON envelope
    And each check reports its status and, when not passing, a one-line remedy

  @integration
  Scenario: doctor exits non-zero when any check fails
    Given at least one doctor check is failing
    When I run "orchard shell doctor"
    Then the command exits non-zero

  @unit
  Scenario: doctor detects a version mismatch across binaries
    Given "orchard", "orchard-shell", and "orchard-daemon" report different versions
    When I run "orchard shell doctor"
    Then the version-parity check fails

  @unit
  Scenario: doctor passes suite-revisions on a single-checkout install
    Given every Go suite binary reports the same VCS revision
    When I run "orchard shell doctor"
    Then the suite-revisions check passes
    And its detail names "orchard" as excluded because it is Rust

  @unit
  Scenario: doctor warns, not fails, when a suite binary lacks --revision
    Given "orchard-upgrade" predates --revision and every other Go suite binary reports the same revision
    When I run "orchard shell doctor"
    Then the suite-revisions check warns and names "orchard-upgrade"
    And its remedy says to rebuild the binary, not to reinstall

  @unit
  Scenario: doctor detects a suite built from different revisions
    Given six suite binaries on PATH where "orchard-upgrade" was built from a different commit than the rest
    When I run "orchard shell doctor"
    Then the suite-revisions check fails naming both revision groups and their binaries

  @unit
  Scenario: doctor fails suite-revisions when orchard-tui is stale
    Given every Go suite binary reports the same revision but "orchard-tui" reports a different one
    When I run "orchard shell doctor"
    Then the suite-revisions check fails naming "orchard-tui"

  @unit
  Scenario: doctor's socket flags and $ORCHARD_TMUX_SOCKET default match orchard shell
    Given $ORCHARD_TMUX_SOCKET is set to "from-env"
    When I run "orchard shell doctor" with no socket flags
    Then the inner-socket check runs against socket "from-env"
    And an explicit "--inner-socket" or "--outer-socket" flag overrides both the environment and the default

  @unit
  Scenario: doctor's systemd check recognizes orchard-daemon.service as well as orchard.service
    Given "systemctl --user is-active orchard-daemon.service" reports "active"
    And "systemctl --user is-active orchard.service" reports "inactive"
    When I run "orchard shell doctor" on Linux
    Then the systemd check passes and names "orchard-daemon.service" as the active unit

  @unit
  Scenario: doctor's path check warns when an earlier $PATH entry shadows a suite binary
    Given "orchard-shell" resolves to "/opt/orchard/orchard-shell"
    And "orchard-daemon" resolves via $PATH to "/home/u/go/bin/orchard-daemon", a stale build outside "/opt/orchard"
    When I run "orchard shell doctor"
    Then the path check warns
    And the remedy suggests reordering $PATH or removing the stale binary

  # orchardist#772 — the sidebar's card content (state glyph, model tag, last
  # message) depends on the claude-session-state Claude Code plugin's hooks.
  @unit
  Scenario: doctor passes the plugin check when claude-session-state is installed and writing state
    Given "~/.claude/plugins/installed_plugins.json" lists "claude-session-state@orchardist"
    And "~/.local/state/claude-sessions/state" contains at least one session file
    When I run "orchard shell doctor"
    Then the plugin check passes

  @unit
  Scenario: doctor warns when the claude-session-state plugin is not installed
    Given "~/.claude/plugins/installed_plugins.json" does not list "claude-session-state@orchardist"
    When I run "orchard shell doctor"
    Then the plugin check warns
    And the remedy is "/plugin marketplace add drewdrewthis/orchardist && /plugin install claude-session-state@orchardist"

  @unit
  Scenario: doctor warns when the plugin is installed but its hooks have never fired
    Given "~/.claude/plugins/installed_plugins.json" lists "claude-session-state@orchardist"
    And "~/.local/state/claude-sessions/state" is empty
    When I run "orchard shell doctor"
    Then the plugin check warns
    And the detail mentions that the hooks have never fired

  @integration
  Scenario: install prints a hint when the claude-session-state plugin is missing
    Given no "claude-session-state@orchardist" entry in "~/.claude/plugins/installed_plugins.json"
    When I run the installer
    Then the human output includes a "hint:" line naming the plugin install remedy
    And "--json" output includes a "hints" array containing the same remedy

  @integration
  Scenario: install prints no plugin hint when claude-session-state is already installed
    Given "~/.claude/plugins/installed_plugins.json" lists "claude-session-state@orchardist"
    When I run the installer
    Then the human output has no "hint:" line
    And "--json" output's "hints" array is empty
