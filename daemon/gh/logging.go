// logging.go — auditable observability for the gh domain (issue #749).
//
// Twin of internal/server/providers/gh/logging.go. The two gh packages are
// kept in step deliberately: this one is the R1 refactor destination, and an
// observability fix that lived in only one of them would silently disappear
// the day the daemon switches over.
//
// Two things are logged here and nowhere else:
//
//   - One Info line per OUTBOUND GitHub call, emitted at the HTTP boundary so
//     no endpoint helper can accidentally skip it. Info, not Debug: the daemon
//     runs at Info by default, and a rate-limit audit that has to be
//     reconstructed from source instead of observed call volume is exactly the
//     failure #749 reported.
//   - Warn lines for the backoff decisions (rate-limit cooldown, stale-serve).
//     These change what the daemon does for minutes at a time and were
//     previously invisible.
//
// O4: this is where cache/rate-limit attribution surfaces.

package gh

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Log messages are constants so operators (and tests) have one stable string
// to grep for per event class.
const (
	// callLogMessage marks one outbound GitHub API call. Counting these lines
	// over a window IS the call-volume audit.
	callLogMessage = "gh: api call"
	// cacheHitLogMessage marks a read served from the in-process cache — the
	// miss counterpart is a callLogMessage line (O4).
	cacheHitLogMessage = "gh: cache hit"
	// cooldownLogMessage marks the daemon deciding to stop calling GitHub.
	cooldownLogMessage = "gh: rate-limit cooldown engaged"
	// staleLogMessage marks the daemon answering from stale cache instead of
	// GitHub.
	staleLogMessage = "gh: serving stale enrichment"
	// staleBatchLogMessage is the batch-path counterpart. It summarises the
	// whole batch in one line rather than emitting one per PR — during a
	// throttle window the sidebar re-polls continuously, and per-PR lines
	// would bury the cooldown line that explains them.
	staleBatchLogMessage = "gh: serving stale enrichment (batch)"
)

// Call kinds distinguish the three outbound surfaces. They share one message
// so a single grep counts every call, and one attribute so a breakdown is a
// group-by rather than three greps.
const (
	callKindREST     = "rest"
	callKindRESTPage = "rest-page"
	callKindGraphQL  = "graphql"
)

// log returns the client's logger, or the process default when none was wired.
// Never nil — a Client built by a test that skipped the logger must not panic
// on its first request.
func (c *Client) log() *slog.Logger {
	if c == nil || c.Logger == nil {
		return slog.Default()
	}
	return c.Logger
}

// logCall emits the one Info line for a completed GitHub round trip.
//
// It is called immediately after the transport returns and BEFORE any
// status-code branching, so a rate-limited, unauthenticated, or 404 response
// is counted exactly like a successful one — each consumed an attempt.
//
// page is 1-based for paginated calls and 0 for single-shot ones (attribute
// omitted). resp may be nil when the round trip itself failed, in which case
// err carries the reason and the response-derived attributes are omitted
// rather than zero-filled: a fabricated rate_limit_remaining=0 would read as
// "quota exhausted" in an audit.
func (c *Client) logCall(kind, endpoint, repo string, page int, start time.Time, resp *http.Response, err error) {
	attrs := []any{
		slog.String("kind", kind),
		slog.String("endpoint", endpoint),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	}
	if repo != "" {
		attrs = append(attrs, slog.String("repo", repo))
	}
	if page > 0 {
		attrs = append(attrs, slog.Int("page", page))
	}
	if resp != nil {
		attrs = append(attrs, slog.Int("status", resp.StatusCode))
		if n, ok := rateLimitRemaining(resp); ok {
			attrs = append(attrs, slog.Int("rate_limit_remaining", n))
		}
	}
	if err != nil {
		attrs = append(attrs, slog.String("err", err.Error()))
	}
	c.log().Info(callLogMessage, attrs...)
}

// rateLimitRemaining reads X-RateLimit-Remaining. ok is false when the header
// is absent or unparseable, so callers can omit the attribute entirely.
func rateLimitRemaining(resp *http.Response) (int, bool) {
	raw := resp.Header.Get("X-RateLimit-Remaining")
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

// repoFromRESTPath projects a REST path onto "owner/name" for the call log.
// GitHub's repo-scoped paths all start `/repos/{owner}/{name}`; anything else
// (e.g. `/user`, `/rate_limit`) legitimately has no repo and yields "".
func repoFromRESTPath(path string) string {
	const prefix = "/repos/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// repoFromGraphQLVariables projects GraphQL query variables onto "owner/name".
// The GraphQL endpoint path is always /graphql, so the repo can only come from
// the variables. Batch enrichment inlines its repos in the query text and
// passes nil variables; that call logs with no repo attribute rather than a
// misleading one.
func repoFromGraphQLVariables(vars map[string]any) string {
	if vars == nil {
		return ""
	}
	owner, ownerOK := vars["owner"].(string)
	name, nameOK := vars["name"].(string)
	if !ownerOK || !nameOK || owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

// logCacheHit records a read answered without touching GitHub (O4).
func (p *Provider) logCacheHit(op, repo string) {
	p.logger.Info(cacheHitLogMessage,
		slog.String("op", op),
		slog.String("repo", repo),
	)
}

// enterRateLimitCooldown arms the rate-limit cooldown and says so at Warn.
//
// The cooldown suppresses every subsequent enrichment call for its duration,
// so the daemon looks stalled to anyone reading the logs at Info. site names
// the caller (EnrichPullRequest / BatchEnrichPullRequests) because the two
// paths back off independently.
func (p *Provider) enterRateLimitCooldown(site, reason string) {
	p.prMu.Lock()
	until := p.clock().Add(rateLimitCooldown)
	p.rateLimitedUntil = until
	p.prMu.Unlock()

	p.logger.Warn(cooldownLogMessage,
		slog.String("site", site),
		slog.String("until", until.Format(time.RFC3339)),
		slog.String("reason", reason),
	)
}

// clearRateLimitCooldown drops the cooldown after a clean response.
func (p *Provider) clearRateLimitCooldown() {
	p.prMu.Lock()
	p.rateLimitedUntil = time.Time{}
	p.prMu.Unlock()
}

// logServingStale records that a PR's enrichment came from cache rather than
// GitHub. Warn, not Debug: a sidebar showing minutes-old merge state is a
// degraded mode, and #749 exists because that degradation was invisible.
func (p *Provider) logServingStale(key PullRequestKey, reason error) {
	p.logger.Warn(staleLogMessage,
		slog.String("key", key.String()),
		slog.String("reason", errText(reason)),
	)
}

// logServingStaleBatch records one batch enrichment falling back to cache.
// served is how many of requested keys had a usable cached value; the rest
// came back empty, which is the difference between "degraded" and "blank".
func (p *Provider) logServingStaleBatch(served, requested int, reason error) {
	p.logger.Warn(staleBatchLogMessage,
		slog.Int("served", served),
		slog.Int("requested", requested),
		slog.String("reason", errText(reason)),
	)
}

// errText renders an error for a log attribute, tolerating nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
