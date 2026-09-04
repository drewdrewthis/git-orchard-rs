package main

import (
	"encoding/json"
	"os"
)

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
