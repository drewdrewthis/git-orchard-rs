// Tests for the daemon's log-level resolution (issue #749).
//
// Before #749 the daemon logger was a bare slog.Default(), which pins the
// level at Info with no operator control — every GitHub-call Debug line was
// dropped, so call volume could not be audited from a running daemon.

package daemon

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestResolveLogLevel is the table test for --log-level / ORCHARD_LOG_LEVEL
// precedence and parsing. The flag wins over the env; the env wins over the
// Info default; unparseable values are an error, not a silent fallback.
func TestResolveLogLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		flag    string
		env     string
		want    slog.Level
		wantErr bool
	}{
		{name: "both empty defaults to info", flag: "", env: "", want: slog.LevelInfo},
		{name: "flag debug", flag: "debug", env: "", want: slog.LevelDebug},
		{name: "flag info", flag: "info", env: "", want: slog.LevelInfo},
		{name: "flag warn", flag: "warn", env: "", want: slog.LevelWarn},
		{name: "flag warning alias", flag: "warning", env: "", want: slog.LevelWarn},
		{name: "flag error", flag: "error", env: "", want: slog.LevelError},
		{name: "env used when flag empty", flag: "", env: "debug", want: slog.LevelDebug},
		{name: "env warn used when flag empty", flag: "", env: "warn", want: slog.LevelWarn},
		{name: "flag overrides env", flag: "error", env: "debug", want: slog.LevelError},
		{name: "flag is case insensitive", flag: "DeBuG", env: "", want: slog.LevelDebug},
		{name: "env is case insensitive", flag: "", env: "ERROR", want: slog.LevelError},
		{name: "surrounding whitespace trimmed", flag: "  warn\t", env: "", want: slog.LevelWarn},
		{name: "whitespace-only env falls back to info", flag: "", env: "   ", want: slog.LevelInfo},
		{name: "unknown flag value errors", flag: "verbose", env: "", wantErr: true},
		{name: "unknown env value errors", flag: "", env: "trace", wantErr: true},
		{name: "numeric flag value errors", flag: "3", env: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveLogLevel(tc.flag, tc.env)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveLogLevel(%q, %q) = %v, want error", tc.flag, tc.env, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveLogLevel(%q, %q) returned unexpected error: %v", tc.flag, tc.env, err)
			}
			if got != tc.want {
				t.Errorf("resolveLogLevel(%q, %q) = %v, want %v", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}

// TestResolveLogLevelErrorNamesTheOffendingValue asserts the error message is
// actionable — it must quote the rejected value and list the accepted set, so
// an operator who typos `--log-level verbose` learns what to type instead.
func TestResolveLogLevelErrorNamesTheOffendingValue(t *testing.T) {
	t.Parallel()

	_, err := resolveLogLevel("verbose", "")
	if err == nil {
		t.Fatal("resolveLogLevel(\"verbose\", \"\") = nil error, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "verbose") {
		t.Errorf("error %q does not quote the rejected value %q", msg, "verbose")
	}
	for _, want := range []string{"debug", "info", "warn", "error"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not list the accepted level %q", msg, want)
		}
	}
}

// TestStartCmdRegistersLogLevelFlag asserts `orchard daemon start` exposes
// --log-level, and that its zero value is empty so resolveLogLevel can tell
// "operator said nothing" from "operator said info".
func TestStartCmdRegistersLogLevelFlag(t *testing.T) {
	t.Parallel()

	c := startCmd("test")
	f := c.Flags().Lookup("log-level")
	if f == nil {
		t.Fatal("`daemon start` does not register a --log-level flag")
	}
	if f.DefValue != "" {
		t.Errorf("--log-level default = %q, want \"\" so the env var can be consulted", f.DefValue)
	}
	if !strings.Contains(f.Usage, "ORCHARD_LOG_LEVEL") {
		t.Errorf("--log-level usage %q does not mention the ORCHARD_LOG_LEVEL env var", f.Usage)
	}
}

// TestStartCmdParsesLogLevelFlag asserts the flag value actually binds — a
// registered-but-unbound flag would parse cleanly and change nothing.
func TestStartCmdParsesLogLevelFlag(t *testing.T) {
	t.Parallel()

	c := startCmd("test")
	if err := c.Flags().Parse([]string{"--log-level", "debug"}); err != nil {
		t.Fatalf("parse --log-level debug: %v", err)
	}
	got, err := c.Flags().GetString("log-level")
	if err != nil {
		t.Fatalf("GetString(log-level): %v", err)
	}
	if got != "debug" {
		t.Errorf("--log-level bound value = %q, want %q", got, "debug")
	}
}

// TestNewDaemonLoggerHonoursLevel asserts the constructed logger actually
// filters: at Info a Debug record is dropped, at Debug it is emitted. Without
// this the flag would parse correctly and still change nothing observable.
func TestNewDaemonLoggerHonoursLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		level     slog.Level
		wantDebug bool
	}{
		{name: "info drops debug", level: slog.LevelInfo, wantDebug: false},
		{name: "debug emits debug", level: slog.LevelDebug, wantDebug: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger := newDaemonLogger(&buf, tc.level)
			logger.Debug("marker-debug")
			logger.Info("marker-info")

			out := buf.String()
			if got := strings.Contains(out, "marker-debug"); got != tc.wantDebug {
				t.Errorf("debug record emitted = %v, want %v (output: %q)", got, tc.wantDebug, out)
			}
			if !strings.Contains(out, "marker-info") {
				t.Errorf("info record missing at level %v (output: %q)", tc.level, out)
			}
		})
	}
}
