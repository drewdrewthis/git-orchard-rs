package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

var graphqlURL = "http://127.0.0.1:7777/graphql"

const fastEvery = 2 * time.Second

const slowEvery = 30 * time.Second

const fastQuery = `{ workView {
  claudeInstances { state sessionUuid model pane { window { session { name } } } lastActivityAt }
  tmuxSessions { name attached windows { panes { paneId } } }
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
	Name     string `json:"name"`
	Attached bool   `json:"attached"`
	Windows  []struct {
		Panes []struct {
			PaneId string `json:"paneId"`
		} `json:"panes"`
	} `json:"windows"`
}

// foldSessions derives what the view needs from a session snapshot: attach
// state, and the pane->session map that the state-dir lane folds its files
// against.
func foldSessions(ss []tmuxSession) (attached map[string]bool, p2s map[string]string) {
	attached, p2s = map[string]bool{}, map[string]string{}
	for _, s := range ss {
		attached[s.Name] = s.Attached
		for _, w := range s.Windows {
			for _, pn := range w.Panes {
				p2s[pn.PaneId] = s.Name
			}
		}
	}
	return attached, p2s
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

	_, p2s := foldSessions(wv.TmuxSessions)
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
	return fastDataMsg{rows: rows, paneToSess: p2s}
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
