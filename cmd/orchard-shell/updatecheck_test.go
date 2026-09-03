package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

func TestSpawnUpdateCheck_SkipsWhenSelfIsEmpty(t *testing.T) {
	called := false
	fake := func(cmd *exec.Cmd) error { called = true; return nil }
	spawnUpdateCheck(fake, "", "1.0.0")
	if called {
		t.Error("processStarter was called with an empty self path")
	}
}

func TestSpawnUpdateCheck_SkipsWhenNoCheckEnvSet(t *testing.T) {
	t.Setenv(release.NoCheckEnv, "1")
	called := false
	fake := func(cmd *exec.Cmd) error { called = true; return nil }
	spawnUpdateCheck(fake, "/opt/orchard/orchard-shell", "1.0.0")
	if called {
		t.Error("processStarter was called with ORCHARD_NO_UPDATE_CHECK set")
	}
}

// @scenario orchard shell startup spawns the background update check
//
// Step 10: the check runs as a detached re-exec'd child, not a goroutine —
// see updatecheck.go's package doc for why. This proves the constructed
// *exec.Cmd carries the hidden flag, the current version, and Setsid so the
// child survives attach()'s syscall.Exec.
func TestSpawnUpdateCheck_ConstructsDetachedReexec(t *testing.T) {
	t.Setenv(release.NoCheckEnv, "")
	const self = "/opt/orchard/orchard-shell"

	var captured *exec.Cmd
	fake := func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	spawnUpdateCheck(fake, self, "1.4.0")

	if captured == nil {
		t.Fatal("processStarter was never called")
	}
	want := []string{self, updateCheckFlag, "1.4.0"}
	if !reflect.DeepEqual(captured.Args, want) {
		t.Errorf("Args = %v; want %v", captured.Args, want)
	}
	if captured.SysProcAttr == nil || !captured.SysProcAttr.Setsid {
		t.Errorf("SysProcAttr = %+v; want a Setsid session leader", captured.SysProcAttr)
	}
}

func TestSpawnUpdateCheck_IgnoresStarterError(t *testing.T) {
	t.Setenv(release.NoCheckEnv, "")
	fake := func(cmd *exec.Cmd) error { return errors.New("boom") }
	spawnUpdateCheck(fake, "/opt/orchard/orchard-shell", "1.0.0") // must not panic
}

// TestRunInternalUpdateCheck_WritesCacheFile drives the detached child's
// entrypoint end-to-end against a local fixture standing in for GitHub
// (release.RepoEnv), asserting the cache file RefreshCheck writes is the one
// doctor's loadUpdateInfo and the sidebar both read.
func TestRunInternalUpdateCheck_WritesCacheFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(release.RepoEnv, srv.URL)
	t.Setenv(release.NoCheckEnv, "")

	runInternalUpdateCheck("1.0.0")

	path, err := release.CheckPath()
	if err != nil {
		t.Fatal(err)
	}
	c := release.LoadCheck(path)
	if c.Current != "1.0.0" || c.Latest != "9.9.9" {
		t.Errorf("cached check = %+v; want current=1.0.0 latest=9.9.9", c)
	}
	if !c.UpdateAvailable() {
		t.Error("UpdateAvailable() = false; want true (9.9.9 > 1.0.0)")
	}
}

func TestRunInternalUpdateCheck_NoCheckEnvSkipsWritingACache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("fixture server was hit; want no network call when ORCHARD_NO_UPDATE_CHECK is set")
		w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(release.RepoEnv, srv.URL)
	t.Setenv(release.NoCheckEnv, "1")

	runInternalUpdateCheck("1.0.0")

	path, err := release.CheckPath()
	if err != nil {
		t.Fatal(err)
	}
	if c := release.LoadCheck(path); c.Current != "" {
		t.Errorf("cache file was written despite ORCHARD_NO_UPDATE_CHECK: %+v", c)
	}
}
