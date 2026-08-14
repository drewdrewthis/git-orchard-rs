package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// pollAttach reports a named row's attach flag as a printable word.
func pollAttach(rows []row, name string) string {
	for _, r := range rows {
		if r.session == name {
			return map[bool]string{true: "true", false: "false"}[r.attached]
		}
	}
	return "absent"
}

// TestLiveSwitchNoRevert drives BOTH real lanes (2s poll + websocket push) against
// the live daemon and a real tmux client, then switches that client and watches the
// row's attach flag. The bug this guards is not "attach never arrives" but "attach
// arrives, then an in-flight poll reverts it" — so it measures both first-true and
// any revert inside the window after it.
func TestLiveSwitchNoRevert(t *testing.T) {
	if os.Getenv("ORCHARD_LIVE") != "1" {
		t.Skip("live only")
	}
	tm := func(args ...string) string {
		out, _ := exec.Command("tmux", args...).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	tm("kill-session", "-t", "zzA")
	tm("kill-session", "-t", "zzB")
	tm("new-session", "-d", "-s", "zzA")
	tm("new-session", "-d", "-s", "zzB")
	defer tm("kill-session", "-t", "zzA")
	defer tm("kill-session", "-t", "zzB")
	tm("new-window", "-d", "-t", "zzB", "TMUX= tmux attach -t zzA")
	time.Sleep(1500 * time.Millisecond)
	var tty string
	for _, ln := range strings.Split(tm("list-clients", "-F", "#{client_tty} #{client_session}"), "\n") {
		if strings.HasSuffix(ln, " zzA") {
			tty = strings.Fields(ln)[0]
		}
	}
	if tty == "" {
		t.Fatal("no probe client on zzA")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	var mu sync.Mutex
	m := &model{}
	type sample struct {
		at   time.Time
		att  bool
		lane string
	}
	var log []sample
	var t0 time.Time
	feed := func(msg tea.Msg, lane string) {
		mu.Lock()
		defer mu.Unlock()
		m.Update(msg)
		if t0.IsZero() {
			return
		}
		for _, r := range m.rows {
			if r.session == "zzB" {
				log = append(log, sample{time.Now(), r.attached, lane})
			}
		}
	}
	go subscribeTmux(ctx, func(msg tea.Msg) { feed(msg, "push") })
	go func() { // the real fast lane, same interval as production
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			msg := fetchFast()
			fd := msg.(fastDataMsg)
			mu.Lock()
			if !t0.IsZero() {
				t.Logf("+%.2fs  POLL returned err=%v rows=%d zzB_attached=%v",
					time.Since(t0).Seconds(), fd.err, len(fd.rows), pollAttach(fd.rows, "zzB"))
			}
			mu.Unlock()
			feed(msg, "poll")
			time.Sleep(fastEvery)
		}
	}()
	time.Sleep(4 * time.Second) // let both lanes settle on the pre-switch world

	mu.Lock()
	t0 = time.Now()
	mu.Unlock()
	tm("switch-client", "-c", tty, "-t", "zzB")
	time.Sleep(8 * time.Second) // long enough for several poll cycles to land

	mu.Lock()
	defer mu.Unlock()
	var first time.Duration = -1
	reverts := 0
	seenTrue := false
	for _, s := range log {
		t.Logf("+%.2fs  %-4s zzB.attached=%v", s.at.Sub(t0).Seconds(), s.lane, s.att)
		if s.att && !seenTrue {
			seenTrue = true
			first = s.at.Sub(t0)
		} else if seenTrue && !s.att {
			reverts++
		}
	}
	if !seenTrue {
		t.Fatal("zzB never showed attached")
	}
	t.Logf("RESULT: first attached=true at %.2fs, reverts after that = %d", first.Seconds(), reverts)
	if reverts > 0 {
		t.Errorf("attach was reverted %d time(s) by a stale lane — this is the 4s bug", reverts)
	}
}
