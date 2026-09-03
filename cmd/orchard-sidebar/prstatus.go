package main

// prStatus collapses the PR's several GitHub enums into the one word that most
// needs acting on, worst-first: merged, closed, draft, conflicts, failing,
// unresolved, green.
//
// "green" is deliberately the narrowest branch — only a literal SUCCESS rollup
// with an APPROVED review earns it. Anything the rollup doesn't positively
// confirm (PENDING, EXPECTED, empty, an enum we don't know) reads as
// "unresolved", never as green: see sol.2026-07-14-daemon-checkrollup-reports-
// false-green, where a rollup-derived verdict showed failing checks as clean.
func prStatus(p prInfo) string {
	switch p.State {
	case "MERGED":
		return "merged"
	case "CLOSED":
		return "closed"
	}
	if p.Draft {
		return "draft"
	}
	if p.MergeStateStatus == "DIRTY" {
		return "conflicts"
	}
	switch p.ChecksRollup {
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED":
		return "failing"
	case "SUCCESS":
		// checks are in; the review is what's left
		if p.ReviewDecision != nil && *p.ReviewDecision == "APPROVED" {
			return "green"
		}
		return "unresolved"
	}
	return "unresolved"
}
