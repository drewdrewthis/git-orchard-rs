package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Synthetic rows, for exercising the list at a scale the machine doesn't
// currently have: ORCHARD_SIDEBAR_FAKE=30 appends 30 of them after whatever is
// real. Every field is derived from the row's index, so the same N always
// renders the same list — a scroll bug reproduces instead of shimmering.
//
// They are labelled twice over: every session name starts with "fake-", and the
// row carries fake=true so a click can never ask tmux to attach a session that
// was never created (selectRow checks it). Unset or 0 means none of this runs.
const fakeEnv = "ORCHARD_SIDEBAR_FAKE"

func fakeCount() int {
	n, err := strconv.Atoi(os.Getenv(fakeEnv))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

var (
	fakeNames    = []string{"payments", "auth", "search", "billing", "inbox", "webhooks", "cache", "uploads", "reports", "onboarding", "scheduler", "notifier"}
	fakeStates   = []string{"input", "working", "idle", "idle", "stalled", "working", "idle", "shell", "input", "idle"}
	fakeModels   = []string{"opus", "sonnet", "haiku", "sonnet", "opus", ""}
	fakeMissions = []string{
		"wire the retry budget into the queue consumer",
		"why does the nightly job double-charge?",
		"drop the legacy /v1 handlers",
		"",
		"cut the cold-start from 27s to under 5",
		"port the fixtures to the new golden format",
		"the flake only reproduces under -race",
	}
)

// fakeRows builds n synthetic rows. Index-derived and side-effect free, so the
// unit tests can build the same list the pane shows.
func fakeRows(n int) []row {
	out := make([]row, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("fake-%02d-%s", i+1, fakeNames[i%len(fakeNames)])
		state := fakeStates[i%len(fakeStates)]
		r := row{
			session: name,
			state:   state,
			fake:    true,
			// hooked: a synthetic row claims a state file so it renders like a
			// real one — the "fake-" prefix is the tag, not a "?" marker
			hooked:  state != "shell",
			mission: fakeMissions[i%len(fakeMissions)],
			model:   fakeModels[i%len(fakeModels)],
			cwd:     "/w/fake/" + name,
			repo:    "acme/fake",
			branch:  fmt.Sprintf("fake/%02d-%s", i+1, fakeNames[i%len(fakeNames)]),
			// old enough that a real session touched in the last few minutes
			// always sorts above the synthetic ones, spread out enough that
			// the age column varies down the list
			lastAct:  time.Now().Add(-time.Duration(i*7+11) * time.Minute),
			attached: false,
		}
		if i%3 == 0 {
			r.issueNum = 900 + i
			r.issueTitle = "synthetic " + fakeNames[i%len(fakeNames)]
		}
		if i%5 == 2 {
			num := 700 + i
			r.pr = &prInfo{Number: num, State: "OPEN", ChecksRollup: "SUCCESS", MergeStateStatus: "CLEAN"}
		}
		out = append(out, r)
	}
	return out
}

// appendFakes appends the synthetic rows, skipping any already present. It
// runs after applyHooks so the hook lane (which owns every real row's state)
// never rewrites a synthetic one, and before rebuild's single sort.
//
// The rows come from m.fakes, resolved once at startup: this runs on every
// rebuild — twice a second — and used to re-read the environment, rebuild the
// same list from scratch and sort it a second time on each one.
func (m *model) appendFakes() {
	if len(m.fakes) == 0 {
		return
	}
	have := make(map[string]bool, len(m.rows))
	for _, r := range m.rows {
		have[r.session] = true
	}
	for _, r := range m.fakes {
		if have[r.session] {
			continue
		}
		m.rows = append(m.rows, r)
	}
}
