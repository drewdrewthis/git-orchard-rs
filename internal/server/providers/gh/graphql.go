package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// graphqlPath is the GitHub GraphQL endpoint path appended to the
// REST BaseURL. GitHub's GraphQL surface is hosted at the same origin
// as the REST API (`https://api.github.com/graphql`); GHES users
// resolve `<host>/api/graphql` via their GH_API_BASE_URL override.
const graphqlPath = "/graphql"

// GraphQL POSTs an arbitrary GraphQL query to GitHub's API and returns
// the full JSON envelope verbatim — `data`, `errors`, and any
// extensions GitHub attaches.
//
// The query string is opaque to the client: we do not parse it, do
// not introspect it, and do not validate it against any schema. GitHub
// validates server-side; validation errors surface as entries in the
// returned envelope's `errors` array, not as a Go error.
//
// Go-error returns are reserved for the things callers cannot recover
// from at the resolver layer: network failure, auth failure (401),
// rate limit (X-RateLimit-Remaining: 0 + 403), or non-2xx HTTP without
// a JSON body. Anything else — including GraphQL-level `errors` —
// rides through as part of the returned envelope so callers see what
// GitHub said verbatim.
//
// `variables` may be nil for queries that take no variables; callers
// pass a `map[string]any` keyed by GraphQL variable name.
func (c *Client) GraphQL(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	return c.GraphQLWithHeaders(ctx, query, variables, nil)
}

// GraphQLWithHeaders is the same as GraphQL but lets the caller attach
// extra request headers — required for GitHub's preview-gated fields
// (`GraphQL-Features: issue_types,sub_issues`, etc). Headers passed
// here are added AFTER the standard auth / content-type / API-version
// headers; callers cannot override the core set.
func (c *Client) GraphQLWithHeaders(ctx context.Context, query string, variables map[string]any, extraHeaders map[string]string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("gh client not initialised")
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("graphql: empty query")
	}

	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal graphql request: %w", err)
	}

	full := c.BaseURL + graphqlPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build graphql request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	// GitHub's GraphQL endpoint requires the JSON content type and
	// honours the same User-Agent / API-Version headers the REST path
	// uses; the Accept header is application/json (NOT the REST media
	// type — keeping the same string would still work, but JSON is the
	// canonical choice for GraphQL).
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if ua := c.UserAgent; ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := c.httpClient().Do(req)
	c.logCall(callKindGraphQL, graphqlPath, repoFromGraphQLVariables(variables), 0, start, resp, err)
	if err != nil {
		return nil, fmt.Errorf("github POST %s: %w", graphqlPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Mirror the REST path's rate-limit + auth shaping so the resolver
	// sees the same typed errors regardless of which GitHub surface the
	// query hit.
	if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining == "0" && resp.StatusCode == http.StatusForbidden {
		var resetAt int64
		if rs := resp.Header.Get("X-RateLimit-Reset"); rs != "" {
			if v, perr := strconv.ParseInt(rs, 10, 64); perr == nil {
				resetAt = v
			}
		}
		return nil, &ErrRateLimitedT{ResetAt: resetAt}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrNotAuthenticated
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read graphql body: %w", err)
	}

	// Non-2xx without a JSON body is a transport-level failure; bubble
	// it up. GitHub's GraphQL endpoint normally returns 200 even when
	// the query has errors (they go into `errors[]`), so any non-2xx
	// here is operationally interesting.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 4096 {
			msg = msg[:4096]
		}
		return nil, &httpError{
			Status:   resp.StatusCode,
			Message:  msg,
			Endpoint: graphqlPath,
		}
	}

	// Validate the response is JSON-shaped before handing back; a 200
	// with non-JSON content is a GitHub bug, but we should not crash
	// the resolver if it ever happens.
	if !json.Valid(raw) {
		return nil, fmt.Errorf("graphql: non-JSON response from %s", graphqlPath)
	}
	return json.RawMessage(raw), nil
}
