package gh

import "time"

// export_test.go exposes package-private helpers for the external test package
// (package gh_test). This file is only compiled during testing.

// ExportMapStatusCheckRollup wraps mapStatusCheckRollup for external tests.
func ExportMapStatusCheckRollup(state string) CiStatus {
	return mapStatusCheckRollup(state)
}

// ExportFilterPhaseLabels wraps filterPhaseLabels for external tests.
// Accepts label names as a slice of strings (the historical shape) and
// converts to []Label internally, then projects the filtered result
// back down to a []string of names so existing tests stay terse.
func ExportFilterPhaseLabels(in []string) []string {
	labels := make([]Label, 0, len(in))
	for _, n := range in {
		labels = append(labels, Label{Name: n})
	}
	filtered := filterPhaseLabels(labels)
	out := make([]string, 0, len(filtered))
	for _, l := range filtered {
		out = append(out, l.Name)
	}
	return out
}

// ExportEnrichTimestamp returns the recorded enrichment timestamp for a
// given PR key, or the zero time if EnrichPullRequest never wrote one.
// External tests use this to directly assert the UNKNOWN-not-cached
// invariant rather than inferring it from HTTP call counts.
func (p *Provider) ExportEnrichTimestamp(key PullRequestKey) time.Time {
	p.prMu.RLock()
	defer p.prMu.RUnlock()
	return p.enrichAt[key]
}

// ExportParseLinkNext wraps parseLinkNext so external tests can assert
// the Link-header parser without touching the package-private symbol.
func ExportParseLinkNext(header string) string {
	return parseLinkNext(header)
}

// ExportSeedPRState seeds the per-key PR cache with a known lifecycle state,
// exactly as a ListPullRequests REST refresh would. External tests use this to
// exercise the terminal-vs-transient UNKNOWN distinction without standing up a
// full REST list fixture.
func (p *Provider) ExportSeedPRState(key PullRequestKey, state PullRequestState) {
	p.prMu.Lock()
	defer p.prMu.Unlock()
	e := p.prs[key]
	e.value.State = state
	p.prs[key] = e
}
