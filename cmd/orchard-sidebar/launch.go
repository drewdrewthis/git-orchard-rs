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
// docs/outer-shell.md.
//
// ADR-016 (the daemon owns tmux) is bent here, not honoured: this creates the
// session with `tmux new-session` on the inner socket, the same client-side
// shim the sidebar already uses for switch-client. The daemon does have a
// launchSession mutation, but it only launches Claude — cwd/name/model/prompt,
// no arbitrary command — and the point of this modal is "run the last command
// again, wherever I say". Replacing this with a daemon mutation that takes a
// command is the follow-up; it is noted in docs/outer-shell.md.

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

// newSessionArgs builds the CREATE half of a launch: a detached session on the
// user's shell, carrying NO command. The command is delivered separately by
// sendCommandArgs, so the pane's process is the shell — the command's exit
// returns to a prompt in DIR instead of tearing the session down (#783). Before
// this, the command WAS the pane process, so `/exit` from claude killed pane →
// window → session and the sidebar row vanished.
func newSessionArgs(dir, name string) []string {
	return []string{"new-session", "-d", "-s", name, "-c", dir}
}

// sendCommandArgs builds the DELIVER half, split into a literal `-l` send and
// a separate Enter (mirroring the daemon's own launchSession,
// internal/server/resolvers/launch.go:72): `-l` stops tmux parsing the
// command as key names, so "claude --resume x" isn't mistaken for a
// keybinding.
//
// Pane survival is timing-independent — the pane's process is the shell, so a
// keystroke that races the shell's startup degrades to "an empty prompt in
// DIR", never a dead pane. Delivery itself is best-effort: a shell that
// flushes typeahead on startup can still drop the send-keys (outer.go
// documents the same flake for outer boot), landing the user at a bare
// prompt with no command run. A live probe on this machine: 10/10 launches
// delivered. An empty command delivers nothing and the session simply opens
// on its shell.
func sendCommandArgs(name, cmd string) ([][]string, bool) {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return nil, false
	}
	return [][]string{
		{"send-keys", "-t", name, "-l", c},
		{"send-keys", "-t", name, "Enter"},
	}, true
}

// launchSession creates the detached session on the shell, delivers the command
// into it, then moves the inner client onto it — the same switch the sidebar's
// own row click performs, so a launched session lands you in it exactly like
// clicking one would. The command is delivered before the switch. The
// switch goes through switchClientTo,
// which REFUSES an unscoped switch on a shared inner socket; that refusal comes
// back as an error and stays on screen, rather than the launch reporting success
// while leaving you where you were.
var launchSession = func(dir, cmd, name string) error {
	if out, err := env.innerCmd(newSessionArgs(dir, name)...).CombinedOutput(); err != nil {
		return fmt.Errorf("new-session: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if argLists, ok := sendCommandArgs(name, cmd); ok {
		for _, args := range argLists {
			if out, err := env.innerCmd(args...).CombinedOutput(); err != nil {
				// A failed send-keys leaves the empty-shell session in place — survivable
				// and visible in the sidebar, so no cleanup needed here.
				return fmt.Errorf("send-keys: %v: %s", err, strings.TrimSpace(string(out)))
			}
		}
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
