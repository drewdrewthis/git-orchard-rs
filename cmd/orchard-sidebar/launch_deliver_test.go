package main

import (
	"errors"
	"reflect"
	"testing"
)

// deliveryOutcome is the pure heart of the poll: any non-shell sample is proof
// the command took; a run of nothing but the shell is the dropped-keystroke
// flake.
func TestDeliveryOutcome(t *testing.T) {
	cases := []struct {
		name    string
		samples []string
		shell   string
		want    deliveryVerdict
	}{
		{"first sample already the command", []string{"claude"}, "zsh", deliveryConfirmed},
		{"command after some shell samples", []string{"zsh", "zsh", "node"}, "zsh", deliveryConfirmed},
		{"all shell resends", []string{"zsh", "zsh", "zsh"}, "zsh", deliveryResend},
		{"no samples resends", nil, "zsh", deliveryResend},
	}
	for _, c := range cases {
		if got := deliveryOutcome(c.samples, c.shell); got != c.want {
			t.Errorf("%s: deliveryOutcome(%v,%q) = %v, want %v", c.name, c.samples, c.shell, got, c.want)
		}
	}
}

// A pane that stays on the shell is the flake: resend exactly once, never a loop.
func TestConfirmDeliveryResendsOnceWhenStuck(t *testing.T) {
	resends := 0
	confirmDelivery("zsh", deliverySeams{
		command: func() (string, error) { return "zsh", nil },
		resend:  func() error { resends++; return nil },
		wait:    func() {},
	})
	if resends != 1 {
		t.Fatalf("resends = %d, want exactly 1", resends)
	}
}

// Once the command shows up in the pane, nothing is resent.
func TestConfirmDeliveryNoResendWhenDelivered(t *testing.T) {
	resends := 0
	confirmDelivery("zsh", deliverySeams{
		command: func() (string, error) { return "claude", nil },
		resend:  func() error { resends++; return nil },
		wait:    func() {},
	})
	if resends != 0 {
		t.Fatalf("resends = %d, want 0", resends)
	}
}

// A pane that reads as shell then flips to the command mid-poll is delivered —
// and the loop must stop reading once it sees the flip, not keep sampling.
func TestPollDeliveryStopsAtFirstNonShell(t *testing.T) {
	calls := 0
	seq := []string{"zsh", "zsh", "claude"}
	v := pollDelivery("zsh", deliverySeams{
		command: func() (string, error) {
			s := seq[calls]
			calls++
			return s, nil
		},
		resend: func() error { return nil },
		wait:   func() {},
	})
	if v != deliveryConfirmed {
		t.Fatalf("verdict = %v, want confirmed", v)
	}
	if calls != 3 {
		t.Fatalf("command reads = %d, want 3 (stop at first non-shell)", calls)
	}
}

// A read error skips that sample rather than aborting the watch, so a pane that
// errors every read still reaches the resend decision.
func TestPollDeliveryToleratesReadErrors(t *testing.T) {
	if v := pollDelivery("zsh", deliverySeams{
		command: func() (string, error) { return "", errors.New("no such pane") },
		resend:  func() error { return nil },
		wait:    func() {},
	}); v != deliveryResend {
		t.Fatalf("verdict = %v, want resend", v)
	}
}

// The literal `-l` send must carry a command with embedded quotes and spaces
// verbatim as ONE argv token — tmux must not reparse it as key names.
func TestSendCommandArgsCarriesLiteralVerbatim(t *testing.T) {
	cmd := `claude --append-system-prompt "say \"hi there\""`
	argLists, ok := sendCommandArgs("sess", cmd)
	if !ok {
		t.Fatal("expected a command to send")
	}
	want := []string{"send-keys", "-t", "sess", "-l", cmd}
	if !reflect.DeepEqual(argLists[0], want) {
		t.Fatalf("literal send = %#v, want %#v", argLists[0], want)
	}
	// Enter is a separate send so tmux submits the line without parsing the
	// command itself as keys.
	if wantEnter := []string{"send-keys", "-t", "sess", "Enter"}; !reflect.DeepEqual(argLists[1], wantEnter) {
		t.Fatalf("enter send = %#v, want %#v", argLists[1], wantEnter)
	}
}
