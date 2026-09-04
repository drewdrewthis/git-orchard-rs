package query

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// pullRequestsQuery is the canonical projection for `query pull-requests`.
// We deliberately ask for the full set of scalar fields so the CLI shows
// the complete shape — consumers piping through `jq` can pick what they
// want. Reviews / comments are intentionally omitted; pulling them for
// every PR amplifies API cost, and the dedicated subcommands cover the
// per-PR detail case.
const pullRequestsQuery = `query PullRequests($repo: String!, $state: PullRequestState) {
  pullRequests(repo: $repo, state: $state) {
    id
    repoOwner
    repoName
    number
    title
    state
    draft
    authorLogin
    baseRef
    headRef
    url
    createdAt
    updatedAt
  }
}`

const issuesQuery = `query Issues($repo: String!, $state: IssueState) {
  issues(repo: $repo, state: $state) {
    id
    repoOwner
    repoName
    number
    title
    state
    authorLogin
    url
    createdAt
    updatedAt
  }
}`

const workflowRunsQuery = `query WorkflowRuns($repo: String!) {
  workflowRuns(repo: $repo) {
    id
    repoOwner
    repoName
    runId
    name
    workflowPath
    status
    conclusion
    headBranch
    headSHA
    url
    createdAt
    updatedAt
  }
}`

// pullRequestsCmd returns `orchard query pull-requests --repo OWNER/REPO --state open`.
func pullRequestsCmd() *cobra.Command {
	var (
		repo  string
		state string
	)
	cmd := &cobra.Command{
		Use:   "pull-requests",
		Short: "List pull requests on a GitHub repo (gh provider)",
		Long: "Issue the GraphQL `pullRequests(repo, state)` query against the running\n" +
			"orchard daemon and print the JSON array. The daemon hits the GitHub API\n" +
			"with the token cached at boot from `gh auth token`.",
		Example: "  orchard query pull-requests --repo alice/repo --state open\n" +
			"  orchard query pull-requests --repo alice/repo --state all",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repo == "" {
				return fmt.Errorf("--repo OWNER/REPO is required")
			}
			s, err := normalisePRState(state)
			if err != nil {
				return err
			}
			return runListWithVars(cmd.Context(), cmd.OutOrStdout(), pullRequestsQuery, "pullRequests", map[string]any{
				"repo":  repo,
				"state": s,
			})
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "owner/name of the GitHub repository")
	cmd.Flags().StringVar(&state, "state", "open", "filter: open|closed|merged|all")
	return cmd
}

// issuesCmd returns `orchard query issues --repo OWNER/REPO --state open`.
func issuesCmd() *cobra.Command {
	var (
		repo  string
		state string
	)
	cmd := &cobra.Command{
		Use:   "issues",
		Short: "List issues on a GitHub repo (gh provider)",
		Example: "  orchard query issues --repo alice/repo\n" +
			"  orchard query issues --repo alice/repo --state closed",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repo == "" {
				return fmt.Errorf("--repo OWNER/REPO is required")
			}
			s, err := normaliseIssueState(state)
			if err != nil {
				return err
			}
			return runListWithVars(cmd.Context(), cmd.OutOrStdout(), issuesQuery, "issues", map[string]any{
				"repo":  repo,
				"state": s,
			})
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "owner/name of the GitHub repository")
	cmd.Flags().StringVar(&state, "state", "open", "filter: open|closed|all")
	return cmd
}

// workflowRunsCmd returns `orchard query workflow-runs --repo OWNER/REPO`.
func workflowRunsCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:     "workflow-runs",
		Short:   "List GitHub Actions workflow runs on a repo (gh provider)",
		Example: "  orchard query workflow-runs --repo alice/repo",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repo == "" {
				return fmt.Errorf("--repo OWNER/REPO is required")
			}
			return runListWithVars(cmd.Context(), cmd.OutOrStdout(), workflowRunsQuery, "workflowRuns", map[string]any{
				"repo": repo,
			})
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "owner/name of the GitHub repository")
	return cmd
}

// normalisePRState maps the user-facing flag into the GraphQL enum
// value. The GraphQL transport is case-sensitive on enum spellings.
func normalisePRState(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "open":
		return "OPEN", nil
	case "closed":
		return "CLOSED", nil
	case "merged":
		return "MERGED", nil
	case "all":
		return "ALL", nil
	default:
		return "", fmt.Errorf("invalid --state %q (expected open|closed|merged|all)", s)
	}
}

func normaliseIssueState(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "open":
		return "OPEN", nil
	case "closed":
		return "CLOSED", nil
	case "all":
		return "ALL", nil
	default:
		return "", fmt.Errorf("invalid --state %q (expected open|closed|all)", s)
	}
}

// runListWithVars POSTs a GraphQL query with variables and prints the
// `data.<root>` array as pretty JSON. Falls back to printing the raw
// GraphQL envelope when there are errors so the user sees them
// verbatim.
func runListWithVars(ctx context.Context, w io.Writer, query, root string, vars map[string]any) error {
	raw, err := postGraphQLWithVars(ctx, query, vars)
	if err != nil {
		return err
	}
	var env struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string   `json:"message"`
			Path    []string `json:"path"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response: %w (body: %s)", err, raw)
	}
	if len(env.Errors) > 0 {
		// Return the first per-field GraphQL error verbatim — that's
		// the AC9 path: gh-derived field errors must surface to the
		// CLI with the daemon's actionable message intact.
		return fmt.Errorf("graphql error: %s", env.Errors[0].Message)
	}
	body, ok := env.Data[root]
	if !ok || len(body) == 0 {
		_, _ = w.Write([]byte("[]\n"))
		return nil
	}
	// Pretty-print whatever was at data.<root> — likely an array.
	var pretty any
	if err := json.Unmarshal(body, &pretty); err != nil {
		_, _ = w.Write(body)
		_, _ = w.Write([]byte("\n"))
		return nil
	}
	out, err := json.MarshalIndent(pretty, "", "  ")
	if err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	_, _ = w.Write(out)
	_, _ = w.Write([]byte("\n"))
	return nil
}
