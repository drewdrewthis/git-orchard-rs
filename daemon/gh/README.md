# `daemon/gh/`

GitHub API: repos (github-side view), issues, pull requests, workflow runs.

## Owns

- **Types:** `PullRequest`, `PullRequestReview`, `ReviewThread`, `Issue`, `Label`, `IssueComment`, `WorkflowRun`
- **Enums:** `MergeableState`, `ReviewDecisionEnum`, `CiStatus`, `PullRequestState`, `IssueState`
- **Queries (typed core):** `pullRequests`, `openPullRequests`, `issues`, `issue`, `pullRequest`, `workflowRuns`
- **Query (pass-through, S16):** `gh(query: String!, variables: JSON): JSON` — forwards arbitrary GitHub GraphQL with the daemon's credentials
- **Subscriptions:** `pullRequestChanged`, `runChanged`
- **Mutations** (to be added in #613, each execs a script per [L5](../../RULES.md)):
  PR review/label/comment, issue create
- **Schema partial:** [`schema.graphql`](./schema.graphql) per [S15](../../RULES.md)

## S16: typed core + pass-through

The typed core covers the hot path with full optimization (cache, loaders, R3-clean, subscriptions). `Query.gh(query, variables): JSON` is the **pass-through escape hatch** — opaque JSON, no cache, no Node interface, no loader. Use it when the typed core doesn't (yet) cover a call shape; when a pass-through call becomes load-bearing, promote it to typed via an issue.

## Current source location (pre-refactor)

- `internal/server/providers/gh/`

## Constitution citations

- [L4](../../RULES.md): GitHub queries cache in-process; no script exec on the hot read path
- [L5](../../RULES.md): mutations exec `scripts/<op>` with `--json` output
- [O12](../../RULES.md): **stale-while-revalidate** is already done here ad-hoc for rate-limit; the refactor promotes it to first-class
- [O11](../../RULES.md): cache policy (read-through vs write-through) is explicit and not mixed within this module
- [S9](../../RULES.md): expected errors (rate-limit, permission-denied) are typed error union results, not `errors[]`
- [O1](../../RULES.md): the EnrichPR consolidation work (#615) lives in this module — duplicate single/batch paths get collapsed
