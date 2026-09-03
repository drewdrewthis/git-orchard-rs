# orchardist#775 — orchard-sidebar: pinned sessions (drag-to-pin, P, M-Shift-Up/Down)
Feature: sidebar pinned sessions
  As an orchard user who returns to the same few sessions constantly
  I want to pin sessions into a stable block at the top of the sidebar that never
    reshuffles on activity, controlled by drag, the P key, and M-Shift-Up/Down
  So that my muscle memory for the top cards and M-1..9 settles instead of
    shuffling every time a background session ticks

  Background:
    Given the sidebar is showing sessions as one flat list ordered by attach recency
    And the sidebar remembers its pins in "pinned" inside sidebar-state.json

  @unit
  Scenario: P pins the selected card into the block
    Given an unpinned session is selected
    When I press "P"
    Then that session's name is appended to the pinned list
    And its card moves to the bottom of the pinned block above the flat list

  @unit
  Scenario: P again unpins the selected card
    Given a pinned session is selected
    When I press "P"
    Then that session's name is removed from the pinned list
    And its card returns to its attach-recency slot in the flat list

  @unit
  Scenario: Dragging an unpinned card into the block pins it
    Given an unpinned card in the flat list
    When I press it, move the pointer into the pinned block region, and release
    Then that session is pinned
    And its card renders inside the pinned block

  @unit
  Scenario: Dragging a pinned card out of the block unpins it
    Given a pinned card in the block
    When I press it, move the pointer below the separator, and release
    Then that session is unpinned
    And its card returns to the flat list

  @unit
  Scenario: A press and release with no motion attaches and never pins
    Given an unpinned card in the flat list
    When I press and release it without moving the pointer
    Then that session is attached as the selection
    And the pinned list is unchanged

  @unit
  Scenario: The pinned block renders above the flat list with a separator
    Given at least one pinned session
    When the sidebar renders the list
    Then the pinned rows render as one contiguous block at the top
    And a separator line that maps to no row follows the block
    And the attach-recency list renders below the separator

  @unit
  Scenario: Pinned rows do not reorder when other sessions gain activity
    Given two pinned sessions and one unpinned session
    When the unpinned session gains activity or is attached
    Then neither pinned row changes its index

  @unit
  Scenario: M-Shift-Up and M-Shift-Down reorder within the block
    Given a pinned card that is not at the top of the block is selected
    When I press M-Shift-Up
    Then it swaps position with the pinned card above it in the pinned list
    When I press M-Shift-Down twice
    Then it swaps down within the block by two positions if room remains

  @unit
  Scenario: Reorder is a no-op at the block ends
    Given the top pinned card is selected
    When I press M-Shift-Up
    Then the pinned list order is unchanged

  @unit
  Scenario: Reorder is a no-op when the selection is unpinned
    Given an unpinned session is selected
    When I press M-Shift-Up or M-Shift-Down
    Then the pinned list order is unchanged
    And no card moves

  @unit
  Scenario: Pins persist and restore in order
    Given I pin session A and then session B
    When the sidebar writes its state
    Then sidebar-state.json contains pinned equal to ["A","B"]
    And a fresh sidebar load renders A above B in the pinned block

  @unit
  Scenario: A width drag, collapse, or bell toggle never wipes the pins
    Given a non-empty pinned list
    When a width drag, a collapse toggle, or a bell toggle writes the sidebar state
    Then the written pinned array is unchanged

  @integration
  Scenario: A pinned name is dropped only when its session is truly gone
    Given a pinned session that is absent from the authoritative tmux live set on a sessions refresh
    When the sidebar folds that refresh in
    Then that name is removed from the pinned list
    And the pruned pinned list is persisted

  @integration
  Scenario: A pinned session missing during a transient daemon spike is retained
    Given a pinned session whose row is only held through a transient fast-lane failure
    When the sidebar handles that failure without the daemon being judged gone
    Then that session stays in the pinned list
    And nothing is persisted that drops it

  @unit
  Scenario: A missing or corrupt state file yields empty pins and still starts
    Given sidebar-state.json is missing or cannot be parsed
    When the sidebar loads its state
    Then the pinned list is empty
    And the sidebar renders with no pinned block

  @unit
  Scenario: The right-click menu offers Pin or Unpin by row state
    Given an unpinned card under the pointer
    When I right-click it
    Then the menu offers "Pin"
    Given a pinned card under the pointer
    When I right-click it
    Then the menu offers "Unpin"

  @unit
  Scenario: Activating the menu Pin item toggles the pin without attaching
    Given the row menu is open on an unpinned card
    When I activate "Pin"
    Then that session is added to the pinned list
    And the attached session and selection are unchanged

  @unit
  Scenario: M-1..9 ordinals count pinned cards first
    Given at least one pinned session above the flat list
    When the sidebar renders the list
    Then the first visible cards carrying ordinals are the pinned ones, in block order
    And M-1 selects the first pinned card
    And the separator line carries no ordinal

  @unit
  Scenario: An active filter narrows the pinned block
    Given two pinned sessions and an active "/" filter that matches only one of them
    When the sidebar renders the list
    Then only the matching pinned row renders in the block
    And the separator line still follows the surviving pinned run

  @unit
  Scenario: The separator is not drawn when the filter excludes every pinned row
    Given at least one pinned session and an active "/" filter that matches none of them
    When the sidebar renders the list
    Then no pinned row renders
    And the row minus-one separator line is not drawn

  @unit
  Scenario: Both outer.conf copies forward the reorder chords and stay identical
    Given cmd/orchard-shell/outer.conf and scripts/outer-shell/outer.conf
    When I inspect their bindings
    Then both bind "M-S-Up" to forward "S-Up" to pane 0.0
    And both bind "M-S-Down" to forward "S-Down" to pane 0.0
    And a diff of the two files is clean

  @e2e
  Scenario: P moves the selected card into the block in the running server
    Given the sidebar is running in the outer "-L orchard-shell" server
    When I press "P" on the selected card
    Then capture-pane shows that card inside the pinned block

  @e2e
  Scenario: Drag pins into the block and drag below the separator unpins, live
    Given the sidebar is running in the outer "-L orchard-shell" server
    When I drag an unpinned card into the block
    Then capture-pane shows it pinned
    When I drag a pinned card below the separator
    Then capture-pane shows it back in the flat list

  @e2e
  Scenario: The block renders above the flat list with the separator, live
    Given the sidebar is running in the outer "-L orchard-shell" server with at least one pin
    When I capture the pane
    Then the pinned block renders above the flat list with the separator line between them

  @e2e
  Scenario: M-Shift-Up/Down swaps a pinned card on screen, live
    Given the sidebar is running in the outer "-L orchard-shell" server with two pinned cards
    When I press M-Shift-Up on the lower pinned card
    Then capture-pane shows the two pinned cards swapped

  @e2e
  Scenario: Menu Pin/Unpin toggles block membership, live
    Given the sidebar is running in the outer "-L orchard-shell" server
    When I right-click a card and activate Pin, then right-click it and activate Unpin
    Then capture-pane shows it enter and then leave the pinned block

  @e2e
  Scenario: The block and its order survive a restart, live
    Given the sidebar is running in the outer "-L orchard-shell" server with an ordered pinned block
    When I restart the sidebar
    Then capture-pane shows the same pinned block in the same order

# --- AC Coverage Map ---
# AC 1  "P toggles pin"                              → Scenario: P pins the selected card into the block; Scenario: P again unpins the selected card
# AC 2  "Drag in pins / drag out unpins; no-motion=click" → Scenario: Dragging an unpinned card into the block pins it; Scenario: Dragging a pinned card out of the block unpins it; Scenario: A press and release with no motion attaches and never pins
# AC 3  "Block above flat list w/ separator; activity-stable" → Scenario: The pinned block renders above the flat list with a separator; Scenario: Pinned rows do not reorder when other sessions gain activity
# AC 4  "M-Shift-Up/Down reorder within block only"   → Scenario: M-Shift-Up and M-Shift-Down reorder within the block; Scenario: Reorder is a no-op at the block ends; Scenario: Reorder is a no-op when the selection is unpinned
# AC 5  "Persisted and restored, order preserved"     → Scenario: Pins persist and restore in order
# AC 6  "Other saves never wipe pins"                 → Scenario: A width drag, collapse, or bell toggle never wipes the pins
# AC 7  "Stale name dropped only when truly gone"     → Scenario: A pinned name is dropped only when its session is truly gone; Scenario: A pinned session missing during a transient daemon spike is retained
# AC 8  "Corrupt/absent state file → empty pins, starts" → Scenario: A missing or corrupt state file yields empty pins and still starts
# AC 9  "Right-click menu Pin/Unpin by state"         → Scenario: The right-click menu offers Pin or Unpin by row state; Scenario: Activating the menu Pin item toggles the pin without attaching
# AC 10 "M-1..9 counts pinned first"                  → Scenario: M-1..9 ordinals count pinned cards first
# AC 11 "Live verification (a-f)"                      → (a) Scenario: P moves the selected card into the block in the running server; (b) Scenario: Drag pins into the block and drag below the separator unpins, live; (c) Scenario: The block renders above the flat list with the separator, live; (d) Scenario: M-Shift-Up/Down swaps a pinned card on screen, live; (e) Scenario: Menu Pin/Unpin toggles block membership, live; (f) Scenario: The block and its order survive a restart, live
# AC 12 "Filter narrows the pinned block"             → Scenario: An active filter narrows the pinned block; Scenario: The separator is not drawn when the filter excludes every pinned row
# AC 13 "Both outer.conf copies forward reorder chords" → Scenario: Both outer.conf copies forward the reorder chords and stay identical
