package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// recover_log_test.go — issue #802 AC2: recovery.log must stay bounded.
//
// A misbehaving hold command can append to recovery.log without limit (a live
// run once wrote 1442 halt entries in ~8s). appendRecoveryLog rotates the file
// to its newest recoveryLogKeep events once it passes recoveryLogMaxBytes —
// by whole JSON lines, so the log stays parseable, the newest event survives,
// and the history/halt helpers keep returning correct values.

// writeRawRecoveryLog fills path with n whole JSON-line events in a single
// bulk write, so a test can build an over-cap log without paying the atomic
// rewrite of one appendRecoveryLog call per line.
func writeRawRecoveryLog(t *testing.T, path string, n int, msg string) {
	t.Helper()
	var buf bytes.Buffer
	for i := 0; i < n; i++ {
		line, err := json.Marshal(recoveryEvent{Pane: "inner", Action: actReattachInner, Message: msg, At: at(i)})
		if err != nil {
			t.Fatalf("marshal event %d: %v", i, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write raw log: %v", err)
	}
}

// realisticMessage approximates a real recovery message so a few thousand
// events cross the 1 MB byte cap (real events are ~200 bytes).
func realisticMessage() string {
	return "inner tmux exited (status 0) — reattached " + strings.Repeat("x", 120)
}

func TestAppendRecoveryLog_RotatesWhenOverCapKeepingNewest(t *testing.T) {
	path := t.TempDir() + "/recovery.log"

	// Enough realistic-sized events that both the byte cap (1 MB) and the event
	// cap (500) are well exceeded before the triggering append.
	const n = 8000
	writeRawRecoveryLog(t, path, n, realisticMessage())

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if before.Size() <= recoveryLogMaxBytes {
		t.Fatalf("seed log is %d bytes; want it over the %d cap to exercise rotation", before.Size(), recoveryLogMaxBytes)
	}

	// One append trips the rotation: trim to the newest recoveryLogKeep, then
	// append this event — the newest of all.
	newest := recoveryEvent{Pane: "inner", Action: actCrashLoopHalt, Message: "newest", At: at(n)}
	if err := appendRecoveryLog(path, newest); err != nil {
		t.Fatalf("appendRecoveryLog: %v", err)
	}

	events := readRecoveryEvents(path)

	// Bounded to exactly the retained window plus the one just appended.
	if want := recoveryLogKeep + 1; len(events) != want {
		t.Errorf("log holds %d events; want %d (keep %d + the appended one)", len(events), want, recoveryLogKeep)
	}

	// The newest event survives, via both read paths.
	last, ok := readLastRecoveryEvent(path)
	if !ok {
		t.Fatal("readLastRecoveryEvent reported no event after rotation")
	}
	if last.Message != "newest" || !last.At.Equal(at(n)) {
		t.Errorf("readLastRecoveryEvent = %+v; want the just-appended newest event", last)
	}
	if got := events[len(events)-1]; got.Message != "newest" {
		t.Errorf("newest readRecoveryEvents entry = %+v; want the appended one", got)
	}

	// Survivors are whole, ordered events — a byte truncation would break the
	// monotonic-timestamp chain (readRecoveryEvents silently drops a split line).
	for i := 1; i < len(events); i++ {
		if !events[i].At.After(events[i-1].At) {
			t.Fatalf("survivors not contiguous/ordered at %d: %v then %v", i, events[i-1].At, events[i].At)
		}
	}
}

// The history and halt helpers must keep working after a rotation: they read
// the same file, so a whole-line rotation leaves their answers correct for the
// events that survived.
func TestRecoveryHistoryAndLastHaltAt_SurviveRotation(t *testing.T) {
	path := t.TempDir() + "/recovery.log"

	const n = 8000
	writeRawRecoveryLog(t, path, n, realisticMessage())

	// A final halt is the newest event of all, and trips the rotation.
	haltAt := at(n)
	if err := appendRecoveryLog(path, recoveryEvent{Pane: "inner", Action: actCrashLoopHalt, Message: "halt", At: haltAt}); err != nil {
		t.Fatalf("appendRecoveryLog(halt): %v", err)
	}

	if got := lastHaltAt(path, "inner"); !got.Equal(haltAt) {
		t.Errorf("lastHaltAt = %v; want the newest halt %v", got, haltAt)
	}

	// recoveryHistory returns exactly the survivors' timestamps (all inner).
	hist := recoveryHistory(path, "inner")
	events := readRecoveryEvents(path)
	if len(hist) != len(events) {
		t.Errorf("recoveryHistory has %d entries; want %d (all survivors are inner)", len(hist), len(events))
	}
	if len(hist) == 0 || !hist[len(hist)-1].Equal(haltAt) {
		t.Errorf("recoveryHistory newest = %v; want %v", hist, haltAt)
	}
}

// appendSidebarLog must cap sidebar.log the same way recovery.log is capped
// (issue #802 follow-up): a crash loop appends this line once per pane death,
// and an uncapped writer is a slow disk leak on the one machine least able to
// notice it.
func TestAppendSidebarLog_BoundsOverCapLogKeepingNewest(t *testing.T) {
	path := t.TempDir() + "/sidebar.log"

	// One bulk write over the cap, so the test pays for exactly one append
	// (appendSidebarLogAt itself) rather than looping calls to build up size.
	line := "2024-01-01T00:00:00Z pre-existing sidebar diagnostic line\n"
	var buf bytes.Buffer
	for buf.Len() <= sidebarLogMaxBytes {
		buf.WriteString(line)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("seed sidebar.log: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if before.Size() <= sidebarLogMaxBytes {
		t.Fatalf("seed log is %d bytes; want it over the %d cap", before.Size(), sidebarLogMaxBytes)
	}

	appendSidebarLogAt(path, "sidebar exited (status 1)")

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after append: %v", err)
	}
	if after.Size() > sidebarLogMaxBytes {
		t.Errorf("sidebar.log is %d bytes after append; want <= cap %d", after.Size(), sidebarLogMaxBytes)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(got), "sidebar exited (status 1)") {
		t.Errorf("sidebar.log after rotation = %q; want it to contain the newest line", got)
	}
	if strings.Contains(string(got), "pre-existing sidebar diagnostic line") {
		t.Errorf("sidebar.log after rotation still contains pre-cap content; want it truncated")
	}
}
