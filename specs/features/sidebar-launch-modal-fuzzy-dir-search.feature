Feature: Launch modal fuzzy directory search over a walked tree (#782)
  As someone launching a session from the sidebar
  I want to type a few scattered letters and have the right directory anywhere
  under a few roots land at the top, fzf-style
  So that launching into a deep worktree is one query and one Enter, not a
  level-by-level walk (follow-up to #779 / #781)

  # On modal open a background walk gathers candidate directories once, from a
  # small root set (selected session cwd, the shared parent of known session
  # cwds, and $HOME), depth-bounded and capped. Each keystroke fuzzy-searches
  # the whole set; Enter picks the highlighted match as the launch directory.

  @unit
  Scenario: A scattered query reaches a deep directory wherever the picker opened
    Given the walked candidates include ".../git-orchard-rs/cmd/orchard-sidebar"
    When I search for "orsi"
    Then ".../git-orchard-rs/cmd/orchard-sidebar" is ranked first
    And pressing Enter selects it as the launch directory
    And searching for "zzqq" matches nothing

  @unit
  Scenario: Results rank by score, then by shorter path on ties
    Given the walked candidates include "/w/crates-orchard-md" and "/w/cmd"
    When I search for "cmd"
    Then "/w/cmd" is ranked above "/w/crates-orchard-md"
    And of two equally scored matches the shorter path ranks first

  @unit
  Scenario: The matched characters are highlighted in each shown path
    Given the walked candidates include "/w/orchard-sidebar"
    When I search for "orsi"
    Then the runes "o", "r", "s" and "i" are highlighted in the shown path

  @unit
  Scenario: The walk prunes noise and stays bounded
    Given a tree containing ".git", "node_modules", "target", a hidden dir, and nesting beyond depth 3
    When the candidate walk runs with the hidden toggle off
    Then none of ".git", "node_modules", "target", the hidden dir, or the depth-4 dir are candidates
    And the candidate count never exceeds the cap of 5000

  @unit
  Scenario: An empty query lists recent launch dirs first, then the roots
    Given the persisted recent launch dirs are "/w/recent-a" and "/w/recent-b"
    And the roots are "/w/recent-a" and "/home"
    When the search query is empty
    Then the picker lists "/w/recent-a", "/w/recent-b", "/home" in that order

  @unit
  Scenario: With no session selected the roots fall back sensibly
    Given no session is selected
    And the known session cwds share the parent "/w/ws"
    When the roots are resolved
    Then they are "/w/ws" and "$HOME"
    And "orsi" still finds the target under them

  @unit
  Scenario: An old single-record last-launch file still seeds recents
    Given a last-launch state file written before recents existed
    And no separate recents file exists yet
    When the modal opens
    Then the last-launch file is read but not modified
    And its recorded dir seeds the picker's recents list

  @unit
  Scenario: Backspace on an empty query widens the roots
    Given the search query is empty
    When I press Backspace
    Then each root's parent is added as a new root and the tree is re-walked

  @perf
  Scenario: Each keystroke matches and ranks within budget
    Given a candidate set of at least 1000 directories
    When I type one more character
    Then matching and ranking complete in under 50 ms

  @e2e
  Scenario: The search field advertises itself as search
    Given the launch modal is open
    Then the directory field placeholder reads "(type to search)"
    And its label reads "search"
