// Package gh — wire→domain projections for PR enrichment.
//
// Pure mapping helpers shared by the single and batched enrichment paths
// (graphql_enrich.go, graphql_enrich_batch.go). Split out to keep
// graphql_enrich.go focused on the fetch/cache flow (file-size rule, RULES.md).
package gh

// PhaseLabels are orchard lifecycle tags that are excluded from
// PullRequest.Labels. Only user-assigned labels are surfaced.
// See ~/.claude/skills/gh-tag/ for the canonical list.
var phaseLabels = map[string]struct{}{
	"investigating": {},
	"needs-plan":    {},
	"needs-repro":   {},
	"planned":       {},
	"in-progress":   {},
	"in-ai-review":  {},
	"pr-ready":      {},
	"blocked":       {},
}

// mapMergeableState maps the raw GitHub string to the typed enum.
// Anything unrecognised maps to UNKNOWN, which is the safe fallback.
func mapMergeableState(s string) MergeableState {
	switch s {
	case "MERGEABLE":
		return MergeableStateMergeable
	case "CONFLICTING":
		return MergeableStateConflicting
	default:
		return MergeableStateUnknown
	}
}

// mapStatusCheckRollup maps GitHub's CommitStatusState / CheckStatusState
// to our CiStatus enum.
//
// Mapping rules (per issue #442 spec):
//   - any FAILURE or ERROR → FAILURE
//   - any PENDING or EXPECTED → PENDING
//   - SUCCESS → SUCCESS
//   - empty / nil / unknown → UNKNOWN
func mapStatusCheckRollup(state string) CiStatus {
	switch state {
	case "SUCCESS":
		return CiStatusSuccess
	case "FAILURE", "ERROR":
		return CiStatusFailure
	case "PENDING", "EXPECTED":
		return CiStatusPending
	default:
		return CiStatusUnknown
	}
}

// mapRollupFromCommits reads the aggregated CI state off the PR's head
// commit. The enrichment query selects `commits(last:1)`, so the single node
// (when present) is the head commit — the same commit headRefOid names.
func mapRollupFromCommits(nodes []enrichCommitNode) CiStatus {
	if len(nodes) == 0 {
		return CiStatusUnknown
	}
	rollup := nodes[0].Commit.StatusCheckRollup
	if rollup == nil {
		return CiStatusUnknown
	}
	return mapStatusCheckRollup(rollup.State)
}

// mapLabels projects the label connection onto the domain type.
func mapLabels(nodes []enrichLabelNode) []Label {
	out := make([]Label, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, Label{Name: n.Name, Color: n.Color, Description: n.Description})
	}
	return out
}

// mapReviews projects the reviews connection onto the domain type (#651).
func mapReviews(nodes []enrichReviewNode) []PullRequestReview {
	out := make([]PullRequestReview, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, PullRequestReview{
			GitHubID:    n.DatabaseID,
			AuthorLogin: n.Author.login(),
			State:       n.State,
			Body:        n.Body,
			SubmittedAt: n.SubmittedAt,
		})
	}
	return out
}

// mapReviewThreads projects the reviewThreads connection onto the domain
// type (#607). AuthorLogin comes from the thread's first comment (whoever
// opened it); LastUpdatedAt from its last (how fresh the conversation is).
func mapReviewThreads(nodes []enrichThreadNode) []ReviewThread {
	out := make([]ReviewThread, 0, len(nodes))
	for _, n := range nodes {
		t := ReviewThread{
			GitHubID:     n.ID,
			IsResolved:   n.IsResolved,
			IsOutdated:   n.IsOutdated,
			Path:         n.Path,
			CommentCount: n.Comments.TotalCount,
		}
		if first := n.Comments.Nodes; len(first) > 0 {
			t.AuthorLogin = first[0].Author.login()
			t.LastUpdatedAt = first[0].CreatedAt
		}
		if last := n.LatestComment.Nodes; len(last) > 0 {
			t.LastUpdatedAt = last[0].CreatedAt
		}
		out = append(out, t)
	}
	return out
}

// filterPhaseLabels returns the input slice with orchard phase labels
// removed, preserving the relative order of the remaining labels.
func filterPhaseLabels(in []Label) []Label {
	out := make([]Label, 0, len(in))
	for _, l := range in {
		if _, isPhase := phaseLabels[l.Name]; !isPhase {
			out = append(out, l)
		}
	}
	return out
}
