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

// TestLiveInFlightPollCannotRevertAttach stages the exact race behind the
// user-reported ~4s lag, deterministically rather than by luck: fastQuery is
// measured at ~1.2-1.5s round-trip, so a poll fired 1s before a switch carries
// pre-switch attach flags and lands just *after* the ~0.5s pushed snapshot.
// Pre-fix that poll won and the selection snapped back; the next clean value
// only arrived a full poll cycle later.
func TestLiveInFlightPollCannotRevertAttach(t *testing.T) {
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
	var t0 time.Time
	att := func() string {
		for _, r := range m.rows {
			if r.session == "zzB" {
				return map[bool]string{true: "true", false: "false"}[r.attached]
			}
		}
		return "absent"
	}
	go subscribeTmux(ctx, func(msg tea.Msg) {
		mu.Lock()
		defer mu.Unlock()
		m.Update(msg)
		if !t0.IsZero() {
			t.Logf("+%.2fs  push landed -> zzB.attached=%s", time.Since(t0).Seconds(), att())
		}
	})
	// seed the model with a full poll so rows exist before the switch
	mu.Lock()
	seed := fetchFast().(fastDataMsg)
	t.Logf("seed poll: err=%v rows=%d", seed.err, len(seed.rows))
	m.Update(seed)
	mu.Unlock()
	time.Sleep(2 * time.Second)

	stale := make(chan tea.Msg, 1)
	go func() { stale <- fetchFast() }() // fired ~1s BEFORE the switch: pre-switch truth
	time.Sleep(1 * time.Second)
	mu.Lock()
	t0 = time.Now()
	mu.Unlock()
	tm("switch-client", "-c", tty, "-t", "zzB")

	msg := <-stale
	mu.Lock()
	pre := att()
	fd := msg.(fastDataMsg)
	t.Logf("in-flight poll: err=%v rows=%d carried_zzB_attached=%s", fd.err, len(fd.rows), pollAttach(fd.rows, "zzB"))
	m.Update(msg)
	post := att()
	dt := time.Since(t0).Seconds()
	mu.Unlock()
	t.Logf("+%.2fs  in-flight poll landed: zzB.attached %s -> %s", dt, pre, post)
	if pre == "true" && post == "false" {
		t.Errorf("REVERTED: the stale in-flight poll undid the pushed attach — this is the reported lag")
	}
	if pre != "true" {
		t.Logf("note: push had not landed yet when the poll returned (%.2fs); race not exercised this run", dt)
	}
}
