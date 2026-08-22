package gh

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WebhookHandler validates incoming GitHub webhook deliveries, parses
// the event, and pushes an InvalidationEvent into the provider's
// subscriber channel for the touched node.
//
// **Stub status (post-v1).** The brief calls for the *endpoint shape*
// only — the full webhook subscription / delivery loop lives in a
// future workstream. For now this handler:
//
//   - Validates the X-Hub-Signature-256 HMAC against a configured
//     secret (when the secret is non-empty; bypassed when empty so
//     the daemon stays usable in single-user dev mode).
//   - Decodes the minimum payload it needs to identify the touched
//     node (action + the {pull_request|issue|workflow_run} block).
//   - Drops the matching cache entry on the provider and broadcasts
//     an InvalidationEvent with the GraphQL node id.
//
// Anything beyond that — replay, dedup, durable delivery log — is
// outside scope. The webhook is a cache-invalidation side channel,
// not a mutation surface (ADR-011 §12).
type WebhookHandler struct {
	Provider *Provider
	// Secret is the HMAC-SHA256 secret. When empty, signature
	// validation is skipped (dev / test mode).
	Secret string
	// Clock injects time.Now for tests that assert event timestamps.
	Clock func() time.Time
}

// NewWebhookHandler constructs a handler bound to a Provider.
func NewWebhookHandler(p *Provider, secret string) *WebhookHandler {
	return &WebhookHandler{
		Provider: p,
		Secret:   secret,
		Clock:    time.Now,
	}
}

// ServeHTTP implements http.Handler. Mounted by the daemon at
// /webhook/github.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MiB cap
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

	if h.Secret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if !validSignature(sig, h.Secret, body) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		http.Error(w, "missing X-GitHub-Event header", http.StatusBadRequest)
		return
	}

	if h.Provider == nil {
		// Defensive — the handler shouldn't be reachable without a
		// provider, but this keeps it from panicking if it is.
		http.Error(w, "gh provider not configured", http.StatusServiceUnavailable)
		return
	}

	nodeID, reason, err := decodeAndInvalidate(h.Provider, event, body, clockOrNow(h.Clock))
	if err != nil {
		// Log to the provider's logger but reply 200 — GitHub retries
		// on non-2xx, and we don't want to spin on unknown events.
		h.Provider.logger.Warn("gh webhook: decode skipped", "event", event, "err", err)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"ignored"}`)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status":"ok","node_id":"%s","reason":"%s"}`, nodeID, reason)
}

// validSignature compares the X-Hub-Signature-256 header against the
// expected HMAC of body using secret. Returns true when the signature
// matches; false otherwise.
//
// GitHub's header format is `sha256=<hex>`. We use hmac.Equal for
// constant-time comparison.
func validSignature(header, secret string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	got := mac.Sum(nil)
	return hmac.Equal(want, got)
}

// decodeAndInvalidate extracts the touched node from the event payload
// and pushes an InvalidationEvent. Returns the node id + reason on
// success.
//
// Only the three nodes the gh provider surfaces are decoded; any other
// event (push, fork, star, ...) is treated as "out of scope" and
// returns an error so the handler can reply 202.
func decodeAndInvalidate(p *Provider, event string, body []byte, at time.Time) (string, string, error) {
	switch event {
	case "pull_request":
		var payload struct {
			PullRequest struct {
				Number int `json:"number"`
			} `json:"pull_request"`
			Repository struct {
				Owner struct {
					Login string `json:"login"`
				} `json:"owner"`
				Name string `json:"name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", "", fmt.Errorf("decode pull_request: %w", err)
		}
		owner := payload.Repository.Owner.Login
		name := payload.Repository.Name
		num := payload.PullRequest.Number
		if owner == "" || name == "" || num == 0 {
			return "", "", errors.New("missing repository or pull_request fields")
		}
		nodeID := prNodeID(PullRequestKey{Owner: owner, Name: name, Number: num})
		p.dropPRCache(PullRequestKey{Owner: owner, Name: name, Number: num})
		p.invalidate(nodeID, "webhook:pull_request", at)
		return nodeID, "webhook:pull_request", nil
	case "issues":
		var payload struct {
			Issue struct {
				Number int `json:"number"`
			} `json:"issue"`
			Repository struct {
				Owner struct {
					Login string `json:"login"`
				} `json:"owner"`
				Name string `json:"name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", "", fmt.Errorf("decode issues: %w", err)
		}
		owner := payload.Repository.Owner.Login
		name := payload.Repository.Name
		num := payload.Issue.Number
		if owner == "" || name == "" || num == 0 {
			return "", "", errors.New("missing repository or issue fields")
		}
		nodeID := fmt.Sprintf("Issue:%s/%s#%d", owner, name, num)
		p.dropIssueCache(IssueKey{Owner: owner, Name: name, Number: num})
		p.invalidate(nodeID, "webhook:issues", at)
		return nodeID, "webhook:issues", nil
	case "workflow_run":
		var payload struct {
			WorkflowRun struct {
				ID int64 `json:"id"`
			} `json:"workflow_run"`
			Repository struct {
				Owner struct {
					Login string `json:"login"`
				} `json:"owner"`
				Name string `json:"name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", "", fmt.Errorf("decode workflow_run: %w", err)
		}
		owner := payload.Repository.Owner.Login
		name := payload.Repository.Name
		runID := payload.WorkflowRun.ID
		if owner == "" || name == "" || runID == 0 {
			return "", "", errors.New("missing repository or workflow_run fields")
		}
		nodeID := fmt.Sprintf("WorkflowRun:%s/%s#%d", owner, name, runID)
		p.dropRunCache(WorkflowRunKey{Owner: owner, Name: name, RunID: runID})
		p.invalidate(nodeID, "webhook:workflow_run", at)
		return nodeID, "webhook:workflow_run", nil
	default:
		return "", "", fmt.Errorf("event %q not surfaced", event)
	}
}

func clockOrNow(c func() time.Time) time.Time {
	if c == nil {
		return time.Now()
	}
	return c()
}

// dropPRCache removes a PR's cached entries (the per-key entry plus
// any list cache for the same repo). Webhook-driven invalidation.
func (p *Provider) dropPRCache(k PullRequestKey) {
	p.prMu.Lock()
	delete(p.prs, k)
	delete(p.enrichAt, k)
	p.prMu.Unlock()
	p.listMu.Lock()
	for lk := range p.listPRsCache {
		if lk.Owner == k.Owner && lk.Name == k.Name {
			delete(p.listPRsCache, lk)
		}
	}
	p.listMu.Unlock()
}

func (p *Provider) dropIssueCache(k IssueKey) {
	p.issueMu.Lock()
	delete(p.issues, k)
	delete(p.issueDeps, k)
	p.issueMu.Unlock()
	p.listMu.Lock()
	for lk := range p.listIssCache {
		if lk.Owner == k.Owner && lk.Name == k.Name {
			delete(p.listIssCache, lk)
		}
	}
	p.listMu.Unlock()
}

func (p *Provider) dropRunCache(k WorkflowRunKey) {
	p.runMu.Lock()
	delete(p.runs, k)
	p.runMu.Unlock()
	p.listMu.Lock()
	for lk := range p.listRunCache {
		if lk.Owner == k.Owner && lk.Name == k.Name {
			delete(p.listRunCache, lk)
		}
	}
	p.listMu.Unlock()
}
