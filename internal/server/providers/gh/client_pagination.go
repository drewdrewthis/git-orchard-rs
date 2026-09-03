package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxPages is the safety cap on paginated list endpoints. With
// defaultPerPage = 100 this yields 1000 items per logical list call.
// Repos with more results than that need a different access pattern
// (cursor-based GraphQL, scoped sub-queries); silently returning a
// truncated slice is preferable to an unbounded fetch that could
// stall the daemon or exhaust the rate-limit window.
//
// The cap is exported so tests can stub a smaller value via the
// `WithMaxPages` override on Client.
const MaxPages = 10

// doListPaginated is the paginating sibling of `do` for top-level JSON
// array responses. It walks GitHub's `Link: <…>; rel="next"` chain up
// to MaxPages and appends each page's decoded items into accum.
//
// `T` is the element type (e.g. one of the anonymous structs that
// listPullRequestsRaw / listIssuesRaw expand into); `accum` receives
// every decoded page concatenated in order.
//
// Per-page errors short-circuit the loop. Rate-limit and 401 errors
// surface as the typed sentinels the single-page `do` returns.
func doListPaginated[T any](ctx context.Context, c *Client, path string, q url.Values, accum *[]T) error {
	if c == nil {
		return errors.New("gh client not initialised")
	}
	first := c.BaseURL + path
	if len(q) > 0 {
		first += "?" + q.Encode()
	}
	return walkPaginated(ctx, c, first, path, func(body io.Reader) error {
		var page []T
		if err := json.NewDecoder(body).Decode(&page); err != nil {
			return fmt.Errorf("decode %s body: %w", path, err)
		}
		*accum = append(*accum, page...)
		return nil
	})
}

// doEnvelopePaginated is the paginating sibling of `do` for
// envelope-shaped list responses (e.g. `{ "workflow_runs": [...] }`).
// Each page's body is decoded via the caller's `decode` closure, which
// is responsible for extracting and appending the items into its own
// accumulator. Loop control and safety cap stay here.
func doEnvelopePaginated(ctx context.Context, c *Client, path string, q url.Values, decode func(io.Reader) error) error {
	if c == nil {
		return errors.New("gh client not initialised")
	}
	first := c.BaseURL + path
	if len(q) > 0 {
		first += "?" + q.Encode()
	}
	return walkPaginated(ctx, c, first, path, decode)
}

// walkPaginated drives the paged GET loop. nextURL is the resolved
// absolute URL for each iteration (Link headers from GitHub are
// already absolute). `relPath` is the slash-prefixed REST path retained
// for error wrapping so callers see the original endpoint, not the
// full URL.
func walkPaginated(ctx context.Context, c *Client, nextURL, relPath string, decode func(io.Reader) error) error {
	cap := c.effectiveMaxPages()
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		applyRESTHeaders(c, req)

		start := time.Now()
		resp, err := c.httpClient().Do(req)
		// One line per page: every page is a separate request against the same
		// rate-limit budget, so collapsing them would under-report volume (#749).
		c.logCall(callKindRESTPage, relPath, repoFromRESTPath(relPath), page, start, resp, err)
		if err != nil {
			return fmt.Errorf("github GET %s: %w", relPath, err)
		}

		if err := checkRESTStatus(resp, relPath); err != nil {
			_ = resp.Body.Close()
			return err
		}

		if err := decode(resp.Body); err != nil {
			_ = resp.Body.Close()
			return err
		}
		link := resp.Header.Get("Link")
		_ = resp.Body.Close()

		next := parseLinkNext(link)
		if next == "" {
			return nil
		}
		if page >= cap {
			return nil
		}
		nextURL = next
	}
}

// applyRESTHeaders attaches the standard GitHub REST headers a
// list-endpoint request expects. Factored out so the paginating loop
// and the single-call `do` stay in step on auth / API version.
func applyRESTHeaders(c *Client, req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if ua := c.UserAgent; ua != "" {
		req.Header.Set("User-Agent", ua)
	}
}

// checkRESTStatus mirrors the rate-limit / auth / non-2xx handling in
// `Client.do`. Body is left open for the caller to decode on success.
func checkRESTStatus(resp *http.Response, path string) error {
	if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining == "0" && resp.StatusCode == http.StatusForbidden {
		var resetAt int64
		if rs := resp.Header.Get("X-RateLimit-Reset"); rs != "" {
			if v, perr := strconv.ParseInt(rs, 10, 64); perr == nil {
				resetAt = v
			}
		}
		return &ErrRateLimitedT{ResetAt: resetAt}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrNotAuthenticated
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &httpError{
			Status:   resp.StatusCode,
			Message:  strings.TrimSpace(string(body)),
			Endpoint: path,
		}
	}
	return nil
}

// parseLinkNext extracts the URL for `rel="next"` from a GitHub Link
// header. Returns "" when there is no next page.
//
// Link header shape:
//
//	<https://api.github.com/.../pulls?page=2>; rel="next",
//	<https://api.github.com/.../pulls?page=5>; rel="last"
//
// The parser splits on commas (top-level — URLs never contain unescaped
// commas in GitHub responses), then on semicolons, and matches the
// `rel="next"` parameter.
func parseLinkNext(header string) string {
	if header == "" {
		return ""
	}
	for _, segment := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(segment), ";")
		if len(parts) < 2 {
			continue
		}
		urlPart := strings.TrimSpace(parts[0])
		if !strings.HasPrefix(urlPart, "<") || !strings.HasSuffix(urlPart, ">") {
			continue
		}
		for _, p := range parts[1:] {
			p = strings.TrimSpace(p)
			if p == `rel="next"` || p == `rel=next` {
				return strings.TrimSuffix(strings.TrimPrefix(urlPart, "<"), ">")
			}
		}
	}
	return ""
}

// effectiveMaxPages returns the configured per-client override (if
// set via test helpers) or the package-level MaxPages cap.
func (c *Client) effectiveMaxPages() int {
	if c == nil || c.MaxPagesOverride <= 0 {
		return MaxPages
	}
	return c.MaxPagesOverride
}
