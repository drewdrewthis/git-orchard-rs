// Tests for tmuxServerResolver.Sessions sort behaviour.

package resolvers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
	tmuxprovider "github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// listAllRow builds a single 18-field list-panes -a -F line as expected by
// the tmux adapter's listAll parser. We only care about the session-level
// fields here; window/pane fields get safe defaults.
func listAllRow(sessionName string, lastActivityUnix int64, paneID string, pid int) string {
	const fs = tmuxprovider.FieldSep
	return strings.Join([]string{
		sessionName,                          // 0  session_name
		"1700000000",                         // 1  session_created
		"0",                                  // 2  session_attached
		fmt.Sprintf("%d", lastActivityUnix),  // 3  session_activity
		"1",                                  // 4  session_windows
		"0",                                  // 5  session_window_index
		"0",                                  // 6  window_index
		"main",                               // 7  window_name
		"1",                                  // 8  window_active
		"1",                                  // 9  window_panes
		paneID,                               // 10 window_active_pane
		paneID,                               // 11 pane_id
		"",                                   // 12 pane_title
		"zsh",                                // 13 pane_current_command
		fmt.Sprintf("%d", pid),               // 14 pane_pid
		"200",                                // 15 pane_width
		"50",                                 // 16 pane_height
		"0",                                  // 17 pane_dead
	}, fs)
}

// sessionsRunner serves only the three commands the adapter calls: info,
// list-panes -a, list-clients. The runner returns one fixed list-panes block
// for every call.
type sessionsRunner struct {
	listAllOutput string
}

func (r *sessionsRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "tmux" {
		return nil, fmt.Errorf("unexpected command: %s", name)
	}
	sub := firstNonFlagTmuxArg(args)
	switch sub {
	case "info":
		return []byte("ok\n"), nil
	case "list-panes":
		return []byte(r.listAllOutput), nil
	case "list-clients":
		return []byte(""), nil
	default:
		return []byte(""), nil
	}
}

func buildSessionsResolver(t *testing.T, rows []string) *tmuxServerResolver {
	t.Helper()
	out := strings.Join(rows, "\n") + "\n"
	tr := &sessionsRunner{listAllOutput: out}
	adapter := tmuxprovider.NewAdapter(tmuxprovider.HostID("local")).
		WithRunner(tr).
		WithSocket("/tmp/orchard-test-tmuxserver.sock")
	prov := tmuxprovider.New(adapter, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := prov.Refresh(ctx); err != nil {
		t.Fatalf("tmux Refresh: %v", err)
	}
	return &tmuxServerResolver{&Resolver{Tmux: prov}}
}

// TestTmuxServerSessions_DefaultSortLastActivity asserts that with no sort
// arg, sessions come back ordered by lastActivityAt DESC.
func TestTmuxServerSessions_DefaultSortLastActivity(t *testing.T) {
	r := buildSessionsResolver(t, []string{
		listAllRow("oldest", 1700000000, "%1", 100),
		listAllRow("newest", 1700001000, "%2", 200),
		listAllRow("middle", 1700000500, "%3", 300),
	})

	got, err := r.Sessions(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	want := []string{"newest", "middle", "oldest"}
	if names := sessionNames(got); !equalNames(names, want) {
		t.Errorf("Sessions order = %v, want %v", names, want)
	}
}

// TestTmuxServerSessions_SortByName asserts NAME forces stable lex order.
func TestTmuxServerSessions_SortByName(t *testing.T) {
	r := buildSessionsResolver(t, []string{
		listAllRow("charlie", 1700001000, "%1", 100),
		listAllRow("alpha", 1700000000, "%2", 200),
		listAllRow("bravo", 1700000500, "%3", 300),
	})

	key := graphql1.TmuxSessionSortName
	got, err := r.Sessions(context.Background(), nil, &key)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	want := []string{"alpha", "bravo", "charlie"}
	if names := sessionNames(got); !equalNames(names, want) {
		t.Errorf("Sessions order = %v, want %v", names, want)
	}
}

// TestTmuxServerSessions_LastActivityTiebreakByName asserts that two sessions
// with identical lastActivityAt fall back to lex-lower name first.
func TestTmuxServerSessions_LastActivityTiebreakByName(t *testing.T) {
	r := buildSessionsResolver(t, []string{
		listAllRow("zebra", 1700001000, "%1", 100),
		listAllRow("alpha", 1700001000, "%2", 200),
		listAllRow("middle", 1700001000, "%3", 300),
	})

	key := graphql1.TmuxSessionSortLastActivity
	got, err := r.Sessions(context.Background(), nil, &key)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	want := []string{"alpha", "middle", "zebra"}
	if names := sessionNames(got); !equalNames(names, want) {
		t.Errorf("Sessions order = %v, want %v", names, want)
	}
}

func sessionNames(sessions []*graphql1.TmuxSession) []string {
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = s.Name
	}
	return out
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
