// Package gh — the GitHub GraphQL enrichment query and its wire shapes.
//
// One selection set (enrichPRFields) is the single source of truth for both
// enrichment paths: the single-PR query builds a document around it, and the
// batch path splices it under one alias per PR. Adding a field here reaches
// both paths; that is deliberate, because a field added to only one of two
// hand-maintained copies is exactly how PullRequest.reviews came to resolve
// null (#651).
package gh

import "fmt"

// Connection caps for the enrichment fetch.
//
// Every field the enrichment covers rides one shared query, so these caps set
// the per-PR cost of the sidebar hot path — they are not per-consumer.
// Reviews are capped tighter than threads because review bodies are unbounded
// prose while a thread projection is a handful of scalars (S10).
const (
	enrichLabelCap  = 50
	enrichReviewCap = 20
	enrichThreadCap = 50
)

// enrichPRFields is the shared selection set expanded inside every aliased
// pull-request block. `latestComment` re-selects the comments connection from
// the other end so one thread yields both its opener (first comment's author)
// and its freshness (last comment's timestamp) — two one-node connections,
// which is far cheaper than paging the whole thread.
var enrichPRFields = fmt.Sprintf(
	`headRefOid mergeable mergeStateStatus reviewDecision`+
		` labels(first:%d){nodes{name color description}}`+
		` commits(last:1){nodes{commit{statusCheckRollup{state}}}}`+
		` reviews(last:%d){nodes{databaseId author{login} state body submittedAt}}`+
		` reviewThreads(first:%d){nodes{id isResolved isOutdated path`+
		` comments(first:1){totalCount nodes{author{login} createdAt}}`+
		` latestComment: comments(last:1){nodes{createdAt}}}}`,
	enrichLabelCap, enrichReviewCap, enrichThreadCap,
)

// enrichPRQuery is the single-PR enrichment document. Derived from
// enrichPRFields so it cannot drift from the batch path.
var enrichPRQuery = fmt.Sprintf(
	`query($owner:String!,$name:String!,$number:Int!){`+
		`repository(owner:$owner,name:$name){pullRequest(number:$number){%s}}}`,
	enrichPRFields,
)

// ghActor is GitHub's Actor shape. Nullable on the wire — a deleted account
// resolves to null — so it is always held behind a pointer.
type ghActor struct {
	Login string `json:"login"`
}

// login returns the actor's login, or "" when GitHub returned null.
func (a *ghActor) login() string {
	if a == nil {
		return ""
	}
	return a.Login
}

type enrichLabelNode struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type enrichCommitNode struct {
	Commit struct {
		StatusCheckRollup *struct {
			State string `json:"state"`
		} `json:"statusCheckRollup"`
	} `json:"commit"`
}

type enrichReviewNode struct {
	DatabaseID  int64    `json:"databaseId"`
	Author      *ghActor `json:"author"`
	State       string   `json:"state"`
	Body        string   `json:"body"`
	SubmittedAt string   `json:"submittedAt"`
}

type enrichThreadNode struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Comments   struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Author    *ghActor `json:"author"`
			CreatedAt string   `json:"createdAt"`
		} `json:"nodes"`
	} `json:"comments"`
	LatestComment struct {
		Nodes []struct {
			CreatedAt string `json:"createdAt"`
		} `json:"nodes"`
	} `json:"latestComment"`
}

// enrichPRAlias is the wire shape of one pull-request block, whether it
// arrives under a batch alias or under `data.repository.pullRequest`.
type enrichPRAlias struct {
	HeadRefOid       string  `json:"headRefOid"`
	Mergeable        string  `json:"mergeable"`
	MergeStateStatus string  `json:"mergeStateStatus"`
	ReviewDecision   *string `json:"reviewDecision"`
	Labels           struct {
		Nodes []enrichLabelNode `json:"nodes"`
	} `json:"labels"`
	Commits struct {
		Nodes []enrichCommitNode `json:"nodes"`
	} `json:"commits"`
	Reviews struct {
		Nodes []enrichReviewNode `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		Nodes []enrichThreadNode `json:"nodes"`
	} `json:"reviewThreads"`
}

// enrichRaw is the single-PR response envelope. It reuses enrichPRAlias so
// the two enrichment paths decode one struct, not two look-alikes.
type enrichRaw struct {
	Data struct {
		Repository struct {
			PullRequest enrichPRAlias `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}
