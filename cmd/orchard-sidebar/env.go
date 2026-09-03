package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Two tmux servers, and a value from one means nothing on the other.
//
// The sessions this sidebar reads, switches, renames and kills live on the
// INNER server. The pane it draws into, the pane it hands focus back to, and
// the width option the wrapper's hooks read all live on the OUTER one. #747's
// first defect was exactly that confusion: an outer pane id sent to the inner
// server, where the same id names a different pane — and resizes it.
//
// The ids are therefore distinct types, so the compiler refuses the mix-up
// rather than the user discovering it.
type (
	// innerSocket is ORCHARD_TMUX_SOCKET: the -L name of the server holding
	// the sessions. Empty is the sidebar's normal, unwrapped mode, where the
	// one server there is owns everything.
	innerSocket string

	// clientTTY is ORCHARD_TMUX_CLIENT: the tty of this wrapper's own inner
	// client, so a switch can be scoped to it. On a SHARED inner server an
	// unscoped switch-client moves an arbitrary attached client, which
	// hijacked an unrelated terminal in the wild (#747 defect 2).
	clientTTY string

	// outerPane is a pane id on the OUTER server. Two of them matter: the
	// sidebar's own pane ($TMUX_PANE, what resize/collapse target) and the
	// inner client's pane (ORCHARD_OUTER_PANE, where focus is handed back).
	outerPane string
)

// tmuxEnv is the wrapper environment, resolved ONCE per process (main, or
// runLaunch for the popup) instead of re-read at every exec site — eight of
// them before this existed, each free to disagree with the others about what
// "wrapped" means.
type tmuxEnv struct {
	inner  innerSocket
	client clientTTY
	outer  outerPane // the inner client's pane: where a click hands focus back
	self   outerPane // this sidebar's own pane: what collapse and resize target
}

// env is the process's resolved wrapper environment. Written once at startup
// and read-only after; tests swap it through setTmuxEnv.
var env tmuxEnv

func readTmuxEnv() tmuxEnv {
	return tmuxEnv{
		inner:  innerSocket(os.Getenv("ORCHARD_TMUX_SOCKET")),
		client: clientTTY(os.Getenv("ORCHARD_TMUX_CLIENT")),
		outer:  outerPane(os.Getenv("ORCHARD_OUTER_PANE")),
		self:   outerPane(os.Getenv("TMUX_PANE")),
	}
}

// wrapped reports whether the sidebar is running inside the outer-shell
// wrapper (docs/outer-shell.md) rather than as a plain pane.
func (e tmuxEnv) wrapped() bool { return e.inner != "" }

// problem names the environment combination the sidebar cannot work in, for
// the header banner. A wrapper that half-configures the sidebar is otherwise
// silent: switches are refused and collapse does nothing, with nothing on
// screen saying why. Empty means the environment is usable.
func (e tmuxEnv) problem() string {
	switch {
	case e.wrapped() && e.client == "":
		return "no ORCHARD_TMUX_CLIENT — switching disabled"
	case e.self == "" && (e.wrapped() || e.outer != ""):
		return "no TMUX_PANE — resize and collapse disabled"
	}
	return ""
}

// innerCmd and innerCmdContext build a tmux invocation against the server
// holding the sessions (the split mirrors exec.Command/exec.CommandContext).
//
// orchard-sidebar runs inside a real tmux pane, so a bare `tmux` exec resolves
// against whichever server owns THAT pane. Inside the wrapper that pane
// belongs to the OUTER server, never the inner one holding the sessions the
// sidebar actually reads and switches — #747 defect: switch-client silently
// no-op'd and the current-session highlight never anchored. With a socket set,
// every exec targets it explicitly via -L instead of relying on server
// inference, and TMUX is stripped from the child env so tmux doesn't
// second-guess the explicit -L using the outer session recorded there. Unset,
// this is exactly exec.CommandContext(ctx, "tmux", args...).
func (e tmuxEnv) innerCmd(args ...string) *exec.Cmd {
	return e.innerCmdContext(context.Background(), args...)
}

func (e tmuxEnv) innerCmdContext(ctx context.Context, args ...string) *exec.Cmd {
	if e.inner == "" {
		return exec.CommandContext(ctx, "tmux", args...)
	}
	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-L", string(e.inner)}, args...)...)
	environ := os.Environ()
	kept := make([]string, 0, len(environ))
	for _, kv := range environ {
		if strings.HasPrefix(kv, "TMUX=") {
			continue
		}
		kept = append(kept, kv)
	}
	cmd.Env = kept
	return cmd
}

// outerCmd builds a tmux invocation against the server that owns this pane —
// the OUTER wrapper's own server when wrapped, and the only server there is
// when not. A plain, unmodified exec: no -L, no TMUX stripped, because
// $TMUX already names exactly the server every outerPane id belongs to.
func (e tmuxEnv) outerCmd(args ...string) *exec.Cmd {
	return exec.Command("tmux", args...)
}

// resizePaneArgs, setPaneOptionArgs and selectPaneArgs take an outerPane and
// nothing else, which is the point: an inner session name cannot reach a
// command that resizes, flags or focuses a pane on the outer server.
func resizePaneArgs(p outerPane, w int) []string {
	return []string{"resize-pane", "-t", string(p), "-x", strconv.Itoa(w)}
}

func setPaneOptionArgs(p outerPane, name, value string) []string {
	return []string{"set-option", "-w", "-t", string(p), name, value}
}

func selectPaneArgs(p outerPane) []string {
	return []string{"select-pane", "-t", string(p)}
}
