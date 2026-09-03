// Package release resolves, downloads and verifies orchard's own GitHub
// release artifacts.
//
// Two consumers share it: `orchard upgrade` (cmd/orchard-upgrade), which
// downloads the suite tarball and replaces the installed binaries, and
// `orchard shell` (cmd/orchard-shell), whose background update check only
// needs the latest tag name.
//
// Every entry point resolves its API root from ORCHARD_RELEASE_REPO, which
// accepts a fixture server URL as well as an owner/repo slug — that is what
// keeps the unit tests off the network (RULES T1).
//
// SHA256SUMS is fetched from the same origin as the tarball it checksums
// (GitHub, or whatever ORCHARD_RELEASE_REPO points at instead). Sums.Verify
// therefore only proves the downloaded bytes match what that origin served:
// it guards against corruption and truncation in transit, not against a
// compromised origin or a substituted release. Nothing here is signed —
// that is a follow-up, not something this package claims today.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// DefaultRepo is the repository releases are read from when
	// ORCHARD_RELEASE_REPO is unset.
	DefaultRepo = "drewdrewthis/orchardist"

	// DefaultAPI is GitHub's REST API root.
	DefaultAPI = "https://api.github.com"

	// RepoEnv overrides the release source. It takes either an "owner/repo"
	// slug (resolved against DefaultAPI) or a full base URL, which points the
	// client at a fixture server instead of GitHub. One variable rather than
	// two because a second knob would be a second source of truth for the
	// same question.
	RepoEnv = "ORCHARD_RELEASE_REPO"

	// AllowHTTPEnv opts a RepoEnv URL back in when it is neither https nor
	// loopback — a self-hosted mirror on a network already trusted, say.
	// Any value other than "1" leaves the rejection in place.
	AllowHTTPEnv = "ORCHARD_RELEASE_ALLOW_HTTP"
)

// Sentinel errors. Callers distinguish these with errors.Is; everything else
// is wrapped with %w from the failing operation (RULES R8).
var (
	// ErrNotFound means the release (or the repo) does not exist.
	ErrNotFound = errors.New("release not found")
	// ErrRateLimited means GitHub refused the unauthenticated request.
	ErrRateLimited = errors.New("github api rate limit exceeded")
	// ErrNoAsset means the release exists but carries no asset for this platform.
	ErrNoAsset = errors.New("release has no matching asset")
	// ErrInsecureURL means RepoEnv named a URL that is neither https nor
	// loopback, with no AllowHTTPEnv opt-in (see ResolveTarget).
	ErrInsecureURL = errors.New("insecure release repo URL")
	// ErrArchiveTooLarge means a suite tarball made ExtractBinaries read past
	// its decompressed byte budget (see MaxArchiveBytes).
	ErrArchiveTooLarge = errors.New("release archive exceeds the decompressed size limit")
)

// Asset is one file attached to a GitHub release.
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
	Size        int64  `json:"size"`
}

// Release is the subset of GitHub's release JSON orchard reads.
type Release struct {
	TagName    string  `json:"tag_name"`
	Name       string  `json:"name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

// Version strips the tag's leading "v" so it can be compared against the
// version baked into a binary by -ldflags.
func (r *Release) Version() string { return strings.TrimPrefix(r.TagName, "v") }

// Asset returns the named asset. Names are exact — asset naming is a release
// contract (see AssetName), not a fuzzy match.
func (r *Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Client reads releases from a GitHub-compatible REST API.
type Client struct {
	HTTP *http.Client
	API  string // API root, no trailing slash
	Repo string // owner/repo

	// resolveErr is set by NewClient when RepoEnv fails ResolveTarget's
	// scheme check. Every request-issuing method returns it before building
	// a request, so a rejected URL fails closed instead of silently falling
	// back to a different endpoint than the one configured.
	resolveErr error
}

// NewClient returns a client configured from ORCHARD_RELEASE_REPO. Errors
// resolving that value are not returned here — Client's construction stays
// infallible — but surface from the first call to Latest, ByTag, Fetch, or
// Download instead, wrapping ErrInsecureURL.
func NewClient() *Client {
	api, repo, err := ResolveTarget(os.Getenv(RepoEnv))
	return &Client{
		HTTP:       &http.Client{Timeout: 30 * time.Second},
		API:        api,
		Repo:       repo,
		resolveErr: err,
	}
}

// ResolveTarget splits ORCHARD_RELEASE_REPO into an API root and a repo
// slug. A value that parses as an absolute URL is a fixture (or enterprise)
// API root; anything else is an owner/repo slug against GitHub.
//
// A URL root is rejected — api and repo come back empty, and err wraps
// ErrInsecureURL — unless it is https, its host is loopback (127.0.0.1,
// localhost, ::1), or AllowHTTPEnv opts back in. RepoEnv reaches the network
// unauthenticated, so a plain http:// root on a routable host would carry
// that traffic in the clear.
func ResolveTarget(v string) (api, repo string, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultAPI, DefaultRepo, nil
	}
	if u, perr := url.Parse(v); perr == nil && u.IsAbs() && u.Host != "" {
		if !strings.EqualFold(u.Scheme, "https") && !isLoopbackHost(u.Hostname()) && os.Getenv(AllowHTTPEnv) != "1" {
			return "", "", fmt.Errorf("%s=%s uses scheme %q on non-loopback host %q; set %s=1 to allow it: %w",
				RepoEnv, v, u.Scheme, u.Hostname(), AllowHTTPEnv, ErrInsecureURL)
		}
		return strings.TrimSuffix(v, "/"), DefaultRepo, nil
	}
	return DefaultAPI, strings.Trim(v, "/"), nil
}

// isLoopbackHost reports whether host — a URL's already-unbracketed
// Hostname() — names the local machine only.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Latest returns the repository's latest published (non-draft) release.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	return c.release(ctx, fmt.Sprintf("%s/repos/%s/releases/latest", c.API, c.Repo))
}

// ByTag returns one release by its exact tag (e.g. "v1.2.3"), which is how
// `orchard upgrade --version` pins — including to an older version.
func (c *Client) ByTag(ctx context.Context, tag string) (*Release, error) {
	if tag == "" {
		return nil, fmt.Errorf("release tag is empty")
	}
	return c.release(ctx, fmt.Sprintf("%s/repos/%s/releases/tags/%s", c.API, c.Repo, url.PathEscape(tag)))
}

func (c *Client) release(ctx context.Context, endpoint string) (*Release, error) {
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var rel Release
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("decode release from %s: %w", endpoint, err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("%s returned a release with no tag_name: %w", endpoint, ErrNotFound)
	}
	return &rel, nil
}

// Fetch returns the bytes at url — used for small assets (SHA256SUMS).
func (c *Client) Fetch(ctx context.Context, endpoint string) ([]byte, error) {
	return c.get(ctx, endpoint)
}

// Download streams url into w. Returns the number of bytes written.
func (c *Client) Download(ctx context.Context, endpoint string, w io.Writer) (int64, error) {
	req, err := c.request(ctx, endpoint, "application/octet-stream")
	if err != nil {
		return 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if err := statusError(endpoint, resp); err != nil {
		return 0, err
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, fmt.Errorf("read %s: %w", endpoint, err)
	}
	return n, nil
}

func (c *Client) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := c.request(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if err := statusError(endpoint, resp); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", endpoint, err)
	}
	return body, nil
}

func (c *Client) request(ctx context.Context, endpoint, accept string) (*http.Request, error) {
	if c.resolveErr != nil {
		return nil, c.resolveErr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", endpoint, err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "orchard-release-client")
	// Unauthenticated by default (60 requests/hour/IP). A token is read from
	// the environment only if the user already has one exported for gh(1);
	// orchard never asks for or stores credentials of its own.
	if tok := firstEnv("ORCHARD_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req, nil
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func statusError(endpoint string, resp *http.Response) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("GET %s: %w", endpoint, ErrNotFound)
	case resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		return fmt.Errorf("GET %s: %w", endpoint, ErrRateLimited)
	default:
		return fmt.Errorf("GET %s: unexpected status %s", endpoint, resp.Status)
	}
}
