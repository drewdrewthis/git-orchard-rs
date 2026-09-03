# orchardist#747 · PR #748 — outer tmux wrapper: single-version suite
Feature: orchard suite versioning
  As an orchard user checking which build I'm running
  I want one version number for the whole suite, baked in at build time
  So that `--version` never leaves me guessing which daemon/shell/CLI I have

  Background:
    Given a release was built from tag "v1.3.0"

  @unit
  Scenario: orchard --version reports the release tag's version
    When I run "orchard --version"
    Then it prints "1.3.0"

  @e2e
  Scenario: All three primary binaries report the same version after a release build
    When I run "orchard --version", "orchard shell --version", and "orchard-daemon --version"
    Then all three report "1.3.0"

  @unit
  Scenario: An unstamped dev build reports "dev"
    Given a binary built without the version ldflag
    When I run "--version" on it
    Then it prints "dev"

  @unit
  Scenario: A dev build is always treated as older than any published release
    Given the running binary reports version "dev"
    And a published release exists at "1.0.0"
    When the update check compares versions
    Then "dev" is treated as older, so an update is reported available

  @e2e
  Scenario: The sidebar shows an update indicator only when a newer version is cached
    Given the update-check cache file reports current "1.2.0" and latest "1.3.0"
    When the sidebar header renders
    Then it shows "⇡v1.3.0"
    And clicking the update glyph shows an overlay with "run: orchard upgrade"

  @unit
  Scenario: The sidebar never performs its own network check
    Given the update-check cache file does not exist
    When the sidebar header renders
    Then no update indicator is shown
    And the sidebar makes no network request to determine this

  @unit
  Scenario: The update-check cache is refreshed at most once per 24 hours
    Given the update-check cache file was last written 2 hours ago
    When "orchard-shell" boots
    Then no refresh is triggered

  @unit
  Scenario: ORCHARD_NO_UPDATE_CHECK disables the background refresh
    Given the environment variable "ORCHARD_NO_UPDATE_CHECK" is set
    And the update-check cache file is older than 24 hours
    When "orchard-shell" boots
    Then the cache file is not refreshed

  @unit
  Scenario: The embedded outer.conf is content-hashed so upgrades never reuse a stale copy
    Given "orchard-shell" boots and materialises its embedded config
    Then the materialised path includes a content hash of the embedded "outer.conf"
    And upgrading to a build with a changed "outer.conf" writes to a new path

  @unit
  Scenario: The header shows a dim update glyph and version when a newer release is cached
    Given the update-check cache file reports current "1.2.0" and latest "1.3.0"
    When the sidebar header renders
    Then it shows "⇡v1.3.0"

  @unit
  Scenario: No update glyph when the cached version is not newer
    Given the update-check cache file reports current "1.3.0" and latest "1.3.0"
    When the sidebar header renders
    Then no update indicator is shown

  @unit
  Scenario: A dev build always shows the update glyph
    Given the running binary reports version "dev"
    And the update-check cache file reports latest "1.0.0"
    When the sidebar header renders
    Then it shows "⇡v1.0.0"

  @unit
  Scenario: The update glyph is the first thing dropped on a narrowing pane
    Given the update-check cache file reports current "1.2.0" and latest "1.3.0"
    When the sidebar header renders at an inner width below 24 columns
    Then no update indicator is shown
    And the "+" launch button and "«" collapse button still render

  @unit
  Scenario: The collapsed strip shows the bare update glyph with no version number
    Given the update-check cache file reports current "1.2.0" and latest "1.3.0"
    And the sidebar pane is collapsed to 3 columns
    When the collapsed strip renders
    Then it shows "⇡"
    And it does not show "1.3.0"

  @unit
  Scenario: Clicking the update glyph opens an overlay naming the upgrade command
    Given the update-check cache file reports current "1.2.0" and latest "1.3.0"
    When I click the update glyph in the header
    Then an overlay reads "update available v1.2.0 → v1.3.0 — run: orchard upgrade"

  @unit
  Scenario: Any keypress dismisses the update overlay
    Given the update overlay is open
    When I press any key
    Then the overlay closes

  @unit
  Scenario: A missing or corrupt update-check cache file shows no indicator and logs nothing
    Given the update-check cache file is missing or not valid JSON
    When the sidebar header renders
    Then no update indicator is shown
    And nothing is logged

  @unit
  Scenario: A state directory that cannot be resolved is logged once, not on every check
    Given the update-check cache path cannot be resolved
    When the sidebar checks for an update twice
    Then only one log line is written
