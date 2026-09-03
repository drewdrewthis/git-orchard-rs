package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ---- state-dir lane: folded per-session files written by the
// claude-session-state plugin (contract: plugin-sources/claude-session-state/
// docs/STATE_SCHEMA.md). Live file + live pid = live session.

type sessFile struct {
	Sid         string `json:"sid"`
	Cwd         string `json:"cwd"`
	State       string `json:"state"`
	Pid         int    `json:"pid"`
	Pane        string `json:"pane"`
	Ts          string `json:"ts"`
	FirstPrompt string `json:"first_prompt"`
	LastPrompt  string `json:"last_prompt"`
	LastTool    string `json:"last_tool"`
	ToolCalls   int    `json:"tool_calls"`
}

type hookState struct {
	state   string // working | idle | input
	lastAct time.Time
	mission string
	cwd     string
}

type hookDataMsg struct {
	bySession map[string]hookState // tmux session name -> state
	// order carries the tmux attach/create timestamps the list sorts on. Read
	// here rather than off the daemon because the schema serves neither, and
	// this lane execs tmux already (paneToSession fallback) and runs whether
	// the daemon is up or down.
	order map[string]sessMeta
	dirOK bool
	// err is a local read that failed (an unreadable state dir). It changes
	// nothing on screen — the "no state dir" footer line already covers the
	// user-visible half — but it is logged rather than dropped, so a broken
	// install leaves a trail.
	err error
}

func stateDirPath() string {
	if d := os.Getenv("CLAUDE_SESSION_STATE_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return home + "/.local/state/claude-sessions/state"
}

func pidAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

// fetchHooksWith folds the state dir against a pane->session map the caller
// already has from the daemon's fast lane. Empty map = the daemon hasn't
// answered yet (or is down), so fall back to asking tmux directly.
func fetchHooksWith(p2s map[string]string) tea.Cmd {
	return func() tea.Msg { return fetchHooksUsing(p2s) }
}

func fetchHooksUsing(p2s map[string]string) tea.Msg {
	files, err := filepath.Glob(stateDirPath() + "/*.json")
	// the tmux ordering keys the list sorts on, fetched whether or not any
	// state file exists: daemon-derived rows need an order too
	order := sessionOrder()
	if err != nil || files == nil {
		return hookDataMsg{order: order, dirOK: err == nil && dirExists(stateDirPath()), err: err}
	}
	if len(p2s) == 0 {
		p2s = paneToSession()
	}
	by := map[string]hookState{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s sessFile
		if json.Unmarshal(raw, &s) != nil || s.Sid == "" || !pidAlive(s.Pid) {
			continue
		}
		sess, ok := p2s[s.Pane]
		if !ok {
			continue // headless session (claude -p, no tmux pane) — orchard status covers it
		}
		t, _ := time.Parse(time.RFC3339, s.Ts)
		// several claude sids can share a tmux session; most recent wins
		if cur, dup := by[sess]; !dup || t.After(cur.lastAct) {
			by[sess] = hookState{state: s.State, lastAct: t, mission: promptOf(s), cwd: s.Cwd}
		}
	}
	return hookDataMsg{bySession: by, order: order, dirOK: true}
}

// promptOf is what the card quotes: the latest thing you asked, which the
// plugin rewrites on every UserPromptSubmit. first_prompt is the set-once
// fallback for a session that has a state file but no prompt since it landed.
func promptOf(s sessFile) string {
	if s.LastPrompt != "" {
		return s.LastPrompt
	}
	return s.FirstPrompt
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
