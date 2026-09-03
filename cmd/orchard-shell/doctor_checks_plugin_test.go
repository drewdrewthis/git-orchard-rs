package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- claude-session-state plugin -----------------------------------------

func writePluginsManifest(t *testing.T, dir string, keys ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"version":3,"plugins":{`)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"` + k + `":[{"scope":"user"}]`)
	}
	b.WriteString(`}}`)
	path := filepath.Join(dir, "installed_plugins.json")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestPluginInstalled(t *testing.T) {
	t.Run("key present", func(t *testing.T) {
		path := writePluginsManifest(t, t.TempDir(), "claude-session-state@orchardist", "other@foo")
		if !pluginInstalled(path) {
			t.Error("pluginInstalled = false; want true")
		}
	})

	t.Run("key absent", func(t *testing.T) {
		path := writePluginsManifest(t, t.TempDir(), "other@foo")
		if pluginInstalled(path) {
			t.Error("pluginInstalled = true; want false")
		}
	})

	t.Run("file missing", func(t *testing.T) {
		if pluginInstalled(filepath.Join(t.TempDir(), "nope.json")) {
			t.Error("pluginInstalled = true; want false")
		}
	})

	t.Run("unparseable file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "installed_plugins.json")
		if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if pluginInstalled(path) {
			t.Error("pluginInstalled = true; want false")
		}
	})
}

func TestPluginStateDirHasFiles(t *testing.T) {
	t.Run("dir with a file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "sess.json"), []byte("{}"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if !pluginStateDirHasFiles(dir) {
			t.Error("pluginStateDirHasFiles = false; want true")
		}
	})

	t.Run("empty dir", func(t *testing.T) {
		if pluginStateDirHasFiles(t.TempDir()) {
			t.Error("pluginStateDirHasFiles = true; want false")
		}
	})

	t.Run("missing dir", func(t *testing.T) {
		if pluginStateDirHasFiles(filepath.Join(t.TempDir(), "nope")) {
			t.Error("pluginStateDirHasFiles = true; want false")
		}
	})

	t.Run("dir containing only a subdirectory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if pluginStateDirHasFiles(dir) {
			t.Error("pluginStateDirHasFiles = true; want false (only a subdirectory present)")
		}
	})
}

func TestEvaluatePlugin(t *testing.T) {
	t.Run("not installed warns with the install remedy", func(t *testing.T) {
		got := evaluatePlugin(false, false)
		if got.Status != statusWarn {
			t.Errorf("Status = %v; want warn", got.Status)
		}
		if !strings.Contains(got.Remedy, "/plugin marketplace add drewdrewthis/orchardist") ||
			!strings.Contains(got.Remedy, "/plugin install claude-session-state@orchardist") {
			t.Errorf("Remedy = %q; want the marketplace-add + install commands", got.Remedy)
		}
	})

	t.Run("installed but no state files warns hooks never fired", func(t *testing.T) {
		got := evaluatePlugin(true, false)
		if got.Status != statusWarn {
			t.Errorf("Status = %v; want warn", got.Status)
		}
		if !strings.Contains(got.Detail, "hooks have never fired") {
			t.Errorf("Detail = %q; want it to mention hooks never firing", got.Detail)
		}
	})

	t.Run("installed and writing state passes", func(t *testing.T) {
		got := evaluatePlugin(true, true)
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass", got.Status)
		}
	})
}

func TestCheckPlugin_ReadsInjectedPaths(t *testing.T) {
	pluginsDir := t.TempDir()
	pluginsFile := writePluginsManifest(t, pluginsDir, "claude-session-state@orchardist")
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "abc.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	env := doctorEnv{pluginsFile: pluginsFile, pluginStateDir: stateDir}
	got := checkPlugin(env)
	if got.ID != "plugin" {
		t.Errorf("ID = %q; want plugin", got.ID)
	}
	if got.Status != statusPass {
		t.Errorf("Status = %v; want pass (detail: %s)", got.Status, got.Detail)
	}
}
