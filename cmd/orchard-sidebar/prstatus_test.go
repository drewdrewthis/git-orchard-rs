package main

import "testing"

// The PR status word and the three-section bucketing: the two pure functions
// that decide what a card SAYS about a session.

func strptr(s string) *string { return &s }

// The status word is a precedence ladder, not a set of independent flags: a
// merged PR is "merged" even with a failing rollup, conflicts outrank checks,
// and anything the rollup doesn't positively confirm is "unresolved".
func TestPRStatus(t *testing.T) {
	cases := []struct {
		name string
		pr   prInfo
		want string
	}{
		{"merged wins over everything", prInfo{State: "MERGED", Draft: true,
			ChecksRollup: "FAILURE", MergeStateStatus: "DIRTY"}, "merged"},
		{"closed", prInfo{State: "CLOSED"}, "closed"},
		{"draft outranks conflicts and checks", prInfo{State: "OPEN", Draft: true,
			MergeStateStatus: "DIRTY", ChecksRollup: "FAILURE"}, "draft"},
		{"dirty merge state is conflicts", prInfo{State: "OPEN",
			MergeStateStatus: "DIRTY", ChecksRollup: "SUCCESS"}, "conflicts"},
		{"failure", prInfo{State: "OPEN", ChecksRollup: "FAILURE"}, "failing"},
		{"error", prInfo{State: "OPEN", ChecksRollup: "ERROR"}, "failing"},
		{"timed out", prInfo{State: "OPEN", ChecksRollup: "TIMED_OUT"}, "failing"},
		{"action required", prInfo{State: "OPEN", ChecksRollup: "ACTION_REQUIRED"}, "failing"},
		{"pending is unresolved", prInfo{State: "OPEN", ChecksRollup: "PENDING"}, "unresolved"},
		{"empty rollup is unresolved, never green", prInfo{State: "OPEN",
			ReviewDecision: strptr("APPROVED")}, "unresolved"},
		{"unknown rollup enum is unresolved, never green", prInfo{State: "OPEN",
			ChecksRollup: "NEUTRAL", ReviewDecision: strptr("APPROVED")}, "unresolved"},
		{"success without approval is unresolved", prInfo{State: "OPEN",
			ChecksRollup: "SUCCESS"}, "unresolved"},
		{"changes requested is unresolved", prInfo{State: "OPEN", ChecksRollup: "SUCCESS",
			ReviewDecision: strptr("CHANGES_REQUESTED")}, "unresolved"},
		{"review required is unresolved", prInfo{State: "OPEN", ChecksRollup: "SUCCESS",
			ReviewDecision: strptr("REVIEW_REQUIRED")}, "unresolved"},
		{"success plus approved is green", prInfo{State: "OPEN", ChecksRollup: "SUCCESS",
			MergeStateStatus: "CLEAN", ReviewDecision: strptr("APPROVED")}, "green"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := prStatus(c.pr); got != c.want {
				t.Errorf("prStatus = %q, want %q", got, c.want)
			}
		})
	}
}

// rowBucket collapses five states to three classes for the state-dot colour
// and the Needs-attention badge (not the list order, which is last-attached):
// working and idle fold together because the spinner already says which is
// which, and only input/stalled count as "needs attention".
func TestRowBucketMapping(t *testing.T) {
	cases := []struct {
		name string
		r    row
		want bucket
	}{
		{"a question or permission prompt", row{state: "input", hooked: true}, bucketAttention},
		{"stopped mid-turn", row{state: "stalled", hooked: true}, bucketAttention},
		{"finished, nobody looking", row{state: "idle", hooked: true}, bucketDone},
		{"finished but you are attached", row{state: "idle", hooked: true, attached: true}, bucketRunning},
		{"idle we only inferred", row{state: "idle"}, bucketRunning},
		{"still working", row{state: "working", hooked: true}, bucketRunning},
		{"a plain shell", row{state: "shell"}, bucketRunning},
		{"no state at all", row{}, bucketRunning},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rowBucket(c.r); got != c.want {
				t.Errorf("rowBucket(%+v) = %v, want %v", c.r, got, c.want)
			}
		})
	}
}
