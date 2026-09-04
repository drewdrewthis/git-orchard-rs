package query

// Guards every GraphQL document the `orchard query` verbs send against
// the canonical daemon schema. Issue #765: contracts.go shipped a
// `contracts(filter: ContractFilter)` document long after the contracts
// domain was deleted from the schema (#666). It compiled but 422'd at
// runtime. This test parses the schema and validates each query
// statically so a stale field or vanished type fails at build time, not
// in front of an operator.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2"
	gqlast "github.com/vektah/gqlparser/v2/ast"
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

// coveredQueryIdents lists the Go identifiers backing every entry in
// sentQueries. TestSchemaValidation_CoverageIsComplete cross-checks this
// against every GraphQL-document const actually declared in the package,
// so a new `fooQuery` const that isn't registered here fails the build
// instead of silently escaping validation.
//
// conversationsQuery is a func, not a const (its document depends on
// runtime flags), so it can't be discovered by the AST walk below. It is
// covered explicitly via sentQueries()["conversations"] and listed here
// only for documentation; the completeness walk skips funcs entirely.
func coveredQueryIdents() map[string]bool {
	return map[string]bool{
		"reposQuery":           true,
		"hostQuery":            true,
		"hostServicesQuery":    true,
		"claudeAccountQuery":   true,
		"claudeInstancesQuery": true,
		"pullRequestsQuery":    true,
		"issuesQuery":          true,
		"workflowRunsQuery":    true,
		"panesQuery":           true,
		"processesQuery":       true,
		// conversationsQuery: covered via the conversationsQuery() func, see above.
	}
}

// isGraphQLDocument reports whether a string literal's unquoted, trimmed
// value looks like a GraphQL document: a named operation (query/mutation/
// subscription) or the anonymous shorthand form starting with `{`.
func isGraphQLDocument(value string) bool {
	v := strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(v, "query "), strings.HasPrefix(v, "query{"):
		return true
	case strings.HasPrefix(v, "mutation "), strings.HasPrefix(v, "mutation{"):
		return true
	case strings.HasPrefix(v, "subscription "), strings.HasPrefix(v, "subscription{"):
		return true
	case strings.HasPrefix(v, "{"):
		return true
	default:
		return false
	}
}

// discoverQueryConstIdents parses every non-test .go file in dir and
// returns the identifier names of top-level const string literals whose
// value looks like a GraphQL document.
func discoverQueryConstIdents(t *testing.T, dir string) []string {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package dir %s: %v", dir, err)
	}

	var idents []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				genDecl, ok := decl.(*ast.GenDecl)
				if !ok || genDecl.Tok != token.CONST {
					continue
				}
				for _, spec := range genDecl.Specs {
					valueSpec, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range valueSpec.Names {
						if i >= len(valueSpec.Values) {
							continue
						}
						lit, ok := valueSpec.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						unquoted, err := strconv.Unquote(lit.Value)
						if err != nil {
							// Raw string literals (`...`) unquote fine via
							// strconv.Unquote too; a failure here means it
							// wasn't a plain string literal at all.
							continue
						}
						if isGraphQLDocument(unquoted) {
							idents = append(idents, name.Name)
						}
					}
				}
			}
		}
	}
	return idents
}

func TestSchemaValidation_CoverageIsComplete(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	dir := filepath.Dir(file)

	covered := coveredQueryIdents()
	for _, ident := range discoverQueryConstIdents(t, dir) {
		if !covered[ident] {
			t.Errorf("GraphQL document const %s is not covered by sentQueries() — register it in coveredQueryIdents (and sentQueries) so schema drift fails in CI", ident)
		}
	}
}

// rootSchemaPath resolves the gqlgen source-of-truth schema.graphql at the
// repo root (see gqlgen.yml `schema:`), as distinct from the mirror copy
// under internal/server/resolvers/ that this test's queries validate
// against.
func rootSchemaPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	return filepath.Join(root, "schema.graphql")
}

// TestSchemaValidation_SchemaMirrorInSync guards against the resolvers
// mirror drifting from the gqlgen source of truth — this package's other
// tests validate against the mirror, so a stale mirror would let a schema
// change go unnoticed here.
func TestSchemaValidation_SchemaMirrorInSync(t *testing.T) {
	source, err := os.ReadFile(rootSchemaPath(t))
	if err != nil {
		t.Fatalf("read root schema.graphql: %v", err)
	}
	mirror, err := os.ReadFile(schemaPath(t))
	if err != nil {
		t.Fatalf("read resolvers schema.graphql: %v", err)
	}
	if !bytes.Equal(source, mirror) {
		t.Fatalf("schema.graphql (repo root) and internal/server/resolvers/schema.graphql have diverged; run make generate to resync the mirror")
	}
}

func TestSchemaValidation_AllQueriesParseAgainstSchema(t *testing.T) {
	raw, err := os.ReadFile(schemaPath(t))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	schema := gqlparser.MustLoadSchema(&gqlast.Source{
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
