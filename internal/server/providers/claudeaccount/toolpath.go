package claudeaccount

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// binDirsEnv names a PATH-style list of directories searched after
// PATH itself. When set it REPLACES the built-in fallback list; set it
// to the empty string to search nothing beyond PATH.
const binDirsEnv = "ORCHARD_BIN_DIRS"

// toolPathEnv returns the environment variable that pins one tool to an
// explicit executable: "ccusage" → "ORCHARD_CCUSAGE_BIN". Characters
// that cannot appear in an env var name are folded to underscores.
func toolPathEnv(tool string) string {
	var b strings.Builder
	b.WriteString("ORCHARD_")
	for _, r := range strings.ToUpper(tool) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteString("_BIN")
	return b.String()
}

// fallbackBinDirs returns the directories searched when a tool is not
// on PATH.
//
// The daemon usually runs under launchd (macOS) or systemd (Linux),
// neither of which sources the user's shell profile: the inherited
// PATH is the system default and contains none of the prefixes that
// `bun install -g` / `npm install -g` / Homebrew write to. Without
// this list `ccusage` is unreachable from the daemon on a host where
// the operator's interactive shell finds it fine (#400).
//
// ORCHARD_BIN_DIRS replaces the list when set.
func fallbackBinDirs() []string {
	if raw, ok := os.LookupEnv(binDirsEnv); ok {
		return filepath.SplitList(raw)
	}
	dirs := make([]string, 0, 4)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".bun", "bin"),
			filepath.Join(home, ".local", "bin"),
		)
	}
	return append(dirs, "/opt/homebrew/bin", "/usr/local/bin")
}

// resolveToolPath resolves a tool name to an absolute executable path.
//
// Search order: the ORCHARD_<TOOL>_BIN pin, then PATH, then
// fallbackBinDirs. A pin that does not point at an executable is an
// error rather than a silent fall-through — a typo'd pin that quietly
// resolved elsewhere would reproduce the invisible failure #400 is
// about.
//
// Returns an error wrapping ErrToolNotInstalled when nothing matched,
// so callers keep their errors.Is / errors.As handling.
func resolveToolPath(tool string) (string, error) {
	if pinned, ok := os.LookupEnv(toolPathEnv(tool)); ok && pinned != "" {
		if isExecutableFile(pinned) {
			return pinned, nil
		}
		return "", fmt.Errorf("%s=%q is not an executable file: %w",
			toolPathEnv(tool), pinned, &ToolNotInstalledError{Tool: tool})
	}
	if p, err := exec.LookPath(tool); err == nil {
		return p, nil
	}
	for _, dir := range fallbackBinDirs() {
		if dir == "" {
			continue
		}
		if cand := filepath.Join(dir, tool); isExecutableFile(cand) {
			return cand, nil
		}
	}
	return "", &ToolNotInstalledError{Tool: tool}
}

// isExecutableFile reports whether path is a non-directory with at
// least one executable bit. Symlinks are followed — a bun shim is a
// legitimate answer.
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// toolLocator wraps resolveToolPath with logging that reports a
// missing tool once per not-found transition.
//
// The provider polls every PollInterval (60s), so an unconditional
// warn would emit ~1400 identical lines a day; latching on the
// transition keeps the signal without the flood, and clearing the
// latch on a successful resolve means a tool that disappears later is
// reported again.
type toolLocator struct {
	logger *slog.Logger

	mu      sync.Mutex
	missing map[string]bool
}

// newToolLocator constructs a locator logging through logger. A nil
// logger defaults to slog.Default().
func newToolLocator(logger *slog.Logger) *toolLocator {
	if logger == nil {
		logger = slog.Default()
	}
	return &toolLocator{logger: logger, missing: map[string]bool{}}
}

// Locate resolves tool to an absolute executable path, warning on the
// first failure after any successful resolution.
func (l *toolLocator) Locate(tool string) (string, error) {
	bin, err := resolveToolPath(tool)

	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		if !l.missing[tool] {
			l.missing[tool] = true
			l.logger.Warn("claudeaccount: CLI not found on PATH or in the fallback bin dirs; its fields resolve null",
				"tool", tool,
				"path", os.Getenv("PATH"),
				"fallback_dirs", strings.Join(fallbackBinDirs(), string(os.PathListSeparator)),
				"override_env", toolPathEnv(tool),
				"err", err)
		}
		return "", err
	}
	if l.missing[tool] {
		delete(l.missing, tool)
		l.logger.Info("claudeaccount: CLI resolved after an earlier miss", "tool", tool, "bin", bin)
	}
	return bin, nil
}
