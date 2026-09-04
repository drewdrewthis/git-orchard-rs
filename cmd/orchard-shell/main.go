// Command orchard-shell boots (or reattaches to) the outer tmux wrapper: a
// two-pane session with the orchard sidebar pinned on the left and a nested
// client onto the user's own tmux server on the right.
//
// Per ADR-013 it is dispatched as `orchard shell`; users do not normally
// invoke orchard-shell directly. It replaces scripts/outer-shell/launch.sh —
// every behaviour that script encoded is preserved here, with its reason (see
// outer.go and tmux.go), plus the things a script could not do: a version
// stamp, self-location for finding its own sidebar, and an embedded
// outer.conf so the wrapper works on a machine with no orchard checkout.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// version is overridden via -ldflags at release time.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable body. It returns the process exit code.
//
// Usage errors exit 1, not the conventional 2: exit 2 is reserved for "the
// session you asked for is not there", which is the one failure a caller
// scripts around.
func run(argv []string, stdout, stderr io.Writer) int {
	// --revision prints the bare VCS revision before any other dispatch, so the
	// doctor suite-revisions check can compare it across binaries (orchardist#787).
	if release.HandleRevisionFlag(argv, stdout) {
		return 0
	}
	if len(argv) == 2 && argv[0] == updateCheckFlag {
		runInternalUpdateCheck(argv[1])
		return 0
	}
	if len(argv) > 0 && argv[0] == "recover-pane" {
		return runRecoverPane(argv[1:], stderr)
	}
	opts, err := parseArgs(argv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "orchard shell: %v\n", err)
		return 1
	}
	if opts.ShowVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if opts.Doctor {
		return runDoctor(opts, stdout, stderr)
	}

	conf, err := resolveConfFor(opts.Conf, selfPath(), opts.InnerSocket, opts.OuterSocket)
	if err != nil {
		fmt.Fprintf(stderr, "orchard shell: %v\n", err)
		return 1
	}
	w := &wrapper{opts: opts, conf: conf, tmux: runTmux, log: stderr, lookPath: exec.LookPath}
	if err := w.ensureReady(); err != nil {
		fmt.Fprintf(stderr, "orchard shell: %v\n", err)
		return exitCodeFor(err)
	}
	spawnUpdateCheck(startDetached, selfPath(), version)
	if opts.Detach {
		fmt.Fprintf(stdout, "outer session %q is up on socket %q; attach with: orchard shell --outer-socket %s\n",
			outerSessionName, opts.OuterSocket, opts.OuterSocket)
		return 0
	}
	if err := attach(w); err != nil {
		fmt.Fprintf(stderr, "orchard shell: %v\n", err)
		return 1
	}
	return 0 // unreachable: attach either execs away or returns an error
}

// ensureReady brings the wrapper to a state that is safe to attach to.
func (w *wrapper) ensureReady() error {
	switch decide(w.probe()) {
	case actionBoot:
		session, err := w.resolveSession()
		if err != nil {
			return err
		}
		if err := w.boot(session); err != nil {
			return err
		}
	case actionRespawn:
		session, err := w.resolveSession()
		if err != nil {
			return err
		}
		if err := w.respawn(session); err != nil {
			return err
		}
	case actionRebuild:
		session, err := w.resolveSession()
		if err != nil {
			return err
		}
		if err := w.rebuild(session); err != nil {
			return err
		}
	}
	// After the inner server is confirmed present (every path above that
	// reaches here has either resolved a session on it or found a live inner
	// client via probe) — never before, so a missing inner server still fails
	// fast and mutates nothing (AC3).
	w.disarmDetachOnDestroy()
	return w.focusInner()
}

func exitCodeFor(err error) int {
	var missing *sessionMissingError
	if errors.As(err, &missing) {
		return exitSessionMissing
	}
	return 1
}

// attach replaces this process with the tmux client.
//
// exec, not a child process: once the client is up the wrapper has nothing
// left to do, and an intermediate process would sit between the user's
// terminal and tmux for every signal, resize and exit status.
func attach(w *wrapper) error {
	if os.Getenv("TMUX") != "" {
		fmt.Fprintln(w.log, "orchard shell: $TMUX is set — tmux will refuse to attach inside an existing session. Run this outside tmux, or use --detach.")
	}
	path, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found on $PATH: %w", err)
	}
	argv := append([]string{"tmux"}, outerArgs(w.opts.OuterSocket, w.conf, "attach", "-t", outerSessionName)...)
	return syscall.Exec(path, argv, os.Environ())
}
