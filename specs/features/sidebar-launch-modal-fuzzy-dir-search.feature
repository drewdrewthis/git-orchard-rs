Feature: Launch modal directory picker uses fuzzy search (#779)
  As someone launching a session from the sidebar
  I want the directory picker to fuzzy-match what I type, fzf-style
  So that I can jump to a deep directory with a few scattered letters instead of an exact substring

  @unit
  Scenario: A subsequence query matches, a non-subsequence query does not
    Given the directory holds "orchard-codex-scripts", "internal" and "target"
    When I search for "ocs"
    Then only "orchard-codex-scripts" is shown
    And searching for "xyz" shows nothing

  @unit
  Scenario: Matches are ordered by score, best first
    Given the directory holds "crates-orchard-md" and "cmd"
    When I search for "cmd"
    Then "cmd" is ranked above "crates-orchard-md"

  @unit
  Scenario: An empty query keeps alphabetical order and honours the hidden toggle
    Given the directory holds "src", ".git", "Docs", "docker" and "target"
    When I clear the search
    Then the entries are "docker", "Docs", "src", "target" in case-insensitive alphabetical order
    And toggling hidden reveals ".git"

  @unit
  Scenario: The parent entry stays pinned first whatever I type
    Given the directory holds "cmd" and "docs"
    When I search for "zzzz" which matches no directory
    Then the picker still lists ".." first with the cursor on it

  @unit
  Scenario: Fuzzy matching is case-insensitive
    Given the directory holds "Docs" and "docker"
    When I search for "doc"
    Then both "Docs" and "docker" are shown

  @e2e
  Scenario: The search field advertises itself as search
    Given the launch modal is open
    Then the directory field placeholder reads "(type to search)"
    And its label reads "search"
