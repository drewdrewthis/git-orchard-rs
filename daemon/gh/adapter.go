// adapter.go — external-world I/O for the gh domain.
//
// This file owns the GitHub HTTP client, all REST endpoint calls,
// the GraphQL POST, and pagination logic. It is internal to the
// gh package — consumers interact only with Service (R2).
//
// L4: no script exec on this read path. All reads are in-process
// net/http against api.github.com (or GH_API_BASE_URL for GHES).
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIBaseURL is the github.com REST endpoint.
const DefaultAPIBaseURL = "https://api.github.com"

// EnvAPIBaseURL is the environment variable that overrides DefaultAPIBaseURL.
// Tests use this with httptest.NewTLSServer; GHES users set it in their shell.
const EnvAPIBaseURL = "GH_API_BASE_URL"

const defaultPerPage = 100

// MaxPages is the safety cap on paginated list endpoints.
// With defaultPerPage=100 this yields 1000 items per logical list call.
const MaxPages = 10

const graphqlPath = "/graphql"

// Client is the GitHub HTTPS client. Stateless w.r.t. callers — every
// method takes a context and returns parsed data or an error. The bearer
// token is captured at construction time.
//
// Per ADR-011 §12: "ten endpoints, not a heavy library".
// Hand-rolled net/http with json.Decoder; no go-github, no oauth2.
type Client struct {
	BaseURL   string
	Token     string
	HTTP      *http.Client
	UserAgent string

	// Logger receives the one Info line per outbound call (issue #749).
	// The Provider wires its own logger here; a nil Logger falls back to
	// slog.Default() rather than dropping the audit trail silently.
	Logger *slog.Logger

	MaxPagesOverride int
}

// NewClient constructs a Client with sane defaults.
func NewClient(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultAPIBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		BaseURL:   baseURL,
		Token:     token,
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		UserAgent: "orchard/v1",
	}
}

// SplitRepo splits an "owner/name" string. Returns an error on malformed input.
func SplitRepo(repo string) (owner, name string, err error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Contains(parts[1], "/") {
		return "", "", fmt.Errorf("malformed repo %q: want owner/name", repo)
	}
	return parts[0], parts[1], nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) maxPages() int {
	if c.MaxPagesOverride > 0 {
		return c.MaxPagesOverride
	}
	return MaxPages
}

// do performs an authenticated GET and decodes the JSON body into out.
func (c *Client) do(ctx context.Context, path string, q url.Values, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil {
		return errors.New("gh client not initialised")
	}

	full := c.BaseURL + path
	if len(q) > 0 {
		full += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if ua := c.UserAgent; ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	start := time.Now()
	resp, err := c.httpClient().Do(req)
	// Logged before any status branching so a 403/404/rate-limited response
	// counts exactly like a 200 — each consumed one attempt (#749).
	c.logCall(callKindREST, path, repoFromRESTPath(path), 0, start, resp, err)
	if err != nil {
		return fmt.Errorf("github GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

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

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s body: %w", path, err)
	}
	return nil
}

// doListPaginated walks GitHub's Link: rel="next" chain for top-level JSON arrays.
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

// doEnvelopePaginated walks paginated envelope-shaped responses.
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

// walkPaginated drives the pagination loop, calling decode on each page body.
func walkPaginated(ctx context.Context, c *Client, firstURL, logPath string, decode func(io.Reader) error) error {
	current := firstURL
	for page := 0; page < c.maxPages(); page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return fmt.Errorf("build paginated request: %w", err)
		}
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if ua := c.UserAgent; ua != "" {
			req.Header.Set("User-Agent", ua)
		}

		start := time.Now()
		resp, err := c.httpClient().Do(req)
		// One line per page: every page is a separate request against the same
		// rate-limit budget, so collapsing them would under-report volume (#749).
		c.logCall(callKindRESTPage, logPath, repoFromRESTPath(logPath), page+1, start, resp, err)
		if err != nil {
			return fmt.Errorf("github GET %s: %w", logPath, err)
		}

		if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining == "0" && resp.StatusCode == http.StatusForbidden {
			var resetAt int64
			if rs := resp.Header.Get("X-RateLimit-Reset"); rs != "" {
				if v, perr := strconv.ParseInt(rs, 10, 64); perr == nil {
					resetAt = v
				}
			}
			_ = resp.Body.Close()
			return &ErrRateLimitedT{ResetAt: resetAt}
		}
		if resp.StatusCode == http.StatusUnauthorized {
			_ = resp.Body.Close()
			return ErrNotAuthenticated
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return &httpError{
				Status:   resp.StatusCode,
				Message:  strings.TrimSpace(string(body)),
				Endpoint: logPath,
			}
		}

		if err := decode(resp.Body); err != nil {
			_ = resp.Body.Close()
			return err
		}
		_ = resp.Body.Close()

		next := parseNextLink(resp.Header.Get("Link"))
		if next == "" {
			return nil
		}
		current = next
	}
	return nil
}

// parseNextLink extracts the `rel="next"` URL from a GitHub Link header.
func parseNextLink(header string) string {
	if header == "" {
		return ""
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		segments := strings.Split(part, ";")
		if len(segments) < 2 {
			continue
		}
		urlPart := strings.TrimSpace(segments[0])
		relPart := strings.TrimSpace(segments[1])
		if strings.Contains(relPart, `rel="next"`) {
			urlPart = strings.TrimPrefix(urlPart, "<")
			urlPart = strings.TrimSuffix(urlPart, ">")
			return urlPart
		}
	}
	return ""
}

// GraphQL POSTs an arbitrary GraphQL query to GitHub's API and returns the
// full JSON envelope verbatim (data, errors, extensions).
// Not cached — query strings are arbitrary (S16b).
func (c *Client) GraphQL(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	return c.GraphQLWithHeaders(ctx, query, variables, nil)
}

// GraphQLWithHeaders is GraphQL but lets the caller attach extra request
// headers — required for GitHub's preview-gated fields.
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

	if !json.Valid(raw) {
		return nil, fmt.Errorf("graphql: non-JSON response from %s", graphqlPath)
	}
	return json.RawMessage(raw), nil
}

// --- Endpoint methods (the ten endpoints from ADR-011 §12) ---

// ListPulls fetches the list of pull requests. State must be OPEN, CLOSED, MERGED, or ALL.
// Pagination walks Link rel="next" up to MaxPages.
func (c *Client) ListPulls(ctx context.Context, owner, name string, state PullRequestState) ([]PullRequest, error) {
	q := url.Values{}
	q.Set("per_page", strconv.Itoa(defaultPerPage))
	switch state {
	case PullRequestStateOpen, "":
		q.Set("state", "open")
	case PullRequestStateClosed, PullRequestStateMerged:
		q.Set("state", "closed")
	case PullRequestStateAll:
		q.Set("state", "all")
	default:
		return nil, fmt.Errorf("unknown PullRequestState %q", state)
	}
	q.Set("sort", "updated")
	q.Set("direction", "desc")

	var items []listPullRequestsRawItem
	if err := doListPaginated(ctx, c, fmt.Sprintf("/repos/%s/%s/pulls", owner, name), q, &items); err != nil {
		return nil, err
	}
	prs := listPullRequestsRaw(items).toPullRequests(owner, name)
	if state == PullRequestStateMerged {
		filtered := prs[:0]
		for _, p := range prs {
			if p.State == PullRequestStateMerged {
				filtered = append(filtered, p)
			}
		}
		return filtered, nil
	}
	if state == PullRequestStateClosed {
		filtered := prs[:0]
		for _, p := range prs {
			if p.State == PullRequestStateClosed {
				filtered = append(filtered, p)
			}
		}
		return filtered, nil
	}
	return prs, nil
}

// GetPull fetches one pull request by number.
func (c *Client) GetPull(ctx context.Context, owner, name string, number int) (PullRequest, error) {
	var raw struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		State     string `json:"state"`
		Draft     bool   `json:"draft"`
		HTMLURL   string `json:"html_url"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
		MergedAt  string `json:"merged_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := c.do(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, name, number), nil, &raw); err != nil {
		return PullRequest{}, err
	}
	state := PullRequestStateOpen
	if raw.State == "closed" {
		if raw.MergedAt != "" {
			state = PullRequestStateMerged
		} else {
			state = PullRequestStateClosed
		}
	}
	return PullRequest{
		RepoOwner:   owner,
		RepoName:    name,
		Number:      raw.Number,
		Title:       raw.Title,
		Body:        raw.Body,
		State:       state,
		Draft:       raw.Draft,
		AuthorLogin: raw.User.Login,
		BaseRef:     raw.Base.Ref,
		HeadRef:     raw.Head.Ref,
		URL:         raw.HTMLURL,
		CreatedAt:   raw.CreatedAt,
		UpdatedAt:   raw.UpdatedAt,
	}, nil
}

// ListIssues fetches the list of issues. PRs are filtered out.
func (c *Client) ListIssues(ctx context.Context, owner, name string, state IssueState) ([]Issue, error) {
	q := url.Values{}
	q.Set("per_page", strconv.Itoa(defaultPerPage))
	switch state {
	case IssueStateOpen, "":
		q.Set("state", "open")
	case IssueStateClosed:
		q.Set("state", "closed")
	case IssueStateAll:
		q.Set("state", "all")
	default:
		return nil, fmt.Errorf("unknown IssueState %q", state)
	}

	var items []listIssuesRawItem
	if err := doListPaginated(ctx, c, fmt.Sprintf("/repos/%s/%s/issues", owner, name), q, &items); err != nil {
		return nil, err
	}
	return listIssuesRaw(items).toIssues(owner, name), nil
}

// GetIssue fetches one issue by number.
func (c *Client) GetIssue(ctx context.Context, owner, name string, number int) (Issue, error) {
	var raw struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		State       string `json:"state"`
		HTMLURL     string `json:"html_url"`
		CreatedAt   string `json:"created_at"`
		UpdatedAt   string `json:"updated_at"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []restLabelRaw `json:"labels"`
	}
	if err := c.do(ctx, fmt.Sprintf("/repos/%s/%s/issues/%d", owner, name, number), nil, &raw); err != nil {
		return Issue{}, err
	}
	if raw.PullRequest != nil {
		return Issue{}, fmt.Errorf("issue %s/%s#%d is a pull request — use GetPull instead", owner, name, number)
	}
	state := IssueStateOpen
	if raw.State == "closed" {
		state = IssueStateClosed
	}
	return Issue{
		RepoOwner:   owner,
		RepoName:    name,
		Number:      raw.Number,
		Title:       raw.Title,
		Body:        raw.Body,
		State:       state,
		AuthorLogin: raw.User.Login,
		URL:         raw.HTMLURL,
		CreatedAt:   raw.CreatedAt,
		UpdatedAt:   raw.UpdatedAt,
		Labels:      restLabelsToLabels(raw.Labels),
	}, nil
}

// ListWorkflowRuns fetches workflow runs for a repo.
func (c *Client) ListWorkflowRuns(ctx context.Context, owner, name string) ([]WorkflowRun, error) {
	q := url.Values{}
	q.Set("per_page", strconv.Itoa(defaultPerPage))
	var accum listWorkflowRunsRaw
	decode := func(body io.Reader) error {
		var page listWorkflowRunsRaw
		if err := json.NewDecoder(body).Decode(&page); err != nil {
			return fmt.Errorf("decode /repos/%s/%s/actions/runs body: %w", owner, name, err)
		}
		accum.WorkflowRuns = append(accum.WorkflowRuns, page.WorkflowRuns...)
		return nil
	}
	if err := doEnvelopePaginated(ctx, c, fmt.Sprintf("/repos/%s/%s/actions/runs", owner, name), q, decode); err != nil {
		return nil, err
	}
	return accum.toRuns(owner, name), nil
}

// GetWorkflowRun fetches one workflow run by id.
func (c *Client) GetWorkflowRun(ctx context.Context, owner, name string, runID int64) (WorkflowRun, error) {
	var raw struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Path       string `json:"path"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
		HTMLURL    string `json:"html_url"`
		CreatedAt  string `json:"created_at"`
		UpdatedAt  string `json:"updated_at"`
	}
	if err := c.do(ctx, fmt.Sprintf("/repos/%s/%s/actions/runs/%d", owner, name, runID), nil, &raw); err != nil {
		return WorkflowRun{}, err
	}
	return WorkflowRun{
		RepoOwner:    owner,
		RepoName:     name,
		RunID:        raw.ID,
		Name:         raw.Name,
		WorkflowPath: raw.Path,
		Status:       raw.Status,
		Conclusion:   raw.Conclusion,
		HeadBranch:   raw.HeadBranch,
		HeadSHA:      raw.HeadSHA,
		URL:          raw.HTMLURL,
		CreatedAt:    raw.CreatedAt,
		UpdatedAt:    raw.UpdatedAt,
	}, nil
}

// ListPullReviews fetches reviews on one pull request.
func (c *Client) ListPullReviews(ctx context.Context, owner, name string, number int) ([]PullRequestReview, error) {
	q := url.Values{}
	q.Set("per_page", strconv.Itoa(defaultPerPage))
	var items []listReviewsRawItem
	if err := doListPaginated(ctx, c, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, name, number), q, &items); err != nil {
		return nil, err
	}
	return listReviewsRaw(items).toReviews(), nil
}

// ListPullComments fetches conversation comments on a pull request.
func (c *Client) ListPullComments(ctx context.Context, owner, name string, number int) ([]IssueComment, error) {
	return c.listIssueLikeComments(ctx, owner, name, number)
}

// ListIssueComments fetches comments on a real issue.
func (c *Client) ListIssueComments(ctx context.Context, owner, name string, number int) ([]IssueComment, error) {
	return c.listIssueLikeComments(ctx, owner, name, number)
}

func (c *Client) listIssueLikeComments(ctx context.Context, owner, name string, number int) ([]IssueComment, error) {
	q := url.Values{}
	q.Set("per_page", strconv.Itoa(defaultPerPage))
	var items []listCommentsRawItem
	if err := doListPaginated(ctx, c, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, name, number), q, &items); err != nil {
		return nil, err
	}
	return listCommentsRaw(items).toComments(), nil
}

// GetRepo fetches repository metadata.
func (c *Client) GetRepo(ctx context.Context, owner, name string) (Repository, error) {
	var raw getRepoRaw
	if err := c.do(ctx, fmt.Sprintf("/repos/%s/%s", owner, name), nil, &raw); err != nil {
		return Repository{}, err
	}
	return raw.toRepository(owner, name), nil
}

// --- Wire shapes (private to this package) ---

type listPullRequestsRawItem struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"`
	Draft     bool   `json:"draft"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	MergedAt  string `json:"merged_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

type listPullRequestsRaw []listPullRequestsRawItem

func (raws listPullRequestsRaw) toPullRequests(owner, name string) []PullRequest {
	out := make([]PullRequest, 0, len(raws))
	for _, r := range raws {
		state := PullRequestStateOpen
		if r.State == "closed" {
			if r.MergedAt != "" {
				state = PullRequestStateMerged
			} else {
				state = PullRequestStateClosed
			}
		}
		out = append(out, PullRequest{
			RepoOwner:   owner,
			RepoName:    name,
			Number:      r.Number,
			Title:       r.Title,
			Body:        r.Body,
			State:       state,
			Draft:       r.Draft,
			AuthorLogin: r.User.Login,
			BaseRef:     r.Base.Ref,
			HeadRef:     r.Head.Ref,
			URL:         r.HTMLURL,
			CreatedAt:   r.CreatedAt,
			UpdatedAt:   r.UpdatedAt,
		})
	}
	return out
}

type listIssuesRawItem struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	State       string `json:"state"`
	HTMLURL     string `json:"html_url"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []restLabelRaw `json:"labels"`
}

type listIssuesRaw []listIssuesRawItem

type restLabelRaw struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func (raws listIssuesRaw) toIssues(owner, name string) []Issue {
	out := make([]Issue, 0, len(raws))
	for _, r := range raws {
		if r.PullRequest != nil {
			continue
		}
		state := IssueStateOpen
		if r.State == "closed" {
			state = IssueStateClosed
		}
		out = append(out, Issue{
			RepoOwner:   owner,
			RepoName:    name,
			Number:      r.Number,
			Title:       r.Title,
			Body:        r.Body,
			State:       state,
			AuthorLogin: r.User.Login,
			URL:         r.HTMLURL,
			CreatedAt:   r.CreatedAt,
			UpdatedAt:   r.UpdatedAt,
			Labels:      restLabelsToLabels(r.Labels),
		})
	}
	return out
}

func restLabelsToLabels(raws []restLabelRaw) []Label {
	out := make([]Label, 0, len(raws))
	for _, l := range raws {
		if _, isPhase := phaseLabels[l.Name]; isPhase {
			continue
		}
		out = append(out, Label{
			Name:        l.Name,
			Color:       l.Color,
			Description: l.Description,
		})
	}
	return out
}

type listWorkflowRunsRaw struct {
	WorkflowRuns []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Path       string `json:"path"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
		HTMLURL    string `json:"html_url"`
		CreatedAt  string `json:"created_at"`
		UpdatedAt  string `json:"updated_at"`
	} `json:"workflow_runs"`
}

func (raws listWorkflowRunsRaw) toRuns(owner, name string) []WorkflowRun {
	out := make([]WorkflowRun, 0, len(raws.WorkflowRuns))
	for _, r := range raws.WorkflowRuns {
		out = append(out, WorkflowRun{
			RepoOwner:    owner,
			RepoName:     name,
			RunID:        r.ID,
			Name:         r.Name,
			WorkflowPath: r.Path,
			Status:       r.Status,
			Conclusion:   r.Conclusion,
			HeadBranch:   r.HeadBranch,
			HeadSHA:      r.HeadSHA,
			URL:          r.HTMLURL,
			CreatedAt:    r.CreatedAt,
			UpdatedAt:    r.UpdatedAt,
		})
	}
	return out
}

type listReviewsRawItem struct {
	ID          int64  `json:"id"`
	State       string `json:"state"`
	Body        string `json:"body"`
	SubmittedAt string `json:"submitted_at"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

type listReviewsRaw []listReviewsRawItem

func (raws listReviewsRaw) toReviews() []PullRequestReview {
	out := make([]PullRequestReview, 0, len(raws))
	for _, r := range raws {
		out = append(out, PullRequestReview{
			GitHubID:    r.ID,
			AuthorLogin: r.User.Login,
			State:       r.State,
			Body:        r.Body,
			SubmittedAt: r.SubmittedAt,
		})
	}
	return out
}

type listCommentsRawItem struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

type listCommentsRaw []listCommentsRawItem

func (raws listCommentsRaw) toComments() []IssueComment {
	out := make([]IssueComment, 0, len(raws))
	for _, c := range raws {
		out = append(out, IssueComment{
			GitHubID:    c.ID,
			AuthorLogin: c.User.Login,
			Body:        c.Body,
			CreatedAt:   c.CreatedAt,
			UpdatedAt:   c.UpdatedAt,
		})
	}
	return out
}

type getRepoRaw struct {
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	Fork          bool   `json:"fork"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
	UpdatedAt     string `json:"updated_at"`
}

func (r getRepoRaw) toRepository(owner, name string) Repository {
	return Repository{
		Owner:         owner,
		Name:          name,
		Description:   r.Description,
		Private:       r.Private,
		Fork:          r.Fork,
		Archived:      r.Archived,
		DefaultBranch: r.DefaultBranch,
		URL:           r.HTMLURL,
		UpdatedAt:     r.UpdatedAt,
	}
}
