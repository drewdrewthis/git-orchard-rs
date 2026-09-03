package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// The words the pane puts on screen for one row: section titles, the model
// family, the branch line, the issue/PR references and the card's right-hand
// tag. One home, because the card, the git box and the section header must
// spell the same fact the same way.

// groupLabel is the section header shown once above each run of same-bucket
// rows (rows are already sorted by bucket, so runs are contiguous).
func groupLabel(b bucket) string {
	switch b {
	case bucketAttention:
		return "Needs attention"
	case bucketDone:
		return "Done"
	default:
		return "Sessions"
	}
}

// shortModel compresses "claude-opus-4-6" style ids to their family name.
func shortModel(id string) string {
	for _, fam := range []string{"fable", "opus", "sonnet", "haiku"} {
		if strings.Contains(id, fam) {
			return fam
		}
	}
	return id
}

// branchLine renders "🌿 branch ↑a ↓b" for the card.
func branchLine(r row) string {
	if r.branch == "" {
		return ""
	}
	s := "🌿 " + r.branch
	if r.ahead != nil && *r.ahead > 0 {
		s += fmt.Sprintf(" ↑%d", *r.ahead)
	}
	if r.behind != nil && *r.behind > 0 {
		s += fmt.Sprintf(" ↓%d", *r.behind)
	}
	return s
}

// issueRef and prRef are the one place the "issue#N" / "pr#M (status)" label
// formats live. The status word comes from prStatus, whose narrowest-green
// ladder is the false-green guard (see its comment).
func issueRef(n int) string { return fmt.Sprintf("issue#%d", n) }

func prRef(p prInfo) string { return fmt.Sprintf("pr#%d (%s)", p.Number, prStatus(p)) }

// dirLabel is the session's working directory (basename), repo slug fallback.
func dirLabel(r row) string {
	if r.cwd != "" {
		return filepath.Base(r.cwd)
	}
	return r.repo
}

// cardTag is the card's right-hand marginal: the issue or PR the session is
// for, falling back to the branch when it is tracking neither.
func cardTag(r row) string {
	switch {
	case r.issueNum > 0:
		return issueRef(r.issueNum)
	case r.pr != nil:
		// number only: the status word is long and the git box already
		// carries it in full for the selected card
		return fmt.Sprintf("pr#%d", r.pr.Number)
	case r.branch != "":
		return trunc(r.branch, 18)
	}
	return ""
}
