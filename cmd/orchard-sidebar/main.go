// orchard-sidebar: a tmux sidebar pane over claude-session-state + the orchard daemon.
//
// Truth layering (issue #719):
//   - state dir (2s): ~/.local/state/claude-sessions/state/*.json written by the
//     claude-session-state plugin hooks — authoritative for working|idle|input
//     and the latest prompt (last_prompt). Works with the daemon down.
//   - daemon fast lane (2s): claudeInstances + tmuxSessions via GraphQL — session
//     inventory, model, pane titles; inference fallback for sessions with no
//     state file (marked "inferred").
//   - daemon slow lane (30s): workView.repos — eagerly walks gh per worktree
//     (measured 27s cold), so it must never block the fast lane.
//
// j/k and clicking both attach the session via `tmux switch-client` — selection
// and the attached session are the same thing. Read-only plus that switch.
//
// CI: statusCheckRollup is collapsed to one status word by prStatus, which only
// ever says "green" on a literal SUCCESS rollup — a queued or unknown rollup
// reads as "unresolved", never green (false-green incident).
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
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var graphqlURL = "http://127.0.0.1:7777/graphql"

const fastEvery = 2 * time.Second
const slowEvery = 30 * time.Second

const fastQuery = `{ workView {
  claudeInstances { state sessionUuid model pane { title window { session { name } } } lastActivityAt }
  tmuxSessions { name attached createdAt windows { panes { paneId } } }
  meta { failureReason }
} }`

const slowQuery = `{ workView { repos { slug worktrees {
  branch path ahead behind
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
			TmuxSessions []tmuxSession `json:"tmuxSessions"`
			Meta         struct {
				FailureReason *string `json:"failureReason"`
			} `json:"meta"`
		} `json:"workView"`
	} `json:"data"`
}

// tmuxSession is the shape both lanes read: the fast poll's tmuxSessions and
// the tmuxSessionsChanged subscription, which emits the same full snapshot.
type tmuxSession struct {
	Name      string `json:"name"`
	Attached  bool   `json:"attached"`
	CreatedAt string `json:"createdAt"`
	Windows   []struct {
		Panes []struct {
			PaneId string `json:"paneId"`
		} `json:"panes"`
	} `json:"windows"`
}

// foldSessions derives everything the view needs from a session snapshot:
// attach state, the within-group sort key, and the pane->session map that
// the state-dir lane folds its files against.
func foldSessions(ss []tmuxSession) (attached map[string]bool, created map[string]time.Time, p2s map[string]string) {
	attached, created, p2s = map[string]bool{}, map[string]time.Time{}, map[string]string{}
	for _, s := range ss {
		attached[s.Name] = s.Attached
		// createdAt is RFC3339 per the schema; a session we can't parse gets a
		// zero time, which sortRows puts last rather than at the top.
		if t, err := time.Parse(time.RFC3339, s.CreatedAt); err == nil {
			created[s.Name] = t
		}
		for _, w := range s.Windows {
			for _, pn := range w.Panes {
				p2s[pn.PaneId] = s.Name
			}
		}
	}
	return attached, created, p2s
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
	Path        string `json:"path"`
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
	mission  string
	lastAct  time.Time
	created  time.Time // tmux session_created — the stable within-group sort key
	cwd      string
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
	rows           []row
	wtBySession    map[string]wtInfo
	repoBySess     map[string]string
	wtByPath       map[string]wtInfo // cwd fallback: worktree path -> info
	repoByPath     map[string]string
	hooksBySess    map[string]hookState
	createdBySess  map[string]time.Time
	paneToSess     map[string]string // daemon-served pane id -> session
	frame          int               // animation frame, advanced by animTickMsg
	stateDirOK     bool
	cursor         int
	cursorSess     string    // session under the cursor, so re-sorts don't drag it
	lineToRow      []int     // rendered line index -> row index (-1 = not a row)
	lineToCopy     []string  // rendered line index -> click-to-copy payload ("" = none)
	copiedUntil    time.Time // git box shows "✓ copied" until this passes
	lastClickAt    time.Time
	lastClickRow   int
	err            error
	subErr         error           // websocket lane only; a drop degrades to polling
	subAt          time.Time       // last pushed snapshot
	attachedBySess map[string]bool // from the push lane; outranks the poll's copy
	fastAt         time.Time
	width          int
	clients        int // attached clients per latest clientSessMsg
	desiredWidth   int // shared @orchard_sidebar_width; 0 until first read
	clientGen      int // bumped on switch/resize; older in-flight reads are stale
}

type fastTickMsg struct{}
type slowTickMsg struct{}
type fastDataMsg struct {
	rows []row
	// pane id (%5) -> session name, and session name -> creation time: both
	// daemon-served, replacing what the client used to get by exec'ing tmux.
	paneToSess    map[string]string
	createdBySess map[string]time.Time
	err           error
}
type slowDataMsg struct {
	wtBySession map[string]wtInfo
	repoBySess  map[string]string
	wtByPath    map[string]wtInfo
	repoByPath  map[string]string
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

// paneToSession maps tmux pane ids (%5) to session names.
//
// DAEMON-DOWN FALLBACK ONLY. The daemon serves this same mapping on
// tmuxSessions{windows{panes{paneId}}}, and fetchHooks prefers it — a client
// that execs tmux for data the daemon owns violates RULES L7/M2 and the
// ADR-017 anti-pattern. It survives here because the state-dir lane is
// documented to work with the daemon down (header, issue #719), and without
// pane->session every state file looks headless and the sidebar renders empty.
// Steady state no longer touches tmux at all; see orchardist#726 for the
// remaining exec (switchClient), which has no daemon equivalent yet.
// Session names may contain spaces, so the name goes last.
func paneToSession() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F",
		"#{pane_id} #{session_name}").Output()
	m := map[string]string{}
	if err != nil {
		return m
	}
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, name, ok := strings.Cut(ln, " ")
		if !ok {
			continue
		}
		m[id] = name
	}
	return m
}

// fetchHooksWith folds the state dir against a pane->session map the caller
// already has from the daemon's fast lane. Empty map = the daemon hasn't
// answered yet (or is down), so fall back to asking tmux directly.
func fetchHooksWith(p2s map[string]string) tea.Cmd {
	return func() tea.Msg { return fetchHooksUsing(p2s) }
}

func fetchHooksUsing(p2s map[string]string) tea.Msg {
	files, err := filepath.Glob(stateDirPath() + "/*.json")
	if err != nil || files == nil {
		return hookDataMsg{dirOK: err == nil && dirExists(stateDirPath())}
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
	return hookDataMsg{bySession: by, dirOK: true}
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

// hasData reports whether a GraphQL envelope carried a usable data payload.
// Absent and explicit null both mean nothing resolved -- the same distinction
// that #693/#695 was filed for in this repo.
func hasData(d json.RawMessage) bool {
	t := bytes.TrimSpace(d)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
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
	// GraphQL can return 200 with an errors array and a zero-value data
	// field; treating that as valid would blank the sidebar. But it can
	// equally return errors alongside a fully populated payload -- the daemon
	// does exactly that when GitHub rate-limits the pr/issue leaves while
	// every other leaf resolves. Only the first case is fatal: discarding a
	// populated payload blanks every github field at once.
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Errors) > 0 && !hasData(envelope.Data) {
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

	_, created, p2s := foldSessions(wv.TmuxSessions)
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
	sortRows(rows)
	return fastDataMsg{rows: rows, paneToSess: p2s, createdBySess: created}
}

func fetchSlow() tea.Msg {
	var out slowResp
	if err := post(slowQuery, 90*time.Second, &out); err != nil {
		return slowDataMsg{err: err}
	}
	wt := map[string]wtInfo{}
	repo := map[string]string{}
	wtp := map[string]wtInfo{}
	repop := map[string]string{}
	for _, r := range out.Data.WorkView.Repos {
		for _, w := range r.Worktrees {
			if w.TmuxSession != nil {
				wt[w.TmuxSession.Name] = w
				repo[w.TmuxSession.Name] = r.Slug
			}
			if w.Path != "" {
				p := filepath.Clean(w.Path)
				wtp[p] = w
				repop[p] = r.Slug
			}
		}
	}
	return slowDataMsg{wtBySession: wt, repoBySess: repo, wtByPath: wtp, repoByPath: repop}
}

func tickAfter(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

// One in-flight request per lane: each lane schedules its next tick only when
// its data message lands, so a slow response can never be overwritten by an
// older poll that finished later.
func (m *model) Init() tea.Cmd {
	return tea.Batch(fetchFast, fetchSlow, fetchHooksWith(nil), fetchClientSession(0),
		tickAfter(animEvery, animTickMsg{}))
}

// workFrames spins the working glyph. Data lanes poll every 2s, far too slow to
// read as motion, so animation gets its own tick that touches nothing but the
// frame counter.
var workFrames = []string{"◐", "◓", "◑", "◒"}

const animEvery = 250 * time.Millisecond

// subFresh is how long a pushed snapshot outranks the poll. The server pings
// every 10s, so anything past that and the socket is not delivering.
const subFresh = 30 * time.Second

// daemonGone is how long the fast lane must stay broken before the sidebar
// believes the daemon is actually gone and falls back to the hook lane alone.
// Comfortably above the 4s client timeout so one slow response cannot trip it.
const daemonGone = 15 * time.Second

type animTickMsg struct{}
type clientTickMsg struct{}

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
// supplies — this also joins hook-appended rows), then the slow-lane join,
// then re-find the cursor row: applyHooks re-sorts, so the old cursor index
// points at whatever card slid into that slot.
func (m *model) rebuild() {
	m.applyHooks()
	m.join()
	m.reanchorCursor()
}

// applyHooks overlays state-dir truth on the daemon-derived rows (hook lane
// wins on state/activity/prompt; daemon data — model, PR join — stays),
// then appends rows for hook-known sessions the daemon missed. With the
// daemon down entirely, this is the whole view.
func (m *model) applyHooks() {
	seen := map[string]bool{}
	for i := range m.rows {
		seen[m.rows[i].session] = true
		m.rows[i].created = m.createdBySess[m.rows[i].session]
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
			mission: h.mission, lastAct: h.lastAct, cwd: h.cwd,
			created: m.createdBySess[sess]})
	}
	sortRows(m.rows)
}

// sortRows: status first, then oldest-session-first within the group, then
// name. Creation time never changes, so a card holds its slot in its group for
// the life of the session — the only movement left is the jump between groups
// when a session changes state. Ordering by recent activity instead meant every
// keystroke anywhere could reshuffle the list under the cursor.
func sortRows(rows []row) {
	sort.SliceStable(rows, func(i, j int) bool {
		if a, b := stateRank[rows[i].state], stateRank[rows[j].state]; a != b {
			return a < b
		}
		// a session tmux hasn't told us about yet sorts last rather than first,
		// so an unknown creation time can't displace a settled card
		ci, cj := rows[i].created, rows[j].created
		if ci.IsZero() != cj.IsZero() {
			return cj.IsZero()
		}
		if !ci.Equal(cj) {
			return ci.Before(cj)
		}
		return rows[i].session < rows[j].session
	})
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A size we didn't ask for while the shared width is known means the
		// user dragged this pane (or the layout shoved it): last write wins,
		// publish it and every other session follows on its next tick. A size
		// matching desiredWidth is just our own enforcement echoing back.
		// Detached resizes are mechanical (proportional redistribution on
		// attach/detach churn, #736), not a human dragging: reassert the shared
		// width instead of adopting whatever the layout shoved us to. Only an
		// attached client's drag is authoritative.
		if m.desiredWidth != 0 && msg.Width != m.desiredWidth && m.clients == 0 {
			m.width = msg.Width
			resizePane(m.desiredWidth)
			return m, nil
		}
		if m.desiredWidth != 0 && msg.Width != m.desiredWidth {
			w := msg.Width
			if w < minWidth {
				w = minWidth
			}
			m.desiredWidth = w
			m.clientGen++ // in-flight reads carry the old width; drop them
			setWidthOption(w)
			if w != msg.Width {
				resizePane(w) // the readable floor kicked in
			}
		}
		m.width = msg.Width
		return m, nil
	case clientSessMsg:
		next := tickAfter(clientEvery, clientTickMsg{})
		// A read that started before the last switch or width change carries
		// the old world; applying it is the visible flicker. Drop it.
		if msg.gen != m.clientGen {
			return m, next
		}
		m.clients = msg.clients
		if msg.width >= minWidth && msg.width != m.desiredWidth {
			m.desiredWidth = msg.width
			if msg.width != m.width {
				resizePane(msg.width)
			}
		} else if msg.width == 0 && m.desiredWidth == 0 && m.width >= minWidth {
			// Bootstrap (#742): the shared option is written only by us, so an
			// empty read on a machine where nothing seeded it keeps desiredWidth
			// zero forever and the WindowSizeMsg enforcement never arms. This
			// pane got its width from the split that created it, so it IS the
			// intended value: adopt it and publish once for every other session.
			m.desiredWidth = m.width
			setWidthOption(m.width)
		}
		// tmux is the authority here — no grace window, no daemon reconciliation.
		// If the name is empty the read failed; keep the last known good value
		// rather than dropping the bar.
		if msg.name != "" && msg.name != m.cursorSess {
			m.cursorSess = msg.name
			// No row yet (brand-new session): the bar goes dark rather than
			// sitting on the card you just left; reanchor finds the row when
			// the daemon serves it.
			m.cursor = -1
			for i, r := range m.rows {
				if r.session == msg.name {
					m.cursor = i
					break
				}
			}
		}
		return m, next
	case clientTickMsg:
		return m, fetchClientSession(m.clientGen)
	case animTickMsg:
		m.frame++
		return m, tickAfter(animEvery, animTickMsg{})
	case fastTickMsg:
		return m, tea.Batch(fetchFast, fetchHooksWith(m.paneToSess))
	case slowTickMsg:
		return m, fetchSlow
	case fastDataMsg:
		m.err = msg.err
		if msg.err == nil {
			m.rows = msg.rows
			m.fastAt = time.Now()
			m.paneToSess = msg.paneToSess
			m.createdBySess = msg.createdBySess
			// The poll's attach flags were true up to a daemon poll ago and the
			// request itself took a moment, so a poll in flight across a switch
			// lands *after* the pushed snapshot carrying pre-switch attachment.
			// Letting it through reverted the selection and made a switch look
			// like it took a full poll cycle to land. The push lane is strictly
			// fresher, so it wins for as long as it is live.
			if m.subLive() {
				for i := range m.rows {
					m.rows[i].attached = m.attachedBySess[m.rows[i].session]
				}
			}
			m.rebuild()
		} else {
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
		}
		return m, tickAfter(fastEvery, fastTickMsg{})
	case hookDataMsg:
		m.hooksBySess = msg.bySession
		m.stateDirOK = msg.dirOK
		m.rebuild()
		return m, nil
	case tmuxSubMsg:
		// pushed snapshot: fresher than any poll, so it wins on attach and on
		// the maps. It does not touch state/model/title — those are the fast
		// lane's, and this message carries nothing about them.
		if msg.err != nil {
			m.subErr = msg.err
			return m, nil
		}
		m.subErr = nil
		m.subAt = time.Now()
		attached, created, p2s := foldSessions(msg.sessions)
		m.createdBySess, m.paneToSess, m.attachedBySess = created, p2s, attached
		live := map[string]bool{}
		for _, s := range msg.sessions {
			live[s.Name] = true
		}
		kept := m.rows[:0]
		have := map[string]bool{}
		for _, r := range m.rows {
			if !live[r.session] {
				continue // session is gone; don't leave a card that would attach to nothing
			}
			r.attached, r.created = attached[r.session], created[r.session]
			have[r.session] = true
			kept = append(kept, r)
		}
		m.rows = kept
		for _, s := range msg.sessions {
			if have[s.Name] {
				continue
			}
			// new session: shell until the fast lane reports a claude instance
			// in it, which is the same default fetchFast applies
			m.rows = append(m.rows, row{session: s.Name, state: "shell",
				attached: attached[s.Name], created: created[s.Name]})
		}
		m.rebuild()
		return m, nil
	case slowDataMsg:
		if msg.err == nil {
			m.wtBySession = msg.wtBySession
			m.repoBySess = msg.repoBySess
			m.wtByPath = msg.wtByPath
			m.repoByPath = msg.repoByPath
			m.join()
		}
		return m, tickAfter(slowEvery, slowTickMsg{})
	case copiedMsg:
		if msg.err == nil {
			m.copiedUntil = time.Now().Add(2 * time.Second)
		}
		return m, nil
	case tea.MouseMsg:
		// clicking a card is the same gesture as walking onto it with j/k:
		// selection and the attached session are one thing, not two. Git-box
		// lines copy instead of selecting — checked first, they map to no row.
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			if msg.Y >= 0 && msg.Y < len(m.lineToCopy) && m.lineToCopy[msg.Y] != "" {
				return m, copyCmd(m.lineToCopy[msg.Y])
			}
			if msg.Y >= 0 && msg.Y < len(m.lineToRow) {
				if ri := m.lineToRow[msg.Y]; ri >= 0 {
					m.selectRow(ri)
				}
			}
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "j", "down":
			m.selectRow(m.cursor + 1)
		case "k", "up":
			m.selectRow(m.cursor - 1)
		case "enter":
			m.selectRow(m.cursor)
		}
	}
	return m, nil
}

// selectRow moves the cursor to i and attaches that session. Selection IS the
// switch — there is no separate "jump" step and no cursor glyph, because the
// selected card is always the session you are looking at.
func (m *model) selectRow(i int) {
	if i < 0 || i >= len(m.rows) {
		return
	}
	m.cursor = i
	m.cursorSess = m.rows[i].session
	// Any client read already in flight predates this switch and would bounce
	// the bar back for a tick (the flicker); bumping the generation kills it.
	m.clientGen++
	switchClient(m.rows[i].session)
}

// ---- local tmux lane: which session is THIS client on, right now.
//
// The daemon's tmuxSessions.attached is per-SESSION and global — it answers
// "is anyone looking at this session", never "am I looking at it". It is also
// a poll behind. Both problems land on the one thing the sidebar most needs to
// be right and instant: which card carries the bar. So attach state is read
// from tmux directly, on a fast local tick.
//
// This is a deliberate exception to ADR-016/017/018 (clients don't exec tmux),
// taken on the user's explicit instruction after the daemon path measured too
// slow to use. The daemon still owns everything else: session inventory,
// claude state, model, PR/issue join. Tracked with switchClient under #726.
type clientSessMsg struct {
	name    string
	clients int // attached clients across all sessions; 0 = detached
	width   int // @orchard_sidebar_width at read time; 0 = unset or unreadable
	gen     int // m.clientGen when the read started; mismatched reads are stale
}

const clientEvery = 150 * time.Millisecond

// fetchClientSession reports the session of the most recently active client.
// With one client that is simply "where you are". With several, most-recent
// activity is the closest thing to "the one you are driving" — and unlike the
// daemon's flag it can never report two sessions at once.
//
// The shared pane width rides the same exec (width before session: names can
// contain spaces, so the free-form field must come last).
func fetchClientSession(gen int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "tmux", "list-clients", "-F",
			"#{client_activity} #{?@orchard_sidebar_width,#{@orchard_sidebar_width},0} #{client_session}").Output()
		if err != nil {
			return clientSessMsg{gen: gen}
		}
		clients, best, width := parseListClients(out)
		return clientSessMsg{name: best, clients: clients, width: width, gen: gen}
	}
}

// resizePane and setWidthOption are vars so tests can observe width traffic
// without a live tmux. Same client-side-exec exception as switchClient.
var resizePane = func(w int) {
	if p := os.Getenv("TMUX_PANE"); p != "" {
		exec.Command("tmux", "resize-pane", "-t", p, "-x", strconv.Itoa(w)).Start()
	}
}

var setWidthOption = func(w int) {
	exec.Command("tmux", "set-option", "-g", "@orchard_sidebar_width", strconv.Itoa(w)).Start()
}

// switchClient is a var so tests can observe the switch without a live tmux.
//
// KNOWN VIOLATION, tracked in orchardist#726. RULES L7 / M2 and ADR-018 line
// 20 put "switch tmux session" on the daemon, but no switchTmuxSession
// mutation exists yet — sendTextToPane is the only tmux mutation in the
// schema. Replace this exec with the mutation when #726 lands.
var switchClient = func(session string) {
	exec.Command("tmux", "switch-client", "-t", session).Start()
}

// daemonDown is the one judgment "the daemon is actually unreachable" —
// shared by the row wipe and the offline banner so they can never disagree.
// A recent fast-lane success holds through a transient spike, and a live push
// lane is itself proof of reachability.
func (m *model) daemonDown() bool {
	return m.err != nil && !m.subLive() &&
		(m.fastAt.IsZero() || time.Since(m.fastAt) > daemonGone)
}

// subLive reports whether the push lane is delivering. One missed keepalive
// window is enough slack that a momentary hiccup doesn't hand attach back to
// the poll; a real drop degrades to polling within it.
func (m *model) subLive() bool {
	return m.subErr == nil && !m.subAt.IsZero() && time.Since(m.subAt) < subFresh
}

// reanchorCursor keeps the cursor on the same session after a re-sort (rows
// reorder whenever a session changes state), falling back to the most recently
// active attached session before any user input has happened.
func (m *model) reanchorCursor() {
	if len(m.rows) == 0 {
		m.cursor = 0
		return
	}
	// Which session the bar sits on is owned by the local tmux lane
	// (clientSessMsg) — it is per-client and ~20ms fresh, where the daemon's
	// attached flag is per-session and a poll behind. So this function no
	// longer picks a row; it only re-finds the row cursorSess moved to after a
	// re-sort, and falls back to something sane on first paint.
	if m.cursorSess != "" {
		for i, r := range m.rows {
			if r.session == m.cursorSess {
				m.cursor = i
				return
			}
		}
		// the session is known but its row hasn't been served yet (brand-new
		// session): keep the bar parked rather than walking to a card the
		// user never chose — that would also clobber cursorSess.
		m.cursor = -1
		return
	}
	// first paint, before the local lane has answered: prefer any session the
	// daemon believes is attached, else the top row.
	best := 0
	for i, r := range m.rows {
		if r.attached {
			best = i
			break
		}
	}
	m.cursor = best
	m.cursorSess = m.rows[best].session
}

var (
	stySelBar  = lipgloss.NewStyle().Foreground(lipgloss.Color(neonPurple)).Bold(true)
	stySelName = lipgloss.NewStyle().Foreground(lipgloss.Color(neonAccent)).Bold(true)
	stySelHead = lipgloss.NewStyle().Foreground(lipgloss.Color(neonAccent)).Bold(true)
	stySelAge  = lipgloss.NewStyle().Foreground(lipgloss.Color(neonAccent)).Bold(true)
	styErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	styDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styPrompt  = lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("8"))
	// the selected card's body lifts out of the dim gray the rest of the list
	// sits in — a pale tint of the accent, so name/border still lead. The
	// prompt quote stays gray (just brighter than inactive): it's the one
	// body line that's prose, not chrome, and the tint read as over-styled
	stySelBody   = lipgloss.NewStyle().Foreground(lipgloss.Color(paleNeon))
	stySelPrompt = lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("7"))
)

// glyph returns the one-cell state marker; the state *word* lives in the
// group header only, so it isn't repeated on every row. All glyphs render in
// the shared dim style — state is encoded by group position, not color.
func glyph(state string) string {
	switch state {
	case "input":
		return "●"
	case "stalled":
		return "✖"
	case "working":
		return "◐"
	case "idle":
		return "○"
	default:
		return "·"
	}
}

// groupLabel is the section header shown once above each run of same-state
// rows (rows are already sorted by stateRank, so runs are contiguous).
func groupLabel(state string) string {
	switch state {
	case "input":
		return "Needs input"
	case "stalled":
		return "Stalled"
	case "working":
		return "Working"
	case "idle":
		return "Idle"
	default:
		return "Shell"
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

// trunc clips to n terminal cells (ANSI- and wide-rune-aware), first line only.
func trunc(s string, n int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	if n < 1 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

// line lays out left + right-aligned right within width cells, truncating
// left (never right) when they don't both fit.
func line(width int, left, right string) string {
	rw := ansi.StringWidth(right)
	avail := width - rw
	if rw > 0 {
		avail-- // one-cell gap
	}
	left = trunc(left, max(1, avail))
	pad := width - ansi.StringWidth(left) - rw
	if pad < 1 {
		pad = 1
	}
	if right == "" {
		return left
	}
	// final clamp: at pathological widths the 1-cell floors on avail/pad can
	// still overshoot; a soft-wrapped line would skew the lineToRow map
	return trunc(left+strings.Repeat(" ", pad)+right, width)
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
// formats live — the card's track line and the git box both render them.
func issueRef(n int) string { return fmt.Sprintf("issue#%d", n) }

func prRef(p prInfo) string { return fmt.Sprintf("pr#%d (%s)", p.Number, prStatus(p)) }

// trackLine renders "issue#N | pr#M (status…)", showing only the halves that
// exist — a "—" placeholder for the missing one is noise, not information.
// The status word comes from prStatus, whose narrowest-green ladder is the
// false-green guard (see its comment).
func trackLine(r row) string {
	var parts []string
	if r.issueNum > 0 {
		parts = append(parts, issueRef(r.issueNum))
	}
	if r.pr != nil {
		parts = append(parts, prRef(*r.pr))
	}
	return strings.Join(parts, " | ")
}

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

// dirLabel is the session's working directory (basename), repo slug fallback.
func dirLabel(r row) string {
	if r.cwd != "" {
		return filepath.Base(r.cwd)
	}
	return r.repo
}

// View layout (everything clipped to the live pane width):
//
//	sections — one dim cap-first header per contiguous state group (rows are
//	           sorted by state, so groups are runs), padded one cell in and
//	           followed by a blank line
//	cards    — every session, everything visible (nothing collapsed), all dim
//	           except the session name. The selected card carries a thick
//	           neon purple left border down its full height, including a
//	           padding line above and below its content; other cards keep a
//	           blank column there so nothing shifts on selection. No gap
//	           lines between cards — the padding lines make each group read
//	           continuous. The border is the only selection glyph
//	           (there is no cursor caret, because selecting a card attaches its
//	           tmux session: cursor and current session are one thing).
//	            ● name [model]     right-aligned age
//	              (blank)
//	              “first prompt” (italic)
//	              🌿 branch ↑a ↓b
//	              📁 directory
//	              issue#N | pr#M (one status word — see prStatus)
//	footer   — key hints
//
// A blank line pads the top, and below minWidth the sub-lines are dropped.
// minWidth is the narrowest pane the full card layout stays readable in.
// Below it the sub-lines (prompt, branch, dir, issue/pr) are dropped rather
// than shredded into one-word slivers — see compact mode in View().
const minWidth = 34

// neonAccent lights up the selected card — its name and the section title it
// sits under — so the eye finds the attached session in one jump. A truecolor
// hex rather than an ANSI index: the 16-color palette has no neon, and every
// one of its slots is already spoken for by a session state.
const neonAccent = "#00F0FF"

// neonPurple is the selected card's thick left border — the border is the
// focus signal, and only the attached session gets one.
const neonPurple = "#BF00FF"

// paleNeon is the selected card's body text — a pale tint of neonAccent, so
// the active card's details read clearly without competing with the name.
const paleNeon = "#A8F0FF"

func (m *model) View() string {
	w := m.width
	if w <= 0 {
		w = 42
	}
	compact := w < minWidth
	var b bytes.Buffer
	lineMap := []int{}
	copyMap := []string{}
	raw := func(s string, rowIdx int) {
		b.WriteString(s + "\n")
		lineMap = append(lineMap, rowIdx)
		copyMap = append(copyMap, "")
	}

	raw("", -1) // top padding — nothing butts against the pane's top edge

	// same judgment as the row wipe: a transient fast-lane error holds the
	// rows silently, so it must not also claim the daemon is offline
	if m.daemonDown() {
		raw(styErr.Render(trunc("⚠ DAEMON OFFLINE — hook states live", w)), -1)
		raw(styDim.Render(trunc(m.err.Error(), w)), -1)
	}

	// one-cell gutter for the attached-session indicator bar, two cells of
	// breathing room on the right so nothing butts against the pane edge
	iw := w - 3

	// selection and the attached session are the same thing (see selectRow), so
	// the gutter bar marks the cursor row — it moves the instant you press j/k
	// rather than waiting for the next daemon poll to report the new attach.
	curIdx := m.cursor

	prev := ""
	for i, r := range m.rows {
		if r.state != prev {
			if prev != "" {
				raw("", -1) // gap before the next group's header
			}
			// the selected card's own section title lights up with it
			headSty := styDim
			if curIdx >= 0 && curIdx < len(m.rows) && m.rows[curIdx].state == r.state {
				headSty = stySelHead
			}
			// right-aligned, opposite the cards' left border rail
			raw(" "+headSty.Render(line(iw, "", groupLabel(r.state))), -1)
			prev = r.state
		}
		// only the attached session gets a border — thick neon purple down
		// every line of the card. Other cards render a plain space in that
		// column so the layout doesn't shift on selection. No gap lines
		// between cards — each card carries a padding line above and below
		// instead, so the group reads continuous.
		isCur := i == curIdx
		pfx := " "
		if isCur {
			pfx = stySelBar.Render("█")
		}
		emit := func(s string, rowIdx int) { raw(pfx+s, rowIdx) }
		emit("", i) // top padding

		g := glyph(r.state)
		if r.state == "working" {
			g = workFrames[m.frame%len(workFrames)]
		}
		ageSty := styDim
		if isCur {
			ageSty = stySelAge
		}
		name := r.session
		bodySty, promptSty := styDim, styPrompt
		if isCur {
			name = stySelName.Render(r.session)
			bodySty, promptSty = stySelBody, stySelPrompt
		}
		left := fmt.Sprintf(" %s %s", styDim.Render(g), name)
		if !r.hooked && r.state != "shell" {
			left += bodySty.Render("?")
		}
		if r.model != "" {
			left += " " + bodySty.Render("["+r.model+"]")
		}
		emit(line(iw, left, ageSty.Render(age(r.lastAct))), i)
		if compact {
			// too narrow for detail lines to say anything useful — name only
			emit("", i) // bottom padding
			continue
		}

		if r.mission != "" {
			// truncate the mission first so the closing quote always survives
			emit(promptSty.Render(trunc("  “"+r.mission, iw-1)+"”"), i)
		}
		if bl := branchLine(r); bl != "" {
			emit(bodySty.Render(trunc("  "+bl, iw)), i)
		}
		if d := dirLabel(r); d != "" {
			emit(bodySty.Render(trunc("  📁 "+d, iw)), i)
		}
		if tl := trackLine(r); tl != "" {
			emit(bodySty.Render(trunc("  "+tl, iw)), i)
		}
		emit("", i) // bottom padding
	}

	// git box — the selected session's git facts laid out to be taken:
	// clicking any line copies its payload (branch, path, issue/PR URL)
	if !compact && curIdx >= 0 && curIdx < len(m.rows) {
		if items := gitBoxItems(m.rows[curIdx]); len(items) > 0 {
			raw("", -1)
			flash := time.Now().Before(m.copiedUntil)
			for _, bl := range gitBoxRender(items, iw, flash) {
				b.WriteString(bl.text + "\n")
				lineMap = append(lineMap, -1) // box clicks copy, never select
				copyMap = append(copyMap, bl.copy)
			}
		}
	}

	raw("", -1)
	if !m.stateDirOK {
		raw(styDim.Render(trunc("no state dir — install claude-session-state", w)), -1)
	}
	raw(" "+styDim.Render(trunc("j/k · click · q", iw)), -1)

	m.lineToRow = lineMap
	m.lineToCopy = copyMap
	return b.String()
}

func main() {
	prog := tea.NewProgram(&model{}, tea.WithAltScreen(), tea.WithMouseCellMotion())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go subscribeTmux(ctx, prog.Send)
	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
