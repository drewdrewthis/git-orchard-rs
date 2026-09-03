Feature: Conversation recap falls back to away_summary
  As an orchard daemon operator
  I want Conversation.recap to also recognize away_summary system records
  So that a session that never ran /recap still shows its most recent
    background-generated summary, with the source visible to clients

  Background:
    Given a conversation jsonl tail containing zero or more of:
      | record kind         | shape                                                                          |
      | recap command output | an explicit /recap output record                                             |
      | away_summary          | {"type":"system","subtype":"away_summary","content":"...","timestamp":"...","uuid":"..."} |

  # ===========================================================================
  # AC 1 - readLatestRecap() recognizes away_summary records; Conversation.recap
  #        returns the newest candidate by transcript order; recapSource exposed
  # ===========================================================================

  @unit
  Scenario: Only an away_summary record is present
    Given the jsonl tail contains one away_summary record and no /recap output
    When readLatestRecap() scans the tail
    Then the returned recap content matches the away_summary record's "content"
    And "recapSource" is "AWAY_SUMMARY"

  @unit
  Scenario: Only an explicit /recap output record is present
    Given the jsonl tail contains one /recap output record and no away_summary record
    When readLatestRecap() scans the tail
    Then the returned recap content matches the /recap output record's content
    And "recapSource" is "RECAP_COMMAND"

  @unit
  Scenario: Both a /recap output and an away_summary are present, away_summary is newer
    Given the jsonl tail contains a /recap output record followed later by an away_summary record
    When readLatestRecap() scans the tail
    Then the returned recap content matches the away_summary record's "content"
    And "recapSource" is "AWAY_SUMMARY"

  @unit
  Scenario: Both a /recap output and an away_summary are present, /recap output is newer
    Given the jsonl tail contains an away_summary record followed later by a /recap output record
    When readLatestRecap() scans the tail
    Then the returned recap content matches the /recap output record's content
    And "recapSource" is "RECAP_COMMAND"

  @unit
  Scenario: Neither record kind is present
    Given the jsonl tail contains no /recap output record and no away_summary record
    When readLatestRecap() scans the tail
    Then the returned recap is empty
    And "recapSource" is not populated

  # ===========================================================================
  # AC 2 - Backward scan stays within maxLatestMarkersWindow; no full-file
  #        parse; malformed records are skipped, not fatal
  # ===========================================================================

  @unit
  Scenario: Backward scan falls back to a head scan when away_summary lies beyond the tail window
    Given away_summary is the only candidate and it lies outside "maxLatestMarkersWindow" bytes from the end
    When readLatestRecap() scans the tail
    Then the tail scan finds no candidate within "maxLatestMarkersWindow" bytes
    And the head-scan fallback runs
    And the returned recap content matches the away_summary record's "content"
    And "recapSource" is "AWAY_SUMMARY"

  @unit
  Scenario: A candidate inside the tail window short-circuits before the head-scan fallback
    Given the jsonl tail contains one away_summary record within "maxLatestMarkersWindow" bytes from the end
    When readLatestRecap() scans the tail
    Then the tail scan stops at that candidate
    And the scan does not read the rest of the file

  @unit
  Scenario: A malformed away_summary line is skipped without failing the scan
    Given the jsonl tail contains one malformed away_summary line followed by one well-formed /recap output record
    When readLatestRecap() scans the tail
    Then the returned recap content matches the /recap output record's content
    And no error is raised for the malformed line

  # ===========================================================================
  # AC 3 - Fixture unit tests for the five candidate combinations
  # ===========================================================================
  # Covered by the AC 1 scenarios above (only away_summary, only /recap,
  # both present, neither present) plus the AC 2 malformed-record scenario.

  # ===========================================================================
  # AC 4 - GraphQL schema + generated models expose recapSource; Schema
  #        Up-To-Date CI passes; orchard-gui shows an "auto" marker
  # ===========================================================================

  @integration
  Scenario: GraphQL schema exposes recapSource on Conversation
    Given the daemon GraphQL schema
    When the "Conversation" type is introspected
    Then it has a field "recap"
    And it has a field "recapSource" of enum type with values "RECAP_COMMAND" and "AWAY_SUMMARY"

  @integration
  Scenario: Generated GraphQL models stay in sync with the schema
    When the Schema Up-To-Date CI check runs
    Then it passes with no diff between the schema and generated models

  @integration
  Scenario: ChatView shows an auto marker when recapSource is AWAY_SUMMARY
    Given a Conversation whose "recapSource" is "AWAY_SUMMARY"
    When ChatView.svelte renders the recap
    Then it displays a small "auto" marker next to the recap

  @integration
  Scenario: SessionPane shows an auto marker when recapSource is AWAY_SUMMARY
    Given a Conversation whose "recapSource" is "AWAY_SUMMARY"
    When SessionPane.svelte renders the recap
    Then it displays a small "auto" marker next to the recap

  @integration
  Scenario: No auto marker is shown when recapSource is RECAP_COMMAND
    Given a Conversation whose "recapSource" is "RECAP_COMMAND"
    When ChatView.svelte and SessionPane.svelte render the recap
    Then neither view displays the "auto" marker

  # --- AC Coverage Map ---
  # AC 1 (readLatestRecap recognizes away_summary; newest-by-transcript-order wins; recapSource exposed)
  #   - @unit "Only an away_summary record is present"
  #   - @unit "Only an explicit /recap output record is present"
  #   - @unit "Both a /recap output and an away_summary are present, away_summary is newer"
  #   - @unit "Both a /recap output and an away_summary are present, /recap output is newer"
  #   - @unit "Neither record kind is present"
  #
  # AC 2 (backward scan stays within maxLatestMarkersWindow; malformed records skipped, not fatal)
  #   - @unit "Backward scan falls back to a head scan when away_summary lies beyond the tail window"
  #   - @unit "A candidate inside the tail window short-circuits before the head-scan fallback"
  #   - @unit "A malformed away_summary line is skipped without failing the scan"
  #
  # AC 3 (fixture unit tests for the five candidate combinations)
  #   - covered by the AC 1 and AC 2 scenarios above
  #
  # AC 4 (GraphQL schema + generated models; Schema Up-To-Date CI; GUI auto marker; no other GUI changes)
  #   - @integration "GraphQL schema exposes recapSource on Conversation"
  #   - @integration "Generated GraphQL models stay in sync with the schema"
  #   - @integration "ChatView shows an auto marker when recapSource is AWAY_SUMMARY"
  #   - @integration "SessionPane shows an auto marker when recapSource is AWAY_SUMMARY"
  #   - @integration "No auto marker is shown when recapSource is RECAP_COMMAND"
  #
  # AC 5 (this feature file covers the fixture cases from AC 3)
  #   - satisfied by this file's existence and the AC 3 coverage above
