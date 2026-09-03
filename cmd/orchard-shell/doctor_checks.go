package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/drewdrewthis/orchardist/internal/release"
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

// systemdUnits mirrors scripts/install.sh's SERVICE_UNITS priority order:
// orchard-daemon.service is the unit this repo actually ships as active
// today; orchard.service is install_service's own template name, checked
// second so an older install using it is still recognized.
var systemdUnits = []string{"orchard-daemon.service", "orchard.service"}

// systemdUnitCheck is one candidate unit's `systemctl --user is-active`
// result.
type systemdUnitCheck struct {
	unit   string
	output string
	err    error
}

func checkSystemd(ctx context.Context, env doctorEnv) checkResult {
	if env.goos != "linux" {
		return checkResult{ID: "systemd", Status: statusPass,
			Detail: fmt.Sprintf("%s does not use systemd; check via: launchctl print gui/$(id -u)/com.orchard.daemon", env.goos)}
	}
	return evaluateSystemd(resolveSystemdUnits(ctx, env))
}

// resolveSystemdUnits runs `systemctl --user is-active` for each candidate
// unit in systemdUnits' priority order, mirroring install.sh's
// detect_active_service: it stops at the first active unit, or at the
// first "systemctl not found" (a host-wide condition — retrying the next
// unit name would just fail the same way).
func resolveSystemdUnits(ctx context.Context, env doctorEnv) []systemdUnitCheck {
	results := make([]systemdUnitCheck, 0, len(systemdUnits))
	for _, unit := range systemdUnits {
		out, err := env.run(ctx, "systemctl", "--user", "is-active", unit)
		results = append(results, systemdUnitCheck{unit: unit, output: out, err: err})
		if err == nil || errors.Is(err, exec.ErrNotFound) {
			break
		}
	}
	return results
}

// evaluateSystemd is checkSystemd's pure decision, given every candidate
// unit's systemctl result in priority order (resolveSystemdUnits).
func evaluateSystemd(results []systemdUnitCheck) checkResult {
	for _, r := range results {
		if r.err == nil {
			return checkResult{ID: "systemd", Status: statusPass, Detail: fmt.Sprintf("%s is active", r.unit)}
		}
		if errors.Is(r.err, exec.ErrNotFound) {
			return checkResult{ID: "systemd", Status: statusWarn, Detail: "systemctl not found — not a systemd host"}
		}
	}
	names := make([]string, len(results))
	outputs := make([]string, len(results))
	for i, r := range results {
		names[i] = r.unit
		outputs[i] = r.output
	}
	return checkResult{ID: "systemd", Status: statusFail,
		Detail: fmt.Sprintf("none of %s is active (systemctl reports: %s)",
			strings.Join(names, ", "), strings.Join(outputs, ", ")),
		Remedy: "install.sh   (or: systemctl --user start orchard)"}
}

// --- PATH --------------------------------------------------------------------

// pathShadow is one suite binary whose $PATH resolution does not match the
// install prefix (self's own directory) — a stale duplicate earlier on
// $PATH silently shadows the real one.
type pathShadow struct {
	name     string // binary name, e.g. "orchard-daemon"
	resolved string // what exec.LookPath actually found
	prefix   string // the sibling path beside self that it shadows
}

func checkPath(env doctorEnv) checkResult {
	return evaluatePath(env.self, env.pathEnv, resolvePathShadows(env.self, env.lookPath))
}

// resolvePathShadows exec.LookPath-resolves every release.SuiteBinaries
// member and flags any whose resolved directory differs from self's own —
// PATH order put a different, same-named binary in front of the one
// actually installed beside self. A binary not found on PATH at all is a
// different problem (untouched by this check).
func resolvePathShadows(self string, lookup pathLookup) []pathShadow {
	if self == "" || lookup == nil {
		return nil
	}
	dir := filepath.Dir(self)
	var shadows []pathShadow
	for _, name := range release.SuiteBinaries {
		resolved, err := lookup(name)
		if err != nil || resolved == "" {
			continue
		}
		if filepath.Dir(resolved) != dir {
			shadows = append(shadows, pathShadow{name: name, resolved: resolved, prefix: filepath.Join(dir, name)})
		}
	}
	return shadows
}

// evaluatePath is checkPath's pure decision, given self's own PATH
// membership and every detected suite-binary shadow (resolvePathShadows).
func evaluatePath(self, pathEnv string, shadows []pathShadow) checkResult {
	if self == "" {
		return checkResult{ID: "path", Status: statusWarn, Detail: "could not resolve this binary's own path"}
	}
	dir := filepath.Dir(self)
	onPath := false
	for _, p := range filepath.SplitList(pathEnv) {
		if p == dir {
			onPath = true
			break
		}
	}
	if !onPath {
		return checkResult{ID: "path", Status: statusFail,
			Detail: dir + " is not on $PATH",
			Remedy: fmt.Sprintf("add it to $PATH, e.g.: export PATH=%s:$PATH", dir)}
	}
	if len(shadows) > 0 {
		parts := make([]string, len(shadows))
		for i, s := range shadows {
			parts[i] = fmt.Sprintf("%s: %s shadows %s", s.name, s.resolved, s.prefix)
		}
		return checkResult{ID: "path", Status: statusWarn,
			Detail: dir + " is on $PATH, but " + strings.Join(parts, "; "),
			Remedy: "reorder PATH or remove the stale binary"}
	}
	return checkResult{ID: "path", Status: statusPass, Detail: dir + " is on $PATH"}
}

// --- claude-session-state plugin -----------------------------------------

// sessionStatePluginKey is the installed_plugins.json key Claude Code's
// plugin system uses for this repo's marketplace (.claude-plugin/
// marketplace.json's "name": "orchardist") and its claude-session-state
// entry (plugin-sources/claude-session-state) — "<plugin>@<marketplace>",
// verified against a live ~/.claude/plugins/installed_plugins.json.
const sessionStatePluginKey = "claude-session-state@orchardist"

// sessionStatePluginRemedy is issue #772's AC: the exact commands to run
// inside Claude Code.
const sessionStatePluginRemedy = "/plugin marketplace add drewdrewthis/orchardist && /plugin install claude-session-state@orchardist"

// installedPluginsManifest is the subset of installed_plugins.json this
// check reads: {"plugins": {"<name>@<marketplace>": [...]}}.
type installedPluginsManifest struct {
	Plugins map[string]json.RawMessage `json:"plugins"`
}

// pluginInstalled reports whether sessionStatePluginKey is present in the
// installed_plugins.json manifest at path. A missing or unparseable file
// means "not installed" — a fresh Claude Code install, or a host with no
// plugin system at all — not an error the doctor run should abort on.
func pluginInstalled(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var manifest installedPluginsManifest
	if json.Unmarshal(data, &manifest) != nil {
		return false
	}
	_, ok := manifest.Plugins[sessionStatePluginKey]
	return ok
}

// pluginStateDirHasFiles reports whether the plugin's state directory holds
// at least one regular file — evidence its hooks have actually fired, not
// just that the plugin is installed.
func pluginStateDirHasFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			return true
		}
	}
	return false
}

func checkPlugin(env doctorEnv) checkResult {
	return evaluatePlugin(pluginInstalled(env.pluginsFile), pluginStateDirHasFiles(env.pluginStateDir))
}

// evaluatePlugin is checkPlugin's pure decision. Neither condition fails
// the overall doctor run (AC: "warn", not fail) — the sidebar still works
// without the plugin, just with degraded card content.
func evaluatePlugin(installed, stateHasFiles bool) checkResult {
	if !installed {
		return checkResult{ID: "plugin", Status: statusWarn,
			Detail: "claude-session-state plugin not found — sidebar cards will show no Claude state (glyph, model, last message)",
			Remedy: sessionStatePluginRemedy}
	}
	if !stateHasFiles {
		return checkResult{ID: "plugin", Status: statusWarn,
			Detail: "claude-session-state plugin is installed but its state dir has no files yet — its hooks have never fired",
			Remedy: "start or resume a Claude Code session to trigger the plugin's hooks"}
	}
	return checkResult{ID: "plugin", Status: statusPass, Detail: "claude-session-state plugin installed and writing state"}
}
