// orchard-status: query live Claude Code sessions from the claude-session-state
// state dir (contract: plugin-sources/claude-session-state/docs/STATE_SCHEMA.md).
// The reference fold consumer: live file + live pid = live session.
//
// Usage: orchard status [--json]   (ADR-013 dispatcher convention)
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

type sessFile struct {
	Sid          string `json:"sid"`
	Cwd          string `json:"cwd"`
	State        string `json:"state"`
	Pid          int    `json:"pid"`
	Pane         string `json:"pane"`
	Ts           string `json:"ts"`
	StartedAt    string `json:"started_at"`
	LastEvent    string `json:"last_event"`
	Message      string `json:"message"`
	FirstPrompt  string `json:"first_prompt"`
	LastPrompt   string `json:"last_prompt"`
	LastTool     string `json:"last_tool"`
	LastResponse string `json:"last_response"`
	ToolCalls    int    `json:"tool_calls"`
}

func stateDir() string {
	if d := os.Getenv("CLAUDE_SESSION_STATE_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return home + "/.local/state/claude-sessions/state"
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func main() {
	asJSON := len(os.Args) > 1 && os.Args[1] == "--json"

	files, _ := filepath.Glob(stateDir() + "/*.json")
	var live []sessFile
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s sessFile
		if json.Unmarshal(raw, &s) != nil || s.Sid == "" {
			continue
		}
		// dead pid with a leftover file = crashed session; not live
		if s.Pid <= 0 || syscall.Kill(s.Pid, 0) != nil {
			continue
		}
		live = append(live, s)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Ts > live[j].Ts })

	if asJSON {
		if live == nil {
			live = []sessFile{}
		}
		json.NewEncoder(os.Stdout).Encode(live)
		return
	}

	if len(live) == 0 {
		fmt.Println("no live sessions (state dir: " + stateDir() + ")")
		return
	}
	for _, s := range live {
		age := ""
		if t, err := time.Parse(time.RFC3339, s.Ts); err == nil {
			age = time.Since(t).Round(time.Second).String()
		}
		fmt.Printf("%-8s %-6s %s  pid=%d pane=%s tools=%d\n",
			s.State, age, s.Cwd, s.Pid, s.Pane, s.ToolCalls)
		if s.FirstPrompt != "" {
			fmt.Printf("         mission: %s\n", trunc(s.FirstPrompt, 100))
		}
		if s.State == "input" && s.Message != "" {
			fmt.Printf("         ⚠ %s\n", trunc(s.Message, 100))
		}
	}
}
