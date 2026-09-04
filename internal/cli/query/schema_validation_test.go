package query

// Guards every GraphQL document the `orchard query` verbs send against
// the canonical daemon schema. Issue #765: contracts.go shipped a
// `contracts(filter: ContractFilter)` document long after the contracts
// domain was deleted from the schema (#666). It compiled but 422'd at
// runtime. This test parses the schema and validates each query
// statically so a stale field or vanished type fails at build time, not
// in front of an operator.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// schemaPath resolves the canonical schema from the repo root. The test
// file lives at internal/cli/query/, so three parent hops reach root —
// mirroring query_e2e_test.go's repoRoot helper.
func schemaPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	return filepath.Join(root, "internal", "server", "resolvers", "schema.graphql")
}

// sentQueries maps a human-readable name to the exact GraphQL document
// the corresponding verb dispatches. Documents built with variable
// substitution are validated in their base (as-sent) form.
func sentQueries() map[string]string {
	return map[string]string{
		"repos":            reposQuery,
		"host":             hostQuery,
		"host-services":    hostServicesQuery,
		"claude-account":   claudeAccountQuery,
		"claude-instances": claudeInstancesQuery,
		"pull-requests":    pullRequestsQuery,
		"issues":           issuesQuery,
		"workflow-runs":    workflowRunsQuery,
		"panes":            panesQuery,
		"processes":        processesQuery,
		// conversations composes its document at runtime; validate the
		// widest form (both optional fields requested).
		"conversations": conversationsQuery(true, true),
	}
}

func TestSchemaValidation_AllQueriesParseAgainstSchema(t *testing.T) {
	raw, err := os.ReadFile(schemaPath(t))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	schema := gqlparser.MustLoadSchema(&ast.Source{
		Name:  "schema.graphql",
		Input: string(raw),
	})

	for name, doc := range sentQueries() {
		t.Run(name, func(t *testing.T) {
			_, errs := gqlparser.LoadQuery(schema, doc)
			if len(errs) > 0 {
				t.Fatalf("query %q does not validate against the daemon schema:\n%v\n\ndocument:\n%s", name, errs, doc)
			}
		})
	}
}
