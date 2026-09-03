package main

import (
	"strings"
	"time"
)

// Delivery confirmation for launchSession. send-keys races the shell reading
// its own startup and can drop the keystrokes (the same 5s flake documented at
// cmd/orchard-shell/outer.go:233), landing the user at a bare prompt with the
// command never run. So after the send we watch the pane and, if the command
// never took, resend it exactly once.

const (
	deliverPolls    = 50                     // ~5s of headroom at deliverInterval
	deliverInterval = 100 * time.Millisecond // between pane_current_command reads
)

type deliveryVerdict int

const (
	deliveryConfirmed deliveryVerdict = iota
	deliveryResend
)

// deliveryOutcome is the whole decision, kept pure: the pane began on the shell
// (captured before delivery), so any sample that is no longer that shell means
// the command replaced it and took. A run of nothing-but-shell samples — the
// empty slice included — is the dropped-keystroke flake and earns a resend.
func deliveryOutcome(samples []string, shell string) deliveryVerdict {
	for _, s := range samples {
		if s != shell {
			return deliveryConfirmed
		}
	}
	return deliveryResend
}

// deliverySeams are the effects the poll/resend loop needs, injected so the
// loop is exercised without a tmux server or a real 5s wall-clock wait.
type deliverySeams struct {
	command func() (string, error) // reads pane_current_command
	resend  func() error           // re-delivers the command
	wait    func()                 // sleeps between polls
}

// confirmDelivery watches the pane after the initial send and resends once — and
// only once, never a loop — if the command never reached it. Best-effort: an
// unreadable pane or a genuinely empty command simply degrades to one extra
// send, which a live shell absorbs.
//
// Why the resend guard exists: it protects shells that flush buffered
// typeahead at startup (cmd/orchard-shell/outer.go:233). A shell that instead
// buffers typeahead and takes >5s to become interactive will run the command
// twice, sequentially (observed live with a 7s .zshrc).
// We accept that tradeoff: a dropped launch fails silently, a doubled one is
// visible on screen and the user can recover from it.
func confirmDelivery(shell string, s deliverySeams) {
	if pollDelivery(shell, s) == deliveryConfirmed {
		return
	}
	logf("launch: command did not reach pane (still on shell %q after ~5s); resending once (cmd/orchard-shell/outer.go:233 flake)", shell)
	if err := s.resend(); err != nil {
		logf("launch: resend failed: %v", err)
	}
}

// pollDelivery samples pane_current_command up to deliverPolls times, stopping
// early the moment the pane is no longer the shell. A read error just skips that
// sample rather than aborting the watch.
func pollDelivery(shell string, s deliverySeams) deliveryVerdict {
	samples := make([]string, 0, deliverPolls)
	for i := 0; i < deliverPolls; i++ {
		if cur, err := s.command(); err == nil {
			samples = append(samples, cur)
			if cur != shell {
				break
			}
		}
		if i < deliverPolls-1 {
			s.wait()
		}
	}
	return deliveryOutcome(samples, shell)
}

// paneCurrentCommand asks tmux what program the pane is running right now —
// the shell before delivery, then the command once it takes.
func paneCurrentCommand(name string) (string, error) {
	out, err := env.innerCmd("display", "-p", "-t", name, "#{pane_current_command}").Output()
	return strings.TrimSpace(string(out)), err
}
