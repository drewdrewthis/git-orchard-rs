// loglevel.go — operator control over the daemon's slog verbosity.
//
// Kept out of daemon.go so the lifecycle file stays about the lifecycle
// (R6/R15); daemon.go only resolves the level and hands the logger down.

package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// EnvLogLevel is the environment variable that sets the daemon's log level
// when --log-level is not given. It exists because launchd and systemd units
// set environment variables far more naturally than they edit ProgramArguments.
const EnvLogLevel = "ORCHARD_LOG_LEVEL"

// defaultLogLevel is what the daemon logs at when neither the flag nor the
// environment variable says otherwise.
const defaultLogLevel = slog.LevelInfo

// logLevelNames maps every accepted spelling to its slog level. "warning" is
// accepted alongside "warn" because both spellings are in common use and a
// rejected level would otherwise refuse to start the daemon.
var logLevelNames = map[string]slog.Level{
	"debug":   slog.LevelDebug,
	"info":    slog.LevelInfo,
	"warn":    slog.LevelWarn,
	"warning": slog.LevelWarn,
	"error":   slog.LevelError,
}

// logLevelUsage is the flag's help text. It names the environment variable so
// `orchard daemon start --help` documents both paths in one place.
const logLevelUsage = "log verbosity: debug|info|warn|error (default info; overrides " + EnvLogLevel + ")"

// resolveLogLevel picks the daemon's log level from the --log-level flag value
// and the ORCHARD_LOG_LEVEL environment value, in that order of precedence,
// falling back to Info when both are empty.
//
// An unrecognised value is an error rather than a silent fallback: a daemon
// that quietly ignored `--log-level debug` would leave the operator staring at
// an empty log believing they had turned debugging on.
func resolveLogLevel(flagVal, envVal string) (slog.Level, error) {
	if lvl, ok, err := parseLogLevel(flagVal, "--log-level"); ok || err != nil {
		return lvl, err
	}
	if lvl, ok, err := parseLogLevel(envVal, EnvLogLevel); ok || err != nil {
		return lvl, err
	}
	return defaultLogLevel, nil
}

// parseLogLevel interprets one candidate value. ok is false when the value is
// empty (or whitespace only), meaning the caller should fall through to the
// next source; source names the origin so the error is actionable.
func parseLogLevel(value, source string) (level slog.Level, ok bool, err error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false, nil
	}
	lvl, known := logLevelNames[strings.ToLower(trimmed)]
	if !known {
		return 0, false, fmt.Errorf("%s: unknown log level %q (want one of: debug, info, warn, error)", source, trimmed)
	}
	return lvl, true, nil
}

// newDaemonLogger builds the daemon's root logger at level, writing text
// records to w.
//
// Callers are expected to install the result as slog's default so provider
// code that reaches for slog.Default() inherits the same level — otherwise
// --log-level would only affect the handful of call sites that were passed
// this logger explicitly.
func newDaemonLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
