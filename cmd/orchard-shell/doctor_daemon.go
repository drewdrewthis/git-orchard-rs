package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// daemonCheckTimeout is the daemon check's own budget, independent of
// doctorTimeout which bounds the whole suite.
const daemonCheckTimeout = 2 * time.Second

// checkDaemon POSTs {version} to the daemon's GraphQL endpoint.
//
// This builds its own minimal client rather than importing
// cmd/orchard-sidebar/graphql.go: that file's envelope handling (partial
// GraphQL errors, subscriptions) is more than a single reachability probe
// needs, and the two are separate binaries — package main cannot import
// another package main regardless.
func checkDaemon(ctx context.Context, url string) checkResult {
	const remedy = "systemctl --user start orchard"
	ctx, cancel := context.WithTimeout(ctx, daemonCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"query":"{version}"}`))
	if err != nil {
		return checkResult{ID: "daemon", Status: statusFail, Detail: err.Error(), Remedy: remedy}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return checkResult{ID: "daemon", Status: statusFail,
			Detail: fmt.Sprintf("daemon unreachable at %s: %v", url, err), Remedy: remedy}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return checkResult{ID: "daemon", Status: statusFail,
			Detail: fmt.Sprintf("daemon at %s returned HTTP %d", url, resp.StatusCode), Remedy: remedy}
	}

	var envelope struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil || envelope.Data.Version == "" {
		return checkResult{ID: "daemon", Status: statusFail,
			Detail: fmt.Sprintf("daemon at %s returned no version", url), Remedy: remedy}
	}
	return checkResult{ID: "daemon", Status: statusPass,
		Detail: fmt.Sprintf("daemon reachable at %s, version %s", url, envelope.Data.Version)}
}
