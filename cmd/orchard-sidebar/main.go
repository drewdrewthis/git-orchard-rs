// orchard-sidebar: a tmux sidebar pane over claude-session-state + the orchard daemon.
//
// Truth layering (issue #719):
//   - state dir (2s): ~/.local/state/claude-sessions/state/*.json written by the
//     claude-session-state plugin hooks — authoritative for working|idle|input,
//     mission (first_prompt), and attention messages. Works with the daemon down.
//   - daemon fast lane (2s): claudeInstances + tmuxSessions via GraphQL — session
//     inventory, model, pane titles; inference fallback for sessions with no
//     state file (marked "inferred").
//   - daemon slow lane (30s): workView.repos — eagerly walks gh per worktree
//     (measured 27s cold), so it must never block the fast lane.
//
// Enter/click jumps via `tmux switch-client`. Read-only plus jump.
//
// CI: statusCheckRollup is rendered as its raw string, never as a pass/fail
// verdict — queued checks with null state read as green through any bad-state
// filter (false-green incident).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const graphqlURL = "http://127.0.0.1:7777/graphql"
const fastEvery = 2 * time.Second
const slowEvery = 30 * time.Second

const fastQuery = `{ workView {
  claudeInstances { state sessionUuid model pane { title window { session { name } } } lastActivityAt }
  tmuxSessions { name attached }
  meta { failureReason }
} }`

const slowQuery = `{ workView { repos { slug worktrees {
  branch ahead behind
  tmuxSession { name }
  pr { number state draft reviewDecision statusCheckRollup mergeStateStatus }
  issue { number title }
} } } }`

type fastResp struct {
	Data struct {
		WorkView struct {
			ClaudeInstances []struct {
				State       string  `json:"state"`
				SessionUuid string  `json:"sessionUuid"`
				Model       *string `json:"model"`
				Pane        *struct {
					Title  string `json:"title"`
					Window struct {
						Session struct {
							Name string `json:"name"`
						} `json:"session"`
					} `json:"window"`
				} `json:"pane"`
				LastActivityAt string `json:"lastActivityAt"`
			} `json:"claudeInstances"`
			TmuxSessions []struct {
				Name     string `json:"name"`
				Attached bool   `json:"attached"`
			} `json:"tmuxSessions"`
			Meta struct {
				FailureReason *string `json:"failureReason"`
			} `json:"meta"`
		} `json:"workView"`
	} `json:"data"`
}

type prInfo struct {
	Number           int     `json:"number"`
	State            string  `json:"state"`
	Draft            bool    `json:"draft"`
	ReviewDecision   *string `json:"reviewDecision"`
	ChecksRollup     string  `json:"statusCheckRollup"`
	MergeStateStatus string  `json:"mergeStateStatus"`
}

type wtInfo struct {
	Branch      string `json:"branch"`
	Ahead       *int   `json:"ahead"`
	Behind      *int   `json:"behind"`
	TmuxSession *struct {
		Name string `json:"name"`
	} `json:"tmuxSession"`
	PR    *prInfo `json:"pr"`
	Issue *struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
	} `json:"issue"`
}

type slowResp struct {
	Data struct {
		WorkView struct {
			Repos []struct {
				Slug      string   `json:"slug"`
				Worktrees []wtInfo `json:"worktrees"`
			} `json:"repos"`
		} `json:"workView"`
	} `json:"data"`
}

type row struct {
	session  string
	state    string // input | stalled | working | idle | shell
	title    string
	attached bool
	hooked   bool // state came from the state dir; false = daemon inference
	message  string
	mission  string
	lastAct  time.Time
	// slow-lane join, may be zero-valued until the first slow fetch lands
	branch     string
	repo       string
	ahead      *int
	behind     *int
	pr         *prInfo
	model      string
	issueNum   int
	issueTitle string
}

var stateRank = map[string]int{"input": 0, "stalled": 1, "working": 2, "idle": 3, "shell": 4}

type model struct {
	rows         []row
	wtBySession  map[string]wtInfo
	repoBySess   map[string]string
	hooksBySess  map[string]hookState
	stateDirOK   bool
	cursor       int
	lineToRow    []int // rendered line index -> row index (-1 = not a row)
	lastClickAt  time.Time
	lastClickRow int
	err          error
	fastAt       time.Time
	slowAt       time.Time
	width        int
}

type fastTickMsg struct{}
type slowTickMsg struct{}
type fastDataMsg struct {
	rows []row
	err  error
}
type slowDataMsg struct {
	wtBySession map[string]wtInfo
	repoBySess  map[string]string
	err         error
}

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
	Message     string `json:"message"`
	FirstPrompt string `json:"first_prompt"`
	LastTool    string `json:"last_tool"`
	ToolCalls   int    `json:"tool_calls"`
}

type hookState struct {
	state   string // working | idle | input
	lastAct time.Time
	pane    string
	message string
	mission string
}

type hookDataMsg struct {
	bySession map[string]hookState // tmux session name -> state
	dirOK     bool
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

// paneToSession maps tmux pane ids (%5) to session names via one tmux call.
func paneToSession() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_id} #{session_name}").Output()
	m := map[string]string{}
	if err != nil {
		return m
	}
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id, name, ok := strings.Cut(ln, " "); ok {
			m[id] = name
		}
	}
	return m
}

func fetchHooks() tea.Msg {
	files, err := filepath.Glob(stateDirPath() + "/*.json")
	if err != nil || files == nil {
		return hookDataMsg{dirOK: err == nil && dirExists(stateDirPath())}
	}
	p2s := paneToSession()
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
			by[sess] = hookState{state: s.State, lastAct: t, pane: s.Pane,
				message: s.Message, mission: s.FirstPrompt}
		}
	}
	return hookDataMsg{bySession: by, dirOK: true}
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func post(query string, timeout time.Duration, out any) error {
	body, _ := json.Marshal(map[string]string{"query": query})
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodPost, graphqlURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("daemon: HTTP %d", resp.StatusCode)
	}
	// GraphQL can return 200 with an errors array and a zero-value data field;
	// treating that as valid would blank the sidebar.
	var envelope struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Errors) > 0 {
		return fmt.Errorf("graphql: %s", envelope.Errors[0].Message)
	}
	return json.Unmarshal(raw, out)
}

func fetchFast() tea.Msg {
	var out fastResp
	if err := post(fastQuery, 4*time.Second, &out); err != nil {
		return fastDataMsg{err: err}
	}
	wv := out.Data.WorkView
	if wv.Meta.FailureReason != nil {
		return fastDataMsg{err: fmt.Errorf("daemon: %s", *wv.Meta.FailureReason)}
	}

	byName := map[string]*row{}
	for _, s := range wv.TmuxSessions {
		byName[s.Name] = &row{session: s.Name, state: "shell", attached: s.Attached}
	}
	seen := map[string]bool{} // dedupe: daemon returns duplicate rows per sessionUuid
	for _, ci := range wv.ClaudeInstances {
		if ci.Pane == nil || (ci.SessionUuid != "" && seen[ci.SessionUuid]) {
			continue
		}
		seen[ci.SessionUuid] = true
		name := ci.Pane.Window.Session.Name
		r, ok := byName[name]
		if !ok {
			r = &row{session: name}
			byName[name] = r
		}
		r.state = ci.State
		r.title = ci.Pane.Title
		if ci.Model != nil {
			r.model = shortModel(*ci.Model)
		}
		r.lastAct, _ = time.Parse(time.RFC3339, ci.LastActivityAt)
	}

	rows := make([]row, 0, len(byName))
	for _, r := range byName {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if a, b := stateRank[rows[i].state], stateRank[rows[j].state]; a != b {
			return a < b
		}
		return rows[i].session < rows[j].session
	})
	return fastDataMsg{rows: rows}
}

func fetchSlow() tea.Msg {
	var out slowResp
	if err := post(slowQuery, 90*time.Second, &out); err != nil {
		return slowDataMsg{err: err}
	}
	wt := map[string]wtInfo{}
	repo := map[string]string{}
	for _, r := range out.Data.WorkView.Repos {
		for _, w := range r.Worktrees {
			if w.TmuxSession != nil {
				wt[w.TmuxSession.Name] = w
				repo[w.TmuxSession.Name] = r.Slug
			}
		}
	}
	return slowDataMsg{wtBySession: wt, repoBySess: repo}
}

func tickAfter(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

// One in-flight request per lane: each lane schedules its next tick only when
// its data message lands, so a slow response can never be overwritten by an
// older poll that finished later.
func (m *model) Init() tea.Cmd {
	return tea.Batch(fetchFast, fetchSlow, fetchHooks)
}

func (m *model) join() {
	for i := range m.rows {
		if w, ok := m.wtBySession[m.rows[i].session]; ok {
			m.rows[i].branch = w.Branch
			m.rows[i].ahead = w.Ahead
			m.rows[i].behind = w.Behind
			m.rows[i].pr = w.PR
			m.rows[i].repo = m.repoBySess[m.rows[i].session]
			if w.Issue != nil {
				m.rows[i].issueNum = w.Issue.Number
				m.rows[i].issueTitle = w.Issue.Title
			}
		}
	}
}

// applyHooks overlays state-dir truth on the daemon-derived rows (hook lane
// wins on state/activity/attention; daemon data — model, PR join — stays),
// then appends rows for hook-known sessions the daemon missed. With the
// daemon down entirely, this is the whole view.
func (m *model) applyHooks() {
	seen := map[string]bool{}
	for i := range m.rows {
		seen[m.rows[i].session] = true
		if h, ok := m.hooksBySess[m.rows[i].session]; ok {
			m.rows[i].state = h.state
			m.rows[i].hooked = true
			m.rows[i].message = h.message
			m.rows[i].mission = h.mission
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
			message: h.message, mission: h.mission, lastAct: h.lastAct})
	}
	sort.SliceStable(m.rows, func(i, j int) bool {
		return stateRank[m.rows[i].state] < stateRank[m.rows[j].state]
	})
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case fastTickMsg:
		return m, tea.Batch(fetchFast, fetchHooks)
	case slowTickMsg:
		return m, fetchSlow
	case fastDataMsg:
		m.err = msg.err
		if msg.err == nil {
			m.rows = msg.rows
			m.fastAt = time.Now()
			m.join()
			m.applyHooks()
			if m.cursor >= len(m.rows) {
				m.cursor = max(0, len(m.rows)-1)
			}
		} else {
			// daemon down: rebuild the view from the hook lane alone
			m.rows = nil
			m.applyHooks()
		}
		return m, tickAfter(fastEvery, fastTickMsg{})
	case hookDataMsg:
		m.hooksBySess = msg.bySession
		m.stateDirOK = msg.dirOK
		m.applyHooks()
		return m, nil
	case slowDataMsg:
		if msg.err == nil {
			m.wtBySession = msg.wtBySession
			m.repoBySess = msg.repoBySess
			m.slowAt = time.Now()
			m.join()
		}
		return m, tickAfter(slowEvery, slowTickMsg{})
	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			if msg.Y >= 0 && msg.Y < len(m.lineToRow) {
				if ri := m.lineToRow[msg.Y]; ri >= 0 {
					m.cursor = ri
					exec.Command("tmux", "switch-client", "-t", m.rows[ri].session).Start()
				}
			}
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "j", "down":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		case "k", "up":
			if m.cursor > 0 {
				m.cursor--
			}
		case "enter":
			if m.cursor < len(m.rows) {
				exec.Command("tmux", "switch-client", "-t", m.rows[m.cursor].session).Start()
			}
		}
	}
	return m, nil
}

var (
	styInput   = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	styStalled = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	styWorking = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styIdle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styShell   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	stySelBar  = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	stySelName = lipgloss.NewStyle().Bold(true).Underline(true)
	styErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	styDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styMeta    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styMsg     = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
)

func glyph(state string) (string, lipgloss.Style) {
	switch state {
	case "input":
		return "● INPUT", styInput
	case "stalled":
		return "✖ STALL", styStalled
	case "working":
		return "◐ work ", styWorking
	case "idle":
		return "○ idle ", styIdle
	default:
		return "· shell", styShell
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

func age(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t).Round(time.Minute)
	if d < time.Minute {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func trunc(s string, n int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// detail renders the worktree line of a row: branch ±ahead/behind, PR state,
// raw checks rollup string (never a verdict), review decision.
func detail(r row) string {
	if r.branch == "" {
		return ""
	}
	s := "  " + r.branch
	if r.ahead != nil && *r.ahead > 0 {
		s += fmt.Sprintf(" ↑%d", *r.ahead)
	}
	if r.behind != nil && *r.behind > 0 {
		s += fmt.Sprintf(" ↓%d", *r.behind)
	}
	if r.issueNum > 0 {
		s += fmt.Sprintf(" · i#%d", r.issueNum)
	}
	if r.pr != nil {
		s += fmt.Sprintf(" · #%d %s", r.pr.Number, r.pr.State)
		if r.pr.Draft {
			s += " draft"
		}
		if r.pr.ChecksRollup != "" {
			s += " ci:" + r.pr.ChecksRollup
		}
		if r.pr.ReviewDecision != nil {
			s += " " + *r.pr.ReviewDecision
		}
		if r.pr.MergeStateStatus != "" && r.pr.MergeStateStatus != "CLEAN" {
			s += " " + r.pr.MergeStateStatus
		}
	}
	return s
}

func (m *model) View() string {
	var b bytes.Buffer
	lineMap := []int{}
	emit := func(s string, rowIdx int) {
		b.WriteString(s + "\n")
		lineMap = append(lineMap, rowIdx)
	}
	sep := styDim.Render(strings.Repeat("─", 40))

	if m.err != nil {
		emit(styErr.Render("⚠ DAEMON OFFLINE"), -1)
		emit(styDim.Render(trunc(m.err.Error(), 38)), -1)
		emit(styDim.Render("hook states still live"), -1)
		emit("", -1)
	}

	counts := map[string]int{}
	for _, r := range m.rows {
		counts[r.state]++
	}
	var hdr []string
	for _, s := range []string{"input", "stalled", "working", "idle"} {
		if counts[s] > 0 {
			g, sty := glyph(s)
			hdr = append(hdr, sty.Render(fmt.Sprintf("%d %s", counts[s], strings.TrimSpace(g))))
		}
	}
	if len(hdr) > 0 {
		emit(strings.Join(hdr, styDim.Render("  ")), -1)
	}
	emit(sep, -1)

	for i, r := range m.rows {
		g, sty := glyph(r.state)
		bar, name := "  ", r.session
		if r.attached {
			bar = stySelBar.Render("▎ ")
			name = stySelName.Render(r.session)
		}
		if i == m.cursor && !r.attached {
			bar = stySelBar.Render("› ")
		}
		line1 := fmt.Sprintf("%s%s %s", bar, sty.Render(g), name)
		if r.attached {
			line1 += styDim.Render(" ⌖")
		}
		if !r.hooked && r.state != "shell" {
			line1 += styDim.Render(" (inferred)")
		}
		emit(line1, i)

		if r.state == "input" && r.message != "" {
			emit(styMsg.Render("    ↳ "+trunc(r.message, 36)), i)
		}
		if r.mission != "" {
			emit(styDim.Render("    “"+trunc(r.mission, 34)+"”"), i)
		}

		meta := "    "
		if r.model != "" {
			meta += r.model + " · "
		}
		if a := age(r.lastAct); a != "" {
			meta += a
		} else {
			meta += "—"
		}
		if r.repo != "" {
			meta += " · " + r.repo
		}
		emit(styDim.Render(meta), i)

		if d := detail(r); d != "" {
			emit(styMeta.Render(d), i)
		}
		emit(sep, -1)
	}

	if !m.stateDirOK {
		emit(styDim.Render("no state dir — install the"), -1)
		emit(styDim.Render("claude-session-state plugin"), -1)
	}
	prNote := "pr:–"
	if !m.slowAt.IsZero() {
		prNote = "pr:" + m.slowAt.Format("15:04")
	}
	stamp := "—"
	if !m.fastAt.IsZero() {
		stamp = m.fastAt.Format("15:04:05")
	}
	emit(styDim.Render(fmt.Sprintf("%d sessions · %s · %s", len(m.rows), stamp, prNote)), -1)
	emit(styDim.Render("click/enter jump · j/k · q quit"), -1)

	m.lineToRow = lineMap
	return b.String()
}

func main() {
	if _, err := tea.NewProgram(&model{}, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
