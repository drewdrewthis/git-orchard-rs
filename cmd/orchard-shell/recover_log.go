package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/drewdrewthis/orchardist/internal/orchpaths"
	"github.com/drewdrewthis/orchardist/internal/release"
)

// recoveryLogMaxBytes / recoveryLogKeep bound recovery.log. A hold command
// misbehaving can grow the log without limit (a live run once wrote 1442 halt
// entries in ~8s), so once it passes the byte cap it is rewritten to only its
// newest recoveryLogKeep events — by whole JSON lines, never a byte truncation
// that could split a line and break readRecoveryEvents.
const (
	recoveryLogMaxBytes = 1 << 20 // 1 MB
	recoveryLogKeep     = 500
)

// recover_log.go — issue #796: the recovery log's filesystem I/O.
//
// recover-pane appends one recoveryEvent per action here; doctor reads the
// most recent one back, and decideRecovery's inputs (restart history, last
// halt) are derived from the same file. The pure decision that consumes these
// lives in recover.go — this file is the only place recovery touches disk.

// recoveryLogPath is the recovery log, alongside the sidebar's own log under
// the orchard state dir.
func recoveryLogPath() (string, error) {
	dir, err := orchpaths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "recovery.log"), nil
}

// appendRecoveryLog appends ev as one JSON line, creating the file and its
// directory on first write.
func appendRecoveryLog(path string, ev recoveryEvent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := rotateRecoveryLog(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// rotateRecoveryLog keeps recovery.log bounded. While the file is at or under
// recoveryLogMaxBytes it does nothing; once it grows past the cap it rewrites
// the file to only its newest recoveryLogKeep events and swaps that in
// atomically via release.Replace, so a crash mid-rewrite never leaves a torn
// log. Rotation is by whole events, so readRecoveryEvents and the history/halt
// helpers keep parsing the survivors and the newest event is always retained.
func rotateRecoveryLog(path string) error {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= recoveryLogMaxBytes {
		return nil // missing or still under the cap: nothing to rotate
	}
	events := readRecoveryEvents(path)
	if len(events) <= recoveryLogKeep {
		return nil // over the byte cap but not the event cap: leave it
	}
	events = events[len(events)-recoveryLogKeep:]

	var buf bytes.Buffer
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	// mode 0: preserve the log's existing file permissions.
	return release.Replace(path, buf.Bytes(), 0)
}

// readRecoveryEvents reads every event in the log, oldest first. A missing
// log is not an error: there is simply no history yet.
func readRecoveryEvents(path string) []recoveryEvent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var events []recoveryEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev recoveryEvent
		if json.Unmarshal(line, &ev) == nil {
			events = append(events, ev)
		}
	}
	return events
}

// readLastRecoveryEvent returns the most recently appended event, or ok=false
// when the log has never been written.
func readLastRecoveryEvent(path string) (recoveryEvent, bool) {
	events := readRecoveryEvents(path)
	if len(events) == 0 {
		return recoveryEvent{}, false
	}
	return events[len(events)-1], true
}

// recoveryHistory returns the restart timestamps recorded for one pane, for
// the crash-loop bound.
func recoveryHistory(path, pane string) []time.Time {
	var out []time.Time
	for _, ev := range readRecoveryEvents(path) {
		if ev.Pane == pane {
			out = append(out, ev.At)
		}
	}
	return out
}

// lastHaltAt returns when this pane was most recently halted, or the zero time
// if it never was — feeding decideRecovery's halt debounce.
func lastHaltAt(path, pane string) time.Time {
	var t time.Time
	for _, ev := range readRecoveryEvents(path) {
		if ev.Pane == pane && ev.Action == actCrashLoopHalt {
			t = ev.At
		}
	}
	return t
}
