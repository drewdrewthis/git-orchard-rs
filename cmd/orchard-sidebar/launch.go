package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The "launch a session" modal, run as `orchard-sidebar launch` inside a tmux
// popup (see openLaunchPopup). It is the same binary so there is one build to
// keep in sync, and a popup because it is the one modal that needs a full
// window and a program of its own — the modal rule in
// docs/outer-shell-prototype.md.
//
// ADR-016 (the daemon owns tmux) is bent here, not honoured: this creates the
// session with `tmux new-session` on the inner socket, the same client-side
// shim the sidebar already uses for switch-client. The daemon does have a
// launchSession mutation, but it only launches Claude — cwd/name/model/prompt,
// no arbitrary command — and the point of this modal is "run the last command
// again, wherever I say". Replacing this with a daemon mutation that takes a
// command is the follow-up; it is noted in docs/outer-shell-prototype.md.

// lastLaunch is what the modal remembers between runs, so "the last command"
// survives a restart of everything.
type lastLaunch struct {
	Cmd  string `json:"cmd"`
	Dir  string `json:"dir"`
	Name string `json:"name"`
	At   string `json:"at"`
}

// defaultCmd is the prefill before anything has ever been launched from here.
// Every session this sidebar watches is a Claude session, so that is the guess
// worth making — and the field is editable anyway.
const defaultCmd = "claude"

func lastLaunchPath() string { return stateFile("sidebar-last-launch.json") }

// loadLastLaunch never fails: a missing or corrupt file just means no memory,
// and the modal opens on its defaults rather than refusing to open.
func loadLastLaunch() lastLaunch {
	var l lastLaunch
	b, err := os.ReadFile(lastLaunchPath())
	if err != nil {
		return lastLaunch{}
	}
	if json.Unmarshal(b, &l) != nil {
		return lastLaunch{}
	}
	return l
}

func saveLastLaunch(l lastLaunch) error {
	l.At = time.Now().UTC().Format(time.RFC3339)
	p := lastLaunchPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// uniqueName resolves a session name collision the way tmux users expect:
// "orchard", then "orchard-2", "orchard-3". tmux would otherwise refuse the
// new-session outright, which reads as the button not working. The modal shows
// the resolved name before you launch (launchform.go): a name silently
// rewritten at launch time is a session you then cannot find.
func uniqueName(base string, taken map[string]bool) string {
	base = sessionSafe(base)
	if base == "" {
		base = "session"
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s-%d", base, i)
		if !taken[c] {
			return c
		}
	}
}

// sessionSafe strips what tmux treats as structure in a target: a session name
// containing "." or ":" makes every later `-t name` ambiguous.
func sessionSafe(s string) string {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer(".", "-", ":", "-", " ", "-").Replace(s)
	return strings.Trim(s, "-")
}

// takenSessions asks the inner server what already exists. A failure (daemon
// down, socket gone) returns an empty set: a name collision then surfaces as
// tmux's own error, which is better than refusing to launch.
var takenSessions = func() map[string]bool {
	out, err := env.innerCmd("list-sessions", "-F", "#{session_name}").Output()
	taken := map[string]bool{}
	if err != nil {
		return taken
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			taken[l] = true
		}
	}
	return taken
}

// newSessionArgs builds the create half of a launch. The command is one
// argument, not split on spaces: tmux hands a single shell-command string to
// the shell, which is what makes "claude --resume x" work.
func newSessionArgs(dir, cmd, name string) []string {
	args := []string{"new-session", "-d", "-s", name, "-c", dir}
	if c := strings.TrimSpace(cmd); c != "" {
		args = append(args, c)
	}
	return args
}

// launchSession creates the detached session and moves the inner client onto
// it — the same two-step the sidebar's own row switch performs, so a launched
// session lands you in it exactly like clicking one would. The switch goes
// through switchClientTo, which REFUSES an unscoped switch on a shared inner
// socket; that refusal comes back as an error and stays on screen, rather than
// the launch reporting success while leaving you where you were.
var launchSession = func(dir, cmd, name string) error {
	args := newSessionArgs(dir, cmd, name)
	if out, err := env.innerCmd(args...).CombinedOutput(); err != nil {
		return fmt.Errorf("new-session: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := switchClientTo(name); err != nil {
		return fmt.Errorf("switch-client: %w", err)
	}
	return saveLastLaunch(lastLaunch{Cmd: cmd, Dir: dir, Name: name})
}

// popupArgs builds the display-popup invocation. Two things make this awkward
// enough to be worth spelling out:
//
//   - display-popup blocks until the popup's command exits, so it can only be
//     called from a goroutine — the sidebar has to keep painting behind it.
//   - a popup inherits the *outer server's* environment, not this pane's, so
//     the socket and client the modal must talk to are passed with -e. Without
//     them the modal would create the session on the wrong tmux server.
//
// Split out from the goroutine so the environment plumbing — the part that
// silently sends the launch to the wrong tmux server when it is wrong — is
// testable without a tmux server.
func popupArgs(exe, dir string) []string {
	args := []string{"display-popup", "-E", "-w", "80%", "-h", "60%"}
	if env.self != "" {
		args = append(args, "-t", string(env.self)) // resolves the client showing the sidebar
	}
	if env.inner != "" {
		args = append(args, "-e", "ORCHARD_TMUX_SOCKET="+string(env.inner))
	}
	if env.client != "" {
		args = append(args, "-e", "ORCHARD_TMUX_CLIENT="+string(env.client))
	}
	// -d on a directory that no longer exists (a removed worktree is a normal
	// thing to inherit from a session) can fail the whole popup, so only pass
	// a directory we just saw. The modal falls back to $HOME without it.
	if dirExists(dir) {
		args = append(args, "-e", "ORCHARD_LAUNCH_DIR="+dir, "-d", dir)
	}
	return append(args, shellQuote(exe)+" launch")
}

var openLaunchPopup = func(dir string) {
	exe, err := os.Executable()
	if err != nil {
		logf("locating self: %v", err)
		return
	}
	// display-popup blocks until the popup's command exits, so this cannot be
	// called on the update loop — the sidebar has to keep painting behind it
	go runOuter(popupArgs(exe, dir)...)
}

// shellQuote wraps a path for the shell tmux runs the popup command through.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// openLaunch is the + button's action: open the modal on the selected
// session's directory.
//
// Focus goes back to the inner pane FIRST, and the popup opens over it. The
// other order raced: the popup takes the keyboard as a client-level overlay,
// and a select-pane arriving afterwards would be moving focus underneath a
// modal that already has it — harmless in practice, but only by accident. This
// way the end state is the same whether the popup opens, fails, or is
// cancelled: the keyboard is on the shell, where it was.
func (m *model) openLaunch() {
	dir := ""
	if r, ok := m.railRow(); ok {
		dir = r.cwd
	}
	handBackFocus()
	openLaunchPopup(dir)
}
