Feature: Daemon — no hidden N+1 for open PRs with `mergeable == UNKNOWN` (#813)
  As an orchardist or daemon client querying `pullRequests { headRefOid reviews reviewThreads unresolvedThreadCount ... }`
  I want open PRs whose `mergeable` is still UNKNOWN to resolve their enrichment from a single GitHub GraphQL round-trip
  So that the N+1-avoidance contract the enrichment warm-up exists to provide is not defeated by the durable cache's correct refusal to persist UNKNOWN

  # Scope: adds a REQUEST-SCOPED enrichment memo in the gh provider
  # (internal/server/providers/gh/), installed once per GraphQL operation via
  # gqlgen `AroundOperations` (internal/server/server.go:449; handler/server.go:122)
  # and consulted by EnrichPullRequest (graphql_enrich.go:62) and
  # BatchEnrichPullRequests (graphql_enrich_batch.go:33) before the durable
  # per-key cache. The durable cache and its #367 rule (shouldCacheEnrichment,
  # graphql_enrich.go:226) are UNCHANGED. The memo dies with the operation, so it
  # never leaks across a websocket connection the way the connection-scoped
  # loaders.Middleware (server.go:243) would.
  # Out of scope: consolidating single + batch enrichment paths (#615); any
  # loader/connection-scoped cache; changing durable cache TTLs.

  Background:
    Given a daemon GraphQL handler wired with the loaders middleware and the per-operation enrichment memo
    And a gh provider pointed at a stub GitHub API that counts every GraphQL round-trip
    And the REST pulls list reports the PRs as OPEN

  # =======================================================================
  # AC1 — No N+1 for the open + UNKNOWN-mergeable trigger condition
  # =======================================================================

  @e2e @issue-813
  Scenario: Three open UNKNOWN-mergeable PRs selecting five enrichment fields cost one GraphQL round-trip
    Given 3 open pull requests whose enrichment fixture carries `mergeable: "UNKNOWN"`
    When I query `pullRequests(repo: "alice/repo", state: OPEN) { number headRefOid unresolvedThreadCount reviews { authorLogin } reviewThreads { path } }`
    Then every PR resolves `headRefOid`, `reviews` (2), `reviewThreads` (4) and `unresolvedThreadCount` (3)
    And no GraphQL `errors` array is present
    And the captured GitHub GraphQL call counter equals 1
    # (This is TestPRReviewSurface_NoNPlusOne_UnknownMergeable, currently red observing 4.)

  # =======================================================================
  # AC2 — Memo is request-scoped, not connection-scoped
  # =======================================================================

  @integration @issue-813
  Scenario: Two operations on one websocket connection each perform their own enrichment fetch
    Given 3 open UNKNOWN-mergeable PRs and one long-lived websocket connection
    When I issue a first enrichment query on that connection
    And I issue a second, identical enrichment query on the SAME connection
    Then the captured GitHub GraphQL call counter equals 1 after the first operation
    And the captured GitHub GraphQL call counter equals 2 after the second operation
    # The per-operation memo is discarded at operation end; it must not serve the
    # first operation's enrichment to the second.

  # =======================================================================
  # AC3 — Regression: the non-UNKNOWN (MERGEABLE) surface is unaffected
  # =======================================================================

  @e2e @issue-813 @regression
  Scenario: The existing MERGEABLE no-N+1 and single-PR surfaces still hold
    Given the existing MERGEABLE-fixture review-surface tests
    When TestPRReviewSurface_NoNPlusOne and TestPRReviewSurface_E2E run
    Then both pass unchanged
    And each still costs exactly one GitHub GraphQL round-trip

  # =======================================================================
  # AC4 — #367 socket-safety: a state change is observable across the socket
  # =======================================================================

  @integration @issue-813 @issue-367
  Scenario: Re-querying after the PR's mergeable resolves returns the fresh value
    Given one socket / loader set and an open PR whose enrichment fixture is `mergeable: "UNKNOWN"`
    When I query the PR's enrichment once
    And the stub fixture is flipped from `UNKNOWN` to `MERGEABLE`
    And I re-query the PR's enrichment on the same socket / loader set
    Then the first response reports `mergeable: UNKNOWN`
    And the second response reports `mergeable: MERGEABLE`
    # Proves the memo did not harden UNKNOWN across the connection.

  # =======================================================================
  # AC5 — Race-clean at volume
  # =======================================================================

  @integration @issue-813 @race
  Scenario: Provider and resolver packages are race-clean over 50 iterations
    Given the memo's concurrent read/write across gqlgen's per-field goroutines
    When `go test -race -count=50 ./internal/server/providers/gh/... ./internal/server/resolvers/...` runs
    Then every iteration passes
    And no `WARNING: DATA RACE` is reported

  # --- AC Coverage Map ---
  # AC1 "e2e: 3 open UNKNOWN PRs × 5 fields → GraphQL calls == 1"
  #     → Scenario: Three open UNKNOWN-mergeable PRs selecting five enrichment fields cost one GraphQL round-trip
  # AC2 "memo is request-scoped — two operations on one ws connection each fetch"
  #     → Scenario: Two operations on one websocket connection each perform their own enrichment fetch
  # AC3 "regression: MERGEABLE surface (NoNPlusOne + E2E) still passes"
  #     → Scenario: The existing MERGEABLE no-N+1 and single-PR surfaces still hold
  # AC4 "#367 socket-safety: flip UNKNOWN→MERGEABLE, re-query returns MERGEABLE"
  #     → Scenario: Re-querying after the PR's mergeable resolves returns the fresh value
  # AC5 "go test -race -count=50 on gh + resolvers, zero race reports"
  #     → Scenario: Provider and resolver packages are race-clean over 50 iterations
