// Package resolvers — gh-backed resolver bodies.
//
// Workstream D-gh wires `Query.pullRequests`, `Query.issues`,
// `Query.workflowRuns`, plus `Project.pullRequests`/`Project.issues`
// and the per-PR/Issue review/comment edges, into the gh provider.
//
// Per-field GraphQL errors — when the daemon is not authenticated
// against GitHub the resolver returns the typed gh.ErrNotAuthenticated.
// gqlgen surfaces that as a per-field error in the GraphQL response,
// which is exactly the ADR-011 §6 / §12 contract: gh-derived fields
// fail individually while sibling fields (host, projects, ...) keep
// resolving.
package resolvers

import (
	"context"
	"fmt"
	"strings"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
)

// errGHNotConfigured is returned when the gh provider was never wired.
// This is an operator misconfiguration (missing WithGH option) and
// surfaces as a per-field GraphQL error so the rest of the schema
// continues to resolve.
var errGHNotConfigured = fmt.Errorf("gh provider not configured")

// queryPullRequestsResolver implements `Query.pullRequests(repo, state)`.
//
// Returns a slice of PullRequest, or a per-field GraphQL error when the
// daemon is not authenticated. The empty result is a valid GraphQL
// response — `[]` not `null` — so the resolver returns an allocated
// slice on the happy path.
func (r *queryResolver) queryPullRequestsResolver(ctx context.Context, repo string, state *graphql1.PullRequestState) ([]*graphql1.PullRequest, error) {
	if r.GH == nil {
		return nil, errGHNotConfigured
	}
	owner, name, err := gh.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	prs, err := r.GH.ListPullRequests(ctx, owner, name, mapPRState(state))
	if err != nil {
		return nil, err
	}
	out := make([]*graphql1.PullRequest, 0, len(prs))
	for _, p := range prs {
		out = append(out, toGraphQLPullRequest(p))
	}
	primeEnrichment(ctx, prs)
	return out, nil
}

// queryIssuesResolver implements `Query.issues(repo, state)`.
func (r *queryResolver) queryIssuesResolver(ctx context.Context, repo string, state *graphql1.IssueState) ([]*graphql1.Issue, error) {
	if r.GH == nil {
		return nil, errGHNotConfigured
	}
	owner, name, err := gh.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	issues, err := r.GH.ListIssues(ctx, owner, name, mapIssueState(state))
	if err != nil {
		return nil, err
	}
	out := make([]*graphql1.Issue, 0, len(issues))
	for _, i := range issues {
		out = append(out, toGraphQLIssue(i))
	}
	return out, nil
}

// queryIssueByNumberResolver implements `Query.issue(repo, number)`.
//
// Resolves null when the issue isn't found (404), the daemon isn't
// authenticated, or the repo is otherwise inaccessible. The "not found"
// case must be `(nil, nil)` so GraphQL surfaces it as `null` rather
// than as a per-field error — this lets callers cross-reference numbers
// without errors propagating into otherwise-healthy responses.
//
// Resolves issue #436.
func (r *queryResolver) queryIssueByNumberResolver(ctx context.Context, repo string, number int64) (*graphql1.Issue, error) {
	if r.GH == nil {
		return nil, errGHNotConfigured
	}
	owner, name, err := gh.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	issue, err := r.GH.GetIssue(ctx, gh.IssueKey{
		Owner:  owner,
		Name:   name,
		Number: int(number),
	})
	if err != nil {
		if gh.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return toGraphQLIssue(issue), nil
}

// queryPullRequestByNumberResolver implements `Query.pullRequest(repo, number)`.
// Same shape as `queryIssueByNumberResolver`. Resolves issue #436.
func (r *queryResolver) queryPullRequestByNumberResolver(ctx context.Context, repo string, number int64) (*graphql1.PullRequest, error) {
	if r.GH == nil {
		return nil, errGHNotConfigured
	}
	owner, name, err := gh.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	pr, err := r.GH.GetPullRequest(ctx, gh.PullRequestKey{
		Owner:  owner,
		Name:   name,
		Number: int(number),
	})
	if err != nil {
		if gh.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return toGraphQLPullRequest(pr), nil
}

// queryOpenPullRequestsResolver implements `Query.openPullRequests(repo, author)`.
//
// Lists every OPEN pull request on the repo, optionally filtered by
// author login. Unlike `Project.pullRequests` (which lists only PRs
// whose branch is locally checked out), this returns the full repo set.
//
// Author filtering happens server-side in this resolver — gh's REST
// `/repos/.../pulls` endpoint doesn't support an author filter, so we
// fetch all open PRs and filter the slice. For repos with thousands
// of open PRs this is wasteful; for the dashboards / standup workflows
// targeted by #435 it's well within the per_page=100 budget.
//
// Resolves issue #435.
func (r *queryResolver) queryOpenPullRequestsResolver(ctx context.Context, repo string, author *string) ([]*graphql1.PullRequest, error) {
	if r.GH == nil {
		return nil, errGHNotConfigured
	}
	owner, name, err := gh.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	prs, err := r.GH.ListPullRequests(ctx, owner, name, gh.PullRequestStateOpen)
	if err != nil {
		return nil, err
	}
	out := make([]*graphql1.PullRequest, 0, len(prs))
	kept := make([]gh.PullRequest, 0, len(prs))
	for _, p := range prs {
		if author != nil && *author != "" && p.AuthorLogin != *author {
			continue
		}
		out = append(out, toGraphQLPullRequest(p))
		kept = append(kept, p)
	}
	primeEnrichment(ctx, kept)
	return out, nil
}

// queryWorkflowRunsResolver implements `Query.workflowRuns(repo)`.
func (r *queryResolver) queryWorkflowRunsResolver(ctx context.Context, repo string) ([]*graphql1.WorkflowRun, error) {
	if r.GH == nil {
		return nil, errGHNotConfigured
	}
	owner, name, err := gh.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	runs, err := r.GH.ListWorkflowRuns(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	out := make([]*graphql1.WorkflowRun, 0, len(runs))
	for _, w := range runs {
		out = append(out, toGraphQLWorkflowRun(w))
	}
	return out, nil
}

// queryGhResolver implements `Query.gh(query, variables)` — the
// pass-through into GitHub's GraphQL API. Issue #418.
//
// The `variables` argument arrives as `interface{}` because the JSON
// scalar can carry any shape. Two valid shapes: nil (no variables) or
// a `map[string]any` keyed by GraphQL variable name. Anything else is
// a caller bug; reject it as a per-field error rather than panicking.
//
// Auth-not-bootstrapped returns the typed `errGHNotConfigured` /
// `gh.ErrNotAuthenticated` so the resolver layer surfaces it as a
// per-field GraphQL error (the rest of the schema keeps resolving).
// GitHub-level GraphQL errors ride through inside the returned envelope
// — they are not Go errors here.
func (r *queryResolver) queryGhResolver(ctx context.Context, query string, variables interface{}) (interface{}, error) {
	if r.GH == nil {
		return nil, errGHNotConfigured
	}
	vars, err := coerceGhVariables(variables)
	if err != nil {
		return nil, err
	}
	return r.GH.GraphQL(ctx, query, vars)
}

// coerceGhVariables narrows the JSON-scalar input to the shape the gh
// client wants. nil is allowed (no variables); a `map[string]any` is
// the happy path. We tolerate `nil` from an absent argument; anything
// else is a misuse of the field that should surface clearly rather
// than smuggle a typed-cast panic into the resolver.
func coerceGhVariables(v interface{}) (map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	return nil, fmt.Errorf("gh: variables must be a JSON object or null, got %T", v)
}

// Per-PR/Issue review and comment edges are auto-bound to struct fields;
// the gh provider populates them eagerly inside ListPullRequests / ListIssues.
// If lazy fetching is desired in a future workstream, register Reviews
// and Comments as resolvers in gqlgen.yml and reintroduce per-field
// resolver methods here.

// mapPRState collapses a *graphql.PullRequestState argument to the
// concrete provider enum the gh package expects. Nil → OPEN per the
// schema default. The mapping is deliberately verbose so a future
// schema rename can be tracked through a compile error here.
func mapPRState(s *graphql1.PullRequestState) gh.PullRequestState {
	if s == nil {
		return gh.PullRequestStateOpen
	}
	switch *s {
	case graphql1.PullRequestStateOpen:
		return gh.PullRequestStateOpen
	case graphql1.PullRequestStateClosed:
		return gh.PullRequestStateClosed
	case graphql1.PullRequestStateMerged:
		return gh.PullRequestStateMerged
	case graphql1.PullRequestStateAll:
		return gh.PullRequestStateAll
	default:
		return gh.PullRequestStateOpen
	}
}

func mapIssueState(s *graphql1.IssueState) gh.IssueState {
	if s == nil {
		return gh.IssueStateOpen
	}
	switch *s {
	case graphql1.IssueStateOpen:
		return gh.IssueStateOpen
	case graphql1.IssueStateClosed:
		return gh.IssueStateClosed
	case graphql1.IssueStateAll:
		return gh.IssueStateAll
	default:
		return gh.IssueStateOpen
	}
}

// mapPRStateBack converts a provider state back to the GraphQL enum.
func mapPRStateBack(s gh.PullRequestState) graphql1.PullRequestState {
	switch s {
	case gh.PullRequestStateOpen:
		return graphql1.PullRequestStateOpen
	case gh.PullRequestStateClosed:
		return graphql1.PullRequestStateClosed
	case gh.PullRequestStateMerged:
		return graphql1.PullRequestStateMerged
	case gh.PullRequestStateAll:
		return graphql1.PullRequestStateAll
	default:
		return graphql1.PullRequestStateOpen
	}
}

func mapIssueStateBack(s gh.IssueState) graphql1.IssueState {
	switch s {
	case gh.IssueStateOpen:
		return graphql1.IssueStateOpen
	case gh.IssueStateClosed:
		return graphql1.IssueStateClosed
	case gh.IssueStateAll:
		return graphql1.IssueStateAll
	default:
		return graphql1.IssueStateOpen
	}
}

// repoGitHubRepo derives owner / repo for a Repo. Per ADR-015 the slug
// IS the GitHub identity, so the slug short-circuits the lookup. The
// fallback (read the origin URL from `.git/config`) covers Repo nodes
// constructed without a slug — e.g. legacy peer-proxy stubs.
// Returns ok=false when neither path produces a valid owner/name pair.
func repoGitHubRepo(obj *graphql1.Repo) (owner, name string, ok bool) {
	if obj == nil {
		return "", "", false
	}
	if obj.Slug != "" {
		if i := strings.IndexByte(obj.Slug, '/'); i > 0 && i+1 < len(obj.Slug) {
			return obj.Slug[:i], obj.Slug[i+1:], true
		}
	}
	if obj.Path == "" {
		return "", "", false
	}
	url, err := gh.ReadOriginURL(obj.Path)
	if err != nil {
		return "", "", false
	}
	o, n, ok := gh.ParseGitHubURL(url)
	if !ok {
		return "", "", false
	}
	return o, n, true
}

// toGraphQLPullRequest projects a provider PullRequest into the GraphQL
// model. Kept here, not in the gh package, because the GraphQL types
// live in the codegen package and the gh package must stay independent
// of generated code.
func toGraphQLPullRequest(p gh.PullRequest) *graphql1.PullRequest {
	return &graphql1.PullRequest{
		ID:          p.ID(),
		RepoOwner:   p.RepoOwner,
		RepoName:    p.RepoName,
		Number:      int64(p.Number),
		Title:       p.Title,
		Body:        p.Body,
		State:       mapPRStateBack(p.State),
		Draft:       p.Draft,
		AuthorLogin: p.AuthorLogin,
		BaseRef:     p.BaseRef,
		HeadRef:     p.HeadRef,
		URL:         p.URL,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
}

func toGraphQLIssue(i gh.Issue) *graphql1.Issue {
	return &graphql1.Issue{
		ID:          i.ID(),
		RepoOwner:   i.RepoOwner,
		RepoName:    i.RepoName,
		Number:      int64(i.Number),
		Title:       i.Title,
		Body:        i.Body,
		State:       mapIssueStateBack(i.State),
		AuthorLogin: i.AuthorLogin,
		URL:         i.URL,
		CreatedAt:   i.CreatedAt,
		UpdatedAt:   i.UpdatedAt,
	}
}

// issueRefToGraphQL projects a thin gh.IssueRef into a graphql.Issue
// carrying identity + title. Body / labels / comments hydrate lazily
// on field selection via the standard Issue resolvers — the
// dependency fetch stays one round-trip for tree-shaped queries.
func issueRefToGraphQL(ref gh.IssueRef) *graphql1.Issue {
	return &graphql1.Issue{
		ID:        ref.ID(),
		RepoOwner: ref.Owner,
		RepoName:  ref.Name,
		Number:    int64(ref.Number),
		Title:     ref.Title,
	}
}

// issueRefsToGraphQL projects a slice of gh.IssueRef. Returns an empty
// (non-nil) slice so callers don't have to guard against null.
func issueRefsToGraphQL(refs []gh.IssueRef) []*graphql1.Issue {
	out := make([]*graphql1.Issue, 0, len(refs))
	for _, r := range refs {
		out = append(out, issueRefToGraphQL(r))
	}
	return out
}

// issueDependencies fetches the dependency-edge snapshot for an Issue
// resolver root. Returns nil deps and nil error on the soft-fail cases
// (gh not configured, malformed node id) so the field resolvers fall
// back to empty results without surfacing a GraphQL error.
func (r *issueResolver) issueDependencies(ctx context.Context, obj *graphql1.Issue) (*gh.IssueDependencies, error) {
	if r.GH == nil || obj == nil {
		return nil, nil
	}
	owner, name, number, ok := splitGHNodeID(obj.ID, "Issue:")
	if !ok {
		return nil, nil
	}
	deps, err := r.GH.EnrichIssueDependencies(ctx, gh.IssueKey{Owner: owner, Name: name, Number: number})
	if err != nil {
		return nil, err
	}
	return &deps, nil
}

// toGraphQLLabels projects gh.Label slice to the GraphQL Label list,
// preserving order. Returns an empty (non-nil) slice so callers don't
// have to guard against null. Used by both Issue.labels and
// PullRequest.labels.
func toGraphQLLabels(in []gh.Label) []*graphql1.Label {
	out := make([]*graphql1.Label, 0, len(in))
	for _, l := range in {
		out = append(out, &graphql1.Label{
			Name:        l.Name,
			Color:       l.Color,
			Description: l.Description,
		})
	}
	return out
}

func toGraphQLWorkflowRun(w gh.WorkflowRun) *graphql1.WorkflowRun {
	return &graphql1.WorkflowRun{
		ID:           w.ID(),
		RepoOwner:    w.RepoOwner,
		RepoName:     w.RepoName,
		RunID:        w.RunID,
		Name:         w.Name,
		WorkflowPath: w.WorkflowPath,
		Status:       w.Status,
		Conclusion:   w.Conclusion,
		HeadBranch:   w.HeadBranch,
		HeadSha:      w.HeadSHA,
		URL:          w.URL,
		CreatedAt:    w.CreatedAt,
		UpdatedAt:    w.UpdatedAt,
	}
}

func toGraphQLReview(r gh.PullRequestReview) *graphql1.PullRequestReview {
	return &graphql1.PullRequestReview{
		ID:          r.NodeID(),
		AuthorLogin: r.AuthorLogin,
		State:       r.State,
		Body:        r.Body,
		SubmittedAt: r.SubmittedAt,
	}
}

// toGraphQLReviews projects the enrichment's review slice, preserving order.
// Returns an empty (non-nil) slice so `reviews` resolves to `[]` rather than
// null on a PR nobody has reviewed (#651).
func toGraphQLReviews(in []gh.PullRequestReview) []*graphql1.PullRequestReview {
	out := make([]*graphql1.PullRequestReview, 0, len(in))
	for _, r := range in {
		out = append(out, toGraphQLReview(r))
	}
	return out
}

// toGraphQLReviewThreads projects the enrichment's review threads (#607).
// Returns an empty (non-nil) slice — the schema declares the list non-null.
func toGraphQLReviewThreads(in []gh.ReviewThread) []*graphql1.ReviewThread {
	out := make([]*graphql1.ReviewThread, 0, len(in))
	for _, t := range in {
		out = append(out, &graphql1.ReviewThread{
			ID:            t.NodeID(),
			IsResolved:    t.IsResolved,
			IsOutdated:    t.IsOutdated,
			Path:          t.Path,
			CommentCount:  int64(t.CommentCount),
			AuthorLogin:   t.AuthorLogin,
			LastUpdatedAt: t.LastUpdatedAt,
		})
	}
	return out
}

func toGraphQLComment(c gh.IssueComment) *graphql1.IssueComment {
	return &graphql1.IssueComment{
		ID:          c.NodeID(),
		AuthorLogin: c.AuthorLogin,
		Body:        c.Body,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
}

// mapMergeableState projects the provider MergeableState onto the GraphQL enum.
func mapMergeableState(s gh.MergeableState) graphql1.MergeableState {
	switch s {
	case gh.MergeableStateMergeable:
		return graphql1.MergeableStateMergeable
	case gh.MergeableStateConflicting:
		return graphql1.MergeableStateConflicting
	default:
		return graphql1.MergeableStateUnknown
	}
}

// mapReviewDecision projects a nullable provider ReviewDecision onto the
// nullable GraphQL ReviewDecisionEnum. Returns nil when the provider value is nil.
func mapReviewDecision(rd *gh.ReviewDecision) *graphql1.ReviewDecisionEnum {
	if rd == nil {
		return nil
	}
	switch *rd {
	case gh.ReviewDecisionApproved:
		v := graphql1.ReviewDecisionEnumApproved
		return &v
	case gh.ReviewDecisionChangesRequested:
		v := graphql1.ReviewDecisionEnumChangesRequested
		return &v
	case gh.ReviewDecisionReviewRequired:
		v := graphql1.ReviewDecisionEnumReviewRequired
		return &v
	case gh.ReviewDecisionCommented:
		v := graphql1.ReviewDecisionEnumCommented
		return &v
	case gh.ReviewDecisionDismissed:
		v := graphql1.ReviewDecisionEnumDismissed
		return &v
	default:
		// Unknown wire value (GitHub may add new ReviewDecision values
		// over time). The schema permits null; surface that rather than
		// silently misclassifying as REVIEW_REQUIRED.
		return nil
	}
}

// mapCiStatus projects the provider CiStatus onto the GraphQL CiStatus enum.
func mapCiStatus(s gh.CiStatus) graphql1.CiStatus {
	switch s {
	case gh.CiStatusSuccess:
		return graphql1.CiStatusSuccess
	case gh.CiStatusFailure:
		return graphql1.CiStatusFailure
	case gh.CiStatusPending:
		return graphql1.CiStatusPending
	default:
		return graphql1.CiStatusUnknown
	}
}
