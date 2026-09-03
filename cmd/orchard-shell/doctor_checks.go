package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// --- tmux version ------------------------------------------------------

// tmuxVersionRe pulls the first major.minor pair out of tmux -V's output —
// "tmux 3.6a", "tmux next-3.4" and similar all carry it as a plain
// substring.
var tmuxVersionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// minTmuxMajor/minTmuxMinor is the deploy target: Ubuntu aarch64 ships tmux
// 3.4.
const (
	minTmuxMajor = 3
	minTmuxMinor = 4
)

func checkTmuxVersion(env doctorEnv) checkResult {
	out, err := env.tmux("-V")
	return evaluateTmuxVersion(out, err)
}

// evaluateTmuxVersion is checkTmuxVersion's pure decision, given tmux -V's
// output (or the error running it).
func evaluateTmuxVersion(output string, err error) checkResult {
	const remedy = "install tmux >= 3.4 (e.g. apt install tmux, brew install tmux)"
	if err != nil {
		return checkResult{ID: "tmux", Status: statusFail,
			Detail: fmt.Sprintf("tmux -V failed: %v", err), Remedy: remedy}
	}
	m := tmuxVersionRe.FindStringSubmatch(output)
	if m == nil {
		return checkResult{ID: "tmux", Status: statusFail,
			Detail: fmt.Sprintf("could not parse a version from %q", output), Remedy: remedy}
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major > minTmuxMajor || (major == minTmuxMajor && minor >= minTmuxMinor) {
		return checkResult{ID: "tmux", Status: statusPass, Detail: output}
	}
	return checkResult{ID: "tmux", Status: statusFail,
		Detail: fmt.Sprintf("%s is older than the required %d.%d", output, minTmuxMajor, minTmuxMinor),
		Remedy: remedy}
}

// --- tmux nesting ------------------------------------------------------

// checkTmuxNesting warns rather than fails: doctor itself runs fine inside
// tmux — only orchard shell's own attach refuses to nest (see main.go's
// attach).
func checkTmuxNesting() checkResult {
	if os.Getenv("TMUX") != "" {
		return checkResult{ID: "tmux-nesting", Status: statusWarn,
			Detail: "$TMUX is set — you are already inside a tmux client; orchard shell's attach will refuse to nest"}
	}
	return checkResult{ID: "tmux-nesting", Status: statusPass, Detail: "$TMUX is not set"}
}

// --- inner socket --------------------------------------------------------

func checkInnerSocket(env doctorEnv) checkResult {
	socket := cmp.Or(env.innerSocket, defaultInnerSocket)
	out, err := env.tmux(innerArgs(socket, "list-sessions")...)
	if err != nil {
		return checkResult{ID: "inner-socket", Status: statusFail,
			Detail: fmt.Sprintf("no tmux server with sessions on socket %q", socket),
			Remedy: "orchard new   (or: tmux -L " + socket + " new -s work)"}
	}
	n := 0
	if out != "" {
		n = len(strings.Split(out, "\n"))
	}
	return checkResult{ID: "inner-socket", Status: statusPass,
		Detail: fmt.Sprintf("socket %q has %d session(s)", socket, n)}
}

// --- outer socket --------------------------------------------------------

// checkOuterSocket reuses the wrapper's own probe/decide — the exact
// decision orchard shell itself makes on startup (outer.go).
func checkOuterSocket(env doctorEnv) checkResult {
	if env.confErr != nil {
		return checkResult{ID: "outer-socket", Status: statusFail,
			Detail: fmt.Sprintf("could not resolve outer tmux config: %v", env.confErr)}
	}
	w := &wrapper{
		opts: Options{
			OuterSocket: cmp.Or(env.outerSocket, defaultOuterSocket),
			InnerSocket: cmp.Or(env.innerSocket, defaultInnerSocket),
		},
		conf: env.conf, tmux: env.tmux, log: io.Discard,
	}
	switch decide(w.probe()) {
	case actionBoot:
		return checkResult{ID: "outer-socket", Status: statusPass,
			Detail: "no outer wrapper session yet — orchard shell will create one"}
	case actionRespawn:
		return checkResult{ID: "outer-socket", Status: statusWarn,
			Detail: "outer wrapper session exists but its inner client is dead",
			Remedy: "orchard shell   (respawns it automatically)"}
	case actionRebuild:
		return checkResult{ID: "outer-socket", Status: statusWarn,
			Detail: fmt.Sprintf("outer session %q does not have the expected two-pane shape", outerSessionName),
			Remedy: "orchard shell   (rebuilds it automatically)"}
	default: // actionAttach
		return checkResult{ID: "outer-socket", Status: statusPass, Detail: "outer wrapper session is healthy"}
	}
}

// --- systemd ---------------------------------------------------------------

func checkSystemd(ctx context.Context, env doctorEnv) checkResult {
	if env.goos != "linux" {
		return checkResult{ID: "systemd", Status: statusPass,
			Detail: fmt.Sprintf("%s does not use systemd; check via: launchctl print gui/$(id -u)/com.orchard.daemon", env.goos)}
	}
	out, err := env.run(ctx, "systemctl", "--user", "is-active", "orchard")
	return evaluateSystemd(out, err)
}

// evaluateSystemd is checkSystemd's pure decision on a Linux host.
func evaluateSystemd(output string, err error) checkResult {
	if err == nil {
		return checkResult{ID: "systemd", Status: statusPass, Detail: "orchard.service is active"}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return checkResult{ID: "systemd", Status: statusWarn, Detail: "systemctl not found — not a systemd host"}
	}
	return checkResult{ID: "systemd", Status: statusFail,
		Detail: fmt.Sprintf("orchard.service is not active (systemctl reports %q)", output),
		Remedy: "systemctl --user start orchard"}
}

// --- PATH --------------------------------------------------------------------

func checkPath(self, pathEnv string) checkResult {
	if self == "" {
		return checkResult{ID: "path", Status: statusWarn, Detail: "could not resolve this binary's own path"}
	}
	dir := filepath.Dir(self)
	for _, p := range filepath.SplitList(pathEnv) {
		if p == dir {
			return checkResult{ID: "path", Status: statusPass, Detail: dir + " is on $PATH"}
		}
	}
	return checkResult{ID: "path", Status: statusFail,
		Detail: dir + " is not on $PATH",
		Remedy: fmt.Sprintf("add it to $PATH, e.g.: export PATH=%s:$PATH", dir)}
}
