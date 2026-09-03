package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

func TestParseArgs_Defaults(t *testing.T) {
	t.Setenv("ORCHARD_TMUX_SOCKET", "") // isolate from the ambient environment
	opts, err := parseArgs(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseArgs(nil): %v", err)
	}
	if opts.InnerSocket != defaultInnerSocket {
		t.Errorf("InnerSocket = %q; want %q", opts.InnerSocket, defaultInnerSocket)
	}
	if opts.OuterSocket != defaultOuterSocket {
		t.Errorf("OuterSocket = %q; want %q", opts.OuterSocket, defaultOuterSocket)
	}
	if opts.Width != defaultWidth {
		t.Errorf("Width = %d; want %d", opts.Width, defaultWidth)
	}
	if opts.Session != "" {
		t.Errorf("Session = %q; want empty (most recently attached)", opts.Session)
	}
	if opts.Conf != "" || opts.Detach || opts.ShowVersion || opts.Doctor {
		t.Errorf("unexpected non-zero default in %+v", opts)
	}
}

func TestParseArgs_EveryFlag(t *testing.T) {
	opts, err := parseArgs([]string{
		"--inner-socket", "work", "--outer-socket", "wrap", "--session", "api",
		"--width", "55", "--conf", "/tmp/other.conf", "--detach",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	want := Options{
		InnerSocket: "work", OuterSocket: "wrap", Session: "api",
		Width: 55, Conf: "/tmp/other.conf", Detach: true,
	}
	if opts != want {
		t.Errorf("parseArgs = %+v; want %+v", opts, want)
	}
}

func TestParseArgs_VersionFlag(t *testing.T) {
	opts, err := parseArgs([]string{"--version"}, io.Discard)
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !opts.ShowVersion {
		t.Error("--version did not set ShowVersion")
	}
}

func TestParseArgs_DoctorSubcommand(t *testing.T) {
	t.Setenv("ORCHARD_TMUX_SOCKET", "") // isolate from the ambient environment
	opts, err := parseArgs([]string{"doctor"}, io.Discard)
	if err != nil {
		t.Fatalf("parseArgs(doctor): %v", err)
	}
	if !opts.Doctor || opts.DoctorJSON {
		t.Errorf("parseArgs(doctor) = %+v; want Doctor only", opts)
	}
	if opts.InnerSocket != defaultInnerSocket {
		t.Errorf("doctor InnerSocket = %q; want %q (same default as orchard shell)", opts.InnerSocket, defaultInnerSocket)
	}
	if opts.OuterSocket != defaultOuterSocket {
		t.Errorf("doctor OuterSocket = %q; want %q (same default as orchard shell)", opts.OuterSocket, defaultOuterSocket)
	}

	opts, err = parseArgs([]string{"doctor", "--json"}, io.Discard)
	if err != nil {
		t.Fatalf("parseArgs(doctor --json): %v", err)
	}
	if !opts.Doctor || !opts.DoctorJSON {
		t.Errorf("parseArgs(doctor --json) = %+v; want Doctor and DoctorJSON", opts)
	}
}

// @scenario doctor's socket flags and $ORCHARD_TMUX_SOCKET default match orchard shell
//
// AC (#747 Bug 2): doctor's inner/outer-socket checks were hardcoded to the
// default socket names, so `orchard shell doctor` reported on the wrong
// server for anyone using non-default sockets. doctor now takes the same
// --inner-socket/--outer-socket flags as orchard shell itself, and honours
// $ORCHARD_TMUX_SOCKET as the inner default the same way.
func TestParseArgs_DoctorSocketFlags(t *testing.T) {
	t.Run("explicit flags are honoured", func(t *testing.T) {
		t.Setenv("ORCHARD_TMUX_SOCKET", "")
		opts, err := parseArgs([]string{"doctor", "--inner-socket", "work", "--outer-socket", "wrap"}, io.Discard)
		if err != nil {
			t.Fatalf("parseArgs: %v", err)
		}
		if opts.InnerSocket != "work" {
			t.Errorf("InnerSocket = %q; want work", opts.InnerSocket)
		}
		if opts.OuterSocket != "wrap" {
			t.Errorf("OuterSocket = %q; want wrap", opts.OuterSocket)
		}
	})

	t.Run("ORCHARD_TMUX_SOCKET sets the inner default", func(t *testing.T) {
		t.Setenv("ORCHARD_TMUX_SOCKET", "from-env")
		opts, err := parseArgs([]string{"doctor"}, io.Discard)
		if err != nil {
			t.Fatalf("parseArgs: %v", err)
		}
		if opts.InnerSocket != "from-env" {
			t.Errorf("InnerSocket = %q; want from-env", opts.InnerSocket)
		}
	})

	t.Run("an explicit flag overrides ORCHARD_TMUX_SOCKET", func(t *testing.T) {
		t.Setenv("ORCHARD_TMUX_SOCKET", "from-env")
		opts, err := parseArgs([]string{"doctor", "--inner-socket", "explicit"}, io.Discard)
		if err != nil {
			t.Fatalf("parseArgs: %v", err)
		}
		if opts.InnerSocket != "explicit" {
			t.Errorf("InnerSocket = %q; want explicit", opts.InnerSocket)
		}
	})
}

// A mistyped flag that lands as a positional would otherwise boot the wrapper
// with a silently different configuration.
func TestParseArgs_RejectsStrayPositionals(t *testing.T) {
	for _, argv := range [][]string{
		{"work"},
		{"--inner-socket", "work", "api"},
		{"doctor", "--json", "extra"},
	} {
		if _, err := parseArgs(argv, io.Discard); err == nil {
			t.Errorf("parseArgs(%v) succeeded; want an error naming the stray argument", argv)
		}
	}
}

func TestParseArgs_RejectsNonPositiveWidth(t *testing.T) {
	if _, err := parseArgs([]string{"--width", "0"}, io.Discard); err == nil {
		t.Error("parseArgs(--width 0) succeeded; want an error")
	}
}

func TestParseArgs_UnknownFlagIsAnError(t *testing.T) {
	if _, err := parseArgs([]string{"--nope"}, io.Discard); err == nil {
		t.Error("parseArgs(--nope) succeeded; want an error")
	}
}

func TestParseArgs_HelpIsFlagErrHelp(t *testing.T) {
	_, err := parseArgs([]string{"--help"}, io.Discard)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseArgs(--help) = %v; want flag.ErrHelp", err)
	}
}

func TestRun_VersionPrintsTheBakedVersionAndExitsZero(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--version) = %d; want 0", code)
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Errorf("stdout = %q; want %q", stdout.String(), version)
	}
}

func TestRun_HelpExitsZero(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--help) = %d; want 0", code)
	}
}

func TestRun_UsageErrorExitsOne(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := run([]string{"--nope"}, &stdout, &stderr); code != 1 {
		t.Fatalf("run(--nope) = %d; want 1 (2 is reserved for a missing session)", code)
	}
}

// doctor --json must always produce a structurally valid envelope, on any
// host: exactly 9 checks (runChecks' documented set), each with a status in
// {pass,warn,fail}. This runs against real seams (tmux, the daemon, $PATH),
// so unlike doctor_test.go's hermetic, pass/fail-specific coverage it cannot
// assert particular outcomes without becoming flaky across machines and CI.
func TestRun_DoctorJSONIsAlwaysValidJSON(t *testing.T) {
	var stdout, stderr strings.Builder
	code := run([]string{"doctor", "--json"}, &stdout, &stderr)
	if code != 0 && code != 1 {
		t.Fatalf("run(doctor --json) = %d; want 0 or 1", code)
	}

	var env doctorEnvelope
	if err := json.Unmarshal([]byte(stdout.String()), &env); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
	}
	if env.Data == nil || len(env.Data.Checks) != 9 {
		t.Fatalf("Data.Checks has %d entries; want 9", len(env.Data.Checks))
	}
	if env.OK && code != 0 {
		t.Errorf("ok=true but exit code = %d; want 0", code)
	}
	if !env.OK && code != 1 {
		t.Errorf("ok=false but exit code = %d; want 1", code)
	}
	for _, c := range env.Data.Checks {
		switch c.Status {
		case statusPass, statusWarn, statusFail:
		default:
			t.Errorf("check %q has invalid status %q", c.ID, c.Status)
		}
		if c.ID == "" {
			t.Error("a check has an empty ID")
		}
	}
}
