package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// logf records one diagnostic line in the sidebar's log file instead of on
// stderr.
//
// The sidebar holds the alt screen for its whole life, so anything written to
// stderr lands IN its pane, on top of the UI, and survives until something
// repaints over that exact cell. A handful of failed switch-clients (a wrapper
// whose pane 0.1 was a plain shell rather than an inner tmux client, so
// ORCHARD_TMUX_CLIENT named no client at all) shredded the live sidebar into
// unreadable strips of half-drawn cards — observed 2026-09-02, and every tmux
// exec here is one more way to reproduce it.
//
// Failures the user has to act on belong in the UI instead (the row menu's
// notice line, the offline banner). This is the trail for everything else, and
// it is best-effort by design: a sidebar that cannot open its log still has a
// job to do.

// logMax caps the file. A tmux exec can fail every tick — a wedged inner
// server, a stale client tty — so an uncapped append-only log is a slow disk
// leak on the one machine least able to notice it. At the cap the file starts
// over: the newest failures are the ones worth having.
const logMax = 1 << 20

var (
	logMu   sync.Mutex
	logDest *os.File // held open for the process's life; nil until first use
	logSize int64
)

func logf(format string, args ...any) {
	line := fmt.Sprintf("%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	logMu.Lock()
	defer logMu.Unlock()
	f := logHandle()
	if f == nil {
		return
	}
	if logSize+int64(len(line)) > logMax {
		// O_APPEND writes go to the end of the file, so truncating to zero is
		// enough to start over — no seek, which O_APPEND would ignore anyway.
		if f.Truncate(0) == nil {
			logSize = 0
		}
	}
	n, err := f.WriteString(line)
	logSize += int64(n)
	if err != nil {
		// a log that cannot be written is not worth a second try every tick
		_ = f.Close()
		logDest = nil
	}
}

// logHandle opens the log once and keeps it: reopening per line cost an
// open/close pair on every failing tmux exec, which is exactly the situation
// where they arrive fastest.
func logHandle() *os.File {
	if logDest != nil {
		return logDest
	}
	p := stateFile("sidebar.log")
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return nil
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	logSize = 0
	if fi, err := f.Stat(); err == nil {
		logSize = fi.Size()
	}
	logDest = f
	return f
}
