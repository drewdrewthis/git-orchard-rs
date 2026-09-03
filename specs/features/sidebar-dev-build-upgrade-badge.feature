Feature: Sidebar header upgrade badge is release-only, dev builds show their ident (#789)
  As an orchardist running a dev build of the sidebar
  I want the header to label a dev binary with its VCS revision instead of a phantom upgrade badge
  So that "⇡v<latest>" means a real release is newer, not that my own unversioned build compared as older

  Background:
    Given the sidebar's `version` var is `dev` outside a release build (cmd/orchard-sidebar/version.go)
    And `release.DevVersion` is the string `dev` (internal/release/semver.go)
    And `release.Compare` treats any non-semver string, `dev` included, as older than any real version
    And the header hint is drawn by `updateHint()` and its click zone by `header()` (cmd/orchard-sidebar/view.go)

  # =======================================================================
  # AC1 — Dev build labels itself, never a phantom upgrade badge
  # =======================================================================

  @issue-789
  Scenario: Dev build with a clean VCS stamp shows its revision, not the badge
    Given `version` is `dev`
    And the build carries `vcs.revision` `abcdef1234567` and `vcs.modified` `false`
    When the header renders
    Then the hint reads `dev@abcdef1`
    And it does not contain the upgrade glyph `⇡`
    And clicking the hint does not open the "update available" overlay

  @issue-789
  Scenario: Dev build from a dirty tree marks the ident with a trailing star
    Given `version` is `dev`
    And the build carries `vcs.revision` `abcdef1234567` and `vcs.modified` `true`
    When the header renders
    Then the hint reads `dev@abcdef1*`

  @issue-789
  Scenario: Dev build with no VCS stamp falls back to plain dev
    Given `version` is `dev`
    And the build carries no `vcs.revision` (built outside a repo or with -buildvcs=false)
    When the header renders
    Then the hint reads `dev`
    And it does not contain the upgrade glyph `⇡`

  # =======================================================================
  # AC2 — Release build: badge only when a newer release exists
  # =======================================================================

  @issue-789
  Scenario: Versioned build with an equal or older latest release shows no badge
    Given `version` is `1.2.3`
    And the cached check's latest release is `1.2.3` or older
    When the header renders
    Then the hint is empty
    And no click zone opens the "update available" overlay

  @issue-789
  Scenario: Versioned build with a newer release shows the upgrade badge
    Given `version` is `1.2.3`
    And the cached check's latest release is `1.3.0`
    When the header renders
    Then the hint reads `⇡v1.3.0`
    And clicking it opens the "update available" overlay
