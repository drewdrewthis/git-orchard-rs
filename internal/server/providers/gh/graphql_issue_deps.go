// Package gh — GraphQL enrichment for Issue dependency graph fields.
//
// GitHub exposes blocked-by / blocking / sub-issue / parent relationships
// behind the `GraphQL-Features: issue_types,sub_issues` preview header.
// This file adds a Provider method that fetches those edges in one
// GraphQL round-trip and caches the result on the per-issue map.
//
// Closes #563.
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// issueDepsTTL governs how long an enriched dependency snapshot is
// trusted before the next call re-fetches. Same shape as enrichmentTTL
// for PullRequest — short enough that newly-added blocked-by edges
// surface within a minute, long enough that batched UI queries reuse
// one fetch.
const issueDepsTTL = 60 * time.Second

// issueDepsPreviewHeaders are the preview gates required for the
// dependency nodes. GitHub returns the fields without them on some
// repositories, but sending the headers is the documented contract and
// avoids silent-empty regressions when GitHub tightens the gating.
var issueDepsPreviewHeaders = map[string]string{
	"GraphQL-Features":        "issue_types,sub_issues",
	"X-Github-Next-Global-ID": "1",
}

// issueDepsQuery requests the four dependency edges in one round-trip.
// Each connection caps at 50 nodes — enough for any real tracker
// hierarchy; oversize trackers can be revisited if they appear.
//
// Wire-format field names are GitHub's: `blockedBy`, `blocking`,
// `subIssues`, `parent`. We alias them to the orchard schema names
// (`blockedByIssues`, `blockingIssues`) so the resolver-side decoder
// stays in step with our public surface.
const issueDepsQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    issue(number:$number){
      blockedByIssues:blockedBy(first:50){
        nodes{ number, title, repository{ owner{ login } name } }
      }
      blockingIssues:blocking(first:50){
        nodes{ number, title, repository{ owner{ login } name } }
      }
      subIssues(first:50){
        nodes{ number, title, repository{ owner{ login } name } }
      }
      parent{ number, title, repository{ owner{ login } name } }
    }
  }
}`

// issueDepsRaw is the wire-shape decoder for issueDepsQuery.
type issueDepsRaw struct {
	Data struct {
		Repository struct {
			Issue struct {
				BlockedByIssues struct {
					Nodes []issueRefRaw `json:"nodes"`
				} `json:"blockedByIssues"`
				BlockingIssues struct {
					Nodes []issueRefRaw `json:"nodes"`
				} `json:"blockingIssues"`
				SubIssues struct {
					Nodes []issueRefRaw `json:"nodes"`
				} `json:"subIssues"`
				Parent *issueRefRaw `json:"parent"`
			} `json:"issue"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

// issueRefRaw mirrors GitHub's Issue node with enough fields to
// hydrate a thin Issue for the cross-issue projection. Title is
// included so common selections (`blockedByIssues { number title }`)
// don't need a second GetIssue round-trip per node.
type issueRefRaw struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	Repository struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"repository"`
}

func (r issueRefRaw) toRef() IssueRef {
	return IssueRef{
		Owner:  r.Repository.Owner.Login,
		Name:   r.Repository.Name,
		Number: r.Number,
		Title:  r.Title,
	}
}

// EnrichIssueDependencies fetches the four dependency edges for one
// issue and caches the result on the provider's per-issue map. The
// cache TTL is issueDepsTTL; subsequent calls within that window
// return the cached snapshot.
//
// Returns the live IssueDependencies value. Empty slices (not nil) are
// returned when GitHub reports no edges so callers can treat the
// result as iterable without a nil guard.
func (p *Provider) EnrichIssueDependencies(ctx context.Context, key IssueKey) (IssueDependencies, error) {
	// --- cache check ---
	p.issueMu.RLock()
	if cached, ok := p.issueDeps[key]; ok && p.clock().Sub(cached.at) < issueDepsTTL {
		out := cached.value
		p.issueMu.RUnlock()
		return out, nil
	}
	p.issueMu.RUnlock()

	c, err := p.httpClient(ctx)
	if err != nil {
		return IssueDependencies{}, err
	}

	variables := map[string]any{
		"owner":  key.Owner,
		"name":   key.Name,
		"number": key.Number,
	}
	raw, err := c.GraphQLWithHeaders(ctx, issueDepsQuery, variables, issueDepsPreviewHeaders)
	if err != nil {
		return IssueDependencies{}, fmt.Errorf("EnrichIssueDependencies graphql: %w", err)
	}

	var envelope issueDepsRaw
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return IssueDependencies{}, fmt.Errorf("EnrichIssueDependencies decode: %w", err)
	}
	if len(envelope.Errors) > 0 {
		// Partial success: keep the edges that resolved; error only when
		// no usable data came back.
		var dataProbe struct {
			Data json.RawMessage `json:"data"`
		}
		hasData := json.Unmarshal(raw, &dataProbe) == nil && hasRawData(dataProbe.Data)
		if !hasData {
			msgs := make([]string, 0, len(envelope.Errors))
			for _, e := range envelope.Errors {
				msgs = append(msgs, e.Message)
			}
			return IssueDependencies{}, fmt.Errorf("EnrichIssueDependencies graphql errors: %s", strings.Join(msgs, "; "))
		}
	}

	wire := envelope.Data.Repository.Issue
	deps := IssueDependencies{
		BlockedBy: refsFromNodes(wire.BlockedByIssues.Nodes),
		Blocking:  refsFromNodes(wire.BlockingIssues.Nodes),
		SubIssues: refsFromNodes(wire.SubIssues.Nodes),
	}
	if wire.Parent != nil {
		ref := wire.Parent.toRef()
		deps.Parent = &ref
	}

	now := p.clock()
	p.issueMu.Lock()
	p.issueDeps[key] = issueDepsEntry{value: deps, at: now}
	p.issueMu.Unlock()

	return deps, nil
}

// refsFromNodes projects a slice of issueRefRaw onto the public
// IssueRef shape. Returns an empty (non-nil) slice when nodes is empty
// so resolvers can iterate without a nil guard.
func refsFromNodes(in []issueRefRaw) []IssueRef {
	out := make([]IssueRef, 0, len(in))
	for _, n := range in {
		out = append(out, n.toRef())
	}
	return out
}
