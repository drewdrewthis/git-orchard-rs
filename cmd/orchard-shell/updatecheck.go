package main

// Step 10: wires internal/release's 24h-cached update check into
// orchard-shell's startup, network-free for the sidebar.
//
// The check runs in a DETACHED CHILD PROCESS, not a goroutine: attach()
// replaces this process image with syscall.Exec, and the --detach path exits
// immediately after printing, so nothing scheduled in-process would survive
// either one. orchard-shell instead re-invokes itself with a hidden flag; the
// child resolves internal/release.CheckPath() and calls
// internal/release.RefreshCheck, which writes
// {"checked_at","current","latest"} JSON to
// ${XDG_STATE_HOME:-~/.local/state}/orchard/update-check.json (see
// internal/release.CheckPath, internal/release.Check). The sidebar only ever
// reads that file; orchard-shell's own doctor check does the same (see
// doctor.go's update check).

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// updateCheckFlag is the hidden re-exec entrypoint run() recognizes before
// normal flag parsing, so it never appears in --help or Options.
const updateCheckFlag = "--internal-update-check"

// updateCheckTimeout bounds the child's GitHub round trip so a hung network
// never leaves an orphaned process behind.
const updateCheckTimeout = 5 * time.Second

// processStarter starts a prepared command without waiting for it — the
// injection seam so spawnUpdateCheck is testable without spawning a real
// process.
type processStarter func(*exec.Cmd) error

// startDetached is the production processStarter.
func startDetached(cmd *exec.Cmd) error { return cmd.Start() }

// spawnUpdateCheck re-invokes self as a detached child that runs the update
// check, then returns immediately without waiting on it. It never surfaces
// an error: a background check that fails to launch is invisible, the same
// contract RefreshCheck itself carries for network failures.
//
// self == "" (os.Executable failed) or ORCHARD_NO_UPDATE_CHECK set both skip
// spawning entirely.
func spawnUpdateCheck(start processStarter, self, currentVersion string) {
	if self == "" || os.Getenv(release.NoCheckEnv) != "" {
		return
	}
	cmd := exec.Command(self, updateCheckFlag, currentVersion)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = start(cmd)
}

// runInternalUpdateCheck is the detached child's entrypoint (see
// spawnUpdateCheck). It resolves the cache path and refreshes it, silently
// on every failure — this runs with no terminal attached, so nothing it does
// is ever visible to a user.
func runInternalUpdateCheck(currentVersion string) {
	path, err := release.CheckPath()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
	defer cancel()
	release.RefreshCheck(ctx, path, currentVersion, time.Now(), release.CheckTTL)
}
