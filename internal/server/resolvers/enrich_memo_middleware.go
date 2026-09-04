package resolvers

import (
	"context"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
)

// EnrichMemoMiddleware installs the gh package's request-scoped enrichment
// memo (gh.WithEnrichMemo) into the operation context for query and mutation
// operations. Wired via handler.Server.AroundOperations in both the daemon
// (internal/server/server.go) and the resolver e2e harness.
//
// The memo collapses primeEnrichment's warm-up and the per-field dataloader
// batch into one GitHub round-trip even for open PRs whose mergeable is
// UNKNOWN — the combination the durable cache correctly refuses to persist
// (#367, #813).
//
// Subscriptions are deliberately excluded: an AroundOperations wrapper fires
// once at subscription start and its ResponseHandler is reused for every
// emission, so a memo installed there would live for the whole subscription
// and serve stale enrichment across emissions. Subscription emissions take the
// no-memo passthrough (a direct EnrichPullRequest re-fetch), preserving the
// #367 freshness contract across a long-lived socket.
func EnrichMemoMiddleware(ctx context.Context, next graphql.OperationHandler) graphql.ResponseHandler {
	if oc := graphql.GetOperationContext(ctx); oc != nil && oc.Operation != nil && oc.Operation.Operation == ast.Subscription {
		return next(ctx)
	}
	return next(gh.WithEnrichMemo(ctx))
}
