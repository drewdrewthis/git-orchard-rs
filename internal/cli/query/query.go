// Package query hosts the `orchard query` cobra subcommand group, which
// dispatches GraphQL queries against the running daemon.
//
// Named verbs land per workstream — host, repos, processes, panes,
// conversations, claude-account, host-services. The `--raw <gql>` escape hatch always works.
//
// The cobra alias `q` mirrors the impl guide's "permitted alias for
// query" so typing ergonomics match the documented CLI shape.
package query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/drewdrewthis/orchardist/internal/server"
)

// daemonURLEnv lets E2E tests redirect the CLI at an httptest.Server
// without binding the production port.
const daemonURLEnv = "ORCHARD_DAEMON_URL"

// daemonURL is the base URL of the local daemon's GraphQL endpoint.
// Overridable in tests via SetDaemonURLForTest, or via the
// ORCHARD_DAEMON_URL env var (which takes precedence per call so a
// running test process can drive multiple isolated daemons).
var daemonURL = "http://" + server.DefaultAddr

func resolveDaemonURL() string {
	if v := os.Getenv(daemonURLEnv); v != "" {
		return v
	}
	return daemonURL
}

// httpTimeout bounds GraphQL roundtrips. Conservative — the daemon is
// local and responses are small JSON.
const httpTimeout = 10 * time.Second

// Command returns the `query` subcommand group with its alias.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "query",
		Aliases: []string{"q"},
		Short:   "Query the running orchard daemon via GraphQL",
		Long: "Dispatch GraphQL queries against the running daemon at " + server.DefaultAddr + ".\n\n" +
			"Use a named verb (e.g. `host`, `repos`, `processes`, `panes`, `conversations`, `claude-account`, `host-services`) for high-level reads,\n" +
			"or `--raw '<gql>'` as the escape hatch when you need a custom GraphQL query.",
		Example: "  orchard query host\n" +
			"  orchard query repos\n" +
			"  orchard query processes\n" +
			"  orchard query panes\n" +
			"  orchard query conversations\n" +
			"  orchard query claude-account\n" +
			"  orchard query host-services\n" +
			"  orchard query --raw 'query { health { status } }'",
	}
	var raw string
	cmd.Flags().StringVar(&raw, "raw", "", "raw GraphQL query string")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if raw == "" {
			return fmt.Errorf("provide a verb (e.g. `host`, `repos`, `processes`, `panes`, `conversations`, `claude-account`, `host-services`) or --raw '<graphql>'")
		}
		return runRaw(cmd.Context(), cmd.OutOrStdout(), raw)
	}
	cmd.AddCommand(hostCmd())
	cmd.AddCommand(reposCmd())
	cmd.AddCommand(processesCmd())
	cmd.AddCommand(panesCmd())
	cmd.AddCommand(conversationsCmd())
	cmd.AddCommand(claudeAccountCmd())
	cmd.AddCommand(hostServicesCmd())
	cmd.AddCommand(claudeInstancesCmd())
	return cmd
}

// reposQuery is the GraphQL document fetched by `query repos`.
const reposQuery = `{ repos { id slug path } }`

func reposCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "repos",
		Short: "List configured repos (Repo nodes)",
		Long: "Calls the running daemon's GraphQL `repos` query and prints the\n" +
			"result as a JSON array. Returns the empty array when no repos are\n" +
			"configured. Use `orchard config add-repo PATH` to register a new one.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRepos(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

// runRepos POSTs the repos query, extracts the `data.repos` array, and
// prints it as pretty JSON. The shape is intentionally a plain
// `[]Repo` (not the GraphQL envelope) so shell consumers can `jq`
// straight in without learning the envelope.
func runRepos(ctx context.Context, w io.Writer) error {
	raw, err := postGraphQL(ctx, reposQuery)
	if err != nil {
		return err
	}
	var env struct {
		Data struct {
			Repos []map[string]any `json:"repos"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response: %w (body: %s)", err, raw)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("graphql error: %s", env.Errors[0].Message)
	}
	if env.Data.Repos == nil {
		env.Data.Repos = []map[string]any{}
	}
	out, err := json.MarshalIndent(env.Data.Repos, "", "  ")
	if err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	_, _ = w.Write(out)
	_, _ = w.Write([]byte("\n"))
	return nil
}

// runRaw POSTs `query` to the daemon's /graphql endpoint and pretty-prints
// the JSON response. Used by both the root --raw escape hatch and named
// verbs that don't decode the response into a typed shape.
func runRaw(ctx context.Context, w io.Writer, query string) error {
	raw, err := postGraphQL(ctx, query)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		// Fall back to raw bytes if the response wasn't JSON.
		_, _ = w.Write(raw)
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			_, _ = w.Write([]byte("\n"))
		}
		return nil
	}
	_, _ = w.Write(pretty.Bytes())
	_, _ = w.Write([]byte("\n"))
	return nil
}

// postGraphQLWithVars sends a GraphQL document with optional variables.
func postGraphQLWithVars(ctx context.Context, query string, variables map[string]any) ([]byte, error) {
	payload := map[string]any{"query": query}
	if len(variables) > 0 {
		payload["variables"] = variables
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resolveDaemonURL()+"/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon not running, start with `orchard daemon start` (%w)", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if pretty := prettyGraphQLErrors(data); pretty != "" {
			return nil, fmt.Errorf("daemon returned %d:\n%s", resp.StatusCode, pretty)
		}
		return nil, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

// postGraphQL sends a GraphQL document to the daemon and returns the
// raw response body.
func postGraphQL(ctx context.Context, query string) ([]byte, error) {
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resolveDaemonURL()+"/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon not running, start with `orchard daemon start` (%w)", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if pretty := prettyGraphQLErrors(data); pretty != "" {
			return nil, fmt.Errorf("daemon returned %d:\n%s", resp.StatusCode, pretty)
		}
		return nil, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

// prettyGraphQLErrors decodes a GraphQL error body and returns a
// human-readable rendering, or "" if the body isn't a recognisable
// GraphQL error envelope. Each error message is followed by its
// `locations` (line:col) when present.
//
// Resolves issue #398. Today's raw 422 dumps `{"errors":[{...}]}` JSON,
// which forces operators to eyeball line/column. The new format reads
//
//	error: Cannot query field "services" on type "Host". Did you mean "hostServices"?
//	  at line 1, col 16
//
// — readable in a terminal without copy/pasting through `jq`.
func prettyGraphQLErrors(body []byte) string {
	var env struct {
		Errors []struct {
			Message   string `json:"message"`
			Locations []struct {
				Line   int `json:"line"`
				Column int `json:"column"`
			} `json:"locations"`
			Path []any `json:"path"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if len(env.Errors) == 0 {
		return ""
	}
	var b strings.Builder
	for i, e := range env.Errors {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "error: %s\n", e.Message)
		for _, loc := range e.Locations {
			fmt.Fprintf(&b, "  at line %d, col %d\n", loc.Line, loc.Column)
		}
		if len(e.Path) > 0 {
			parts := make([]string, 0, len(e.Path))
			for _, p := range e.Path {
				parts = append(parts, fmt.Sprintf("%v", p))
			}
			fmt.Fprintf(&b, "  path: %s\n", strings.Join(parts, "."))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// SetDaemonURLForTest overrides the daemon URL. Tests that drive the
// CLI against an httptest.Server use this to redirect HTTP traffic
// without monkey-patching net/http.
func SetDaemonURLForTest(u string) func() {
	prev := daemonURL
	daemonURL = u
	return func() { daemonURL = prev }
}
