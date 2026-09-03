package main

import (
	"path/filepath"
	"time"
)

// How each lane's answer lands in the model. Split from the model itself
// because this is where the freshness rules live — which lane outranks which
// on attach, what a transient failure may and may not wipe — and every one of
// them was learned from a defect.

// applyFast folds one fast-lane answer into the model. The poll owns
// state/model/title and the row inventory; it does NOT own attach while the
// push lane is live (see below).
func (m *model) applyFast(msg fastDataMsg) {
	m.err = msg.err
	if msg.err != nil {
		// A slow answer is not the daemon going away. fastQuery is normally
		// well under 1.5s but spikes past the 4s client timeout while tmux
		// churns -- which is exactly when the user switches sessions. Wiping
		// here dropped every daemon-derived row, and the selection with it,
		// for as long as the spike lasted. Hold the last good snapshot
		// through a transient failure; the push lane keeps its attach flags
		// honest meanwhile. Only fall back to the hook lane alone once the
		// daemon has really been unreachable for a while (daemonDown — the
		// same judgment that gates the offline banner).
		if m.daemonDown() {
			m.rows = nil
		}
		m.rebuild()
		return
	}
	m.rows = msg.rows
	m.fastAt = time.Now()
	m.paneToSess = msg.paneToSess
	// The poll's attach flags were true up to a daemon poll ago and the
	// request itself took a moment, so a poll in flight across a switch lands
	// *after* the pushed snapshot carrying pre-switch attachment. Letting it
	// through reverted the selection and made a switch look like it took a
	// full poll cycle to land. The push lane is strictly fresher, so it wins
	// for as long as it is live.
	if m.subLive() {
		for i := range m.rows {
			m.rows[i].attached = m.attachedBySess[m.rows[i].session]
		}
	}
	m.rebuild()
}

// applySessions folds one pushed tmux snapshot into the model: fresher than
// any poll, so it wins on attach and on the pane map. It does NOT touch
// state/model/mission — those are the fast lane's, and this message carries
// nothing about them.
func (m *model) applySessions(sessions []tmuxSession) {
	m.subAt = time.Now()
	attached, p2s := foldSessions(sessions)
	m.paneToSess, m.attachedBySess = p2s, attached
	live := map[string]bool{}
	for _, s := range sessions {
		live[s.Name] = true
	}
	kept := m.rows[:0]
	have := map[string]bool{}
	for _, r := range m.rows {
		if !live[r.session] {
			continue // session is gone; don't leave a card that attaches to nothing
		}
		r.attached = attached[r.session]
		have[r.session] = true
		kept = append(kept, r)
	}
	m.rows = kept
	for _, s := range sessions {
		if have[s.Name] {
			continue
		}
		// new session: shell until the fast lane reports a claude instance in
		// it, which is the same default fetchFast applies
		m.rows = append(m.rows, row{session: s.Name, state: "shell",
			attached: attached[s.Name]})
	}
	m.rebuild()
}

func (m *model) join() {
	for i := range m.rows {
		w, ok := m.wtBySession[m.rows[i].session]
		repo := m.repoBySess[m.rows[i].session]
		if !ok && m.rows[i].cwd != "" {
			// The daemon joins each worktree to a single tmux session; a
			// second session sitting in the same checkout loses the name
			// join, so fall back to an exact cwd -> worktree-path match.
			// Exact, not prefix: worktrees nest under repo roots, so a
			// prefix join would hand nested-worktree sessions the parent's
			// branch.
			p := filepath.Clean(m.rows[i].cwd)
			w, ok = m.wtByPath[p]
			repo = m.repoByPath[p]
		}
		if !ok {
			continue
		}
		m.rows[i].branch = w.Branch
		m.rows[i].ahead = w.Ahead
		m.rows[i].behind = w.Behind
		m.rows[i].pr = w.PR
		m.rows[i].repo = repo
		if w.Issue != nil {
			m.rows[i].issueNum = w.Issue.Number
			m.rows[i].issueTitle = w.Issue.Title
		}
	}
}

// rebuild re-derives everything view-facing after any lane lands: hook
// overlay first (the cwd-fallback join reads row.cwd, which the overlay
// supplies — this also joins hook-appended rows), then the synthetic rows,
// then ONE sort over the finished list, then the slow-lane join, then re-find
// the cursor row: the sort moves cards, so the old cursor index points at
// whatever card slid into that slot.
//
// The bell goes last, and here rather than in each lane: every path that can
// put a session into the Needs-attention bucket ends up in this function, and
// a transition detected in three places is a transition detected three times.
func (m *model) rebuild() {
	m.applyHooks()
	m.appendFakes()
	sortRows(m.rows)
	m.join()
	m.reanchorCursor()
	m.bellCheck()
}

// applyHooks overlays state-dir truth on the daemon-derived rows (hook lane
// wins on state/activity/prompt; daemon data — model, PR join — stays),
// then appends rows for hook-known sessions the daemon missed. With the
// daemon down entirely, this is the whole view.
func (m *model) applyHooks() {
	seen := map[string]bool{}
	for i := range m.rows {
		seen[m.rows[i].session] = true
		if m.rows[i].fake {
			// A synthetic row carries its own state and has no hook file, so
			// the else branch below would strip its hooked flag on every
			// rebuild and paint a "state unverified" ? on every card.
			continue
		}
		if h, ok := m.hooksBySess[m.rows[i].session]; ok {
			m.rows[i].state = h.state
			m.rows[i].hooked = true
			m.rows[i].mission = h.mission
			m.rows[i].cwd = h.cwd
			if h.lastAct.After(m.rows[i].lastAct) {
				m.rows[i].lastAct = h.lastAct
			}
		} else {
			m.rows[i].hooked = false
		}
	}
	for sess, h := range m.hooksBySess {
		if seen[sess] {
			continue
		}
		m.rows = append(m.rows, row{session: sess, state: h.state, hooked: true,
			mission: h.mission, lastAct: h.lastAct, cwd: h.cwd})
	}
}
