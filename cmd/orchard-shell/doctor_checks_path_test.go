package main

import (
	"os/exec"
	"strings"
	"testing"
)

// --- PATH ------------------------------------------------------------------

// TestResolvePathShadows exercises the impure half against an injected
// pathLookup — no real binaries or $PATH touched.
func TestResolvePathShadows(t *testing.T) {
	const self = "/opt/orchard/orchard-shell"

	t.Run("self unresolved returns no shadows without calling lookup", func(t *testing.T) {
		called := false
		got := resolvePathShadows("", func(name string) (string, error) {
			called = true
			return "", nil
		})
		if got != nil {
			t.Errorf("shadows = %+v; want nil", got)
		}
		if called {
			t.Error("lookup was called despite self being unresolved")
		}
	})

	t.Run("nil lookup returns no shadows", func(t *testing.T) {
		if got := resolvePathShadows(self, nil); got != nil {
			t.Errorf("shadows = %+v; want nil", got)
		}
	})

	t.Run("every suite binary resolves beside self: no shadows", func(t *testing.T) {
		got := resolvePathShadows(self, func(name string) (string, error) {
			return "/opt/orchard/" + name, nil
		})
		if len(got) != 0 {
			t.Errorf("shadows = %+v; want none", got)
		}
	})

	t.Run("a binary missing from PATH entirely is not a shadow", func(t *testing.T) {
		got := resolvePathShadows(self, func(name string) (string, error) {
			return "", exec.ErrNotFound
		})
		if len(got) != 0 {
			t.Errorf("shadows = %+v; want none (not-found is a different problem)", got)
		}
	})

	// The Mac bug: ~/go/bin/orchard-daemon (a stale `go install`, version
	// dev) precedes the real install dir on $PATH.
	t.Run("a stale duplicate earlier on PATH is flagged", func(t *testing.T) {
		got := resolvePathShadows(self, func(name string) (string, error) {
			if name == "orchard-daemon" {
				return "/home/u/go/bin/orchard-daemon", nil
			}
			return "/opt/orchard/" + name, nil
		})
		if len(got) != 1 {
			t.Fatalf("shadows = %+v; want exactly one", got)
		}
		s := got[0]
		if s.name != "orchard-daemon" {
			t.Errorf("name = %q; want orchard-daemon", s.name)
		}
		if s.resolved != "/home/u/go/bin/orchard-daemon" {
			t.Errorf("resolved = %q; want the stale path", s.resolved)
		}
		if s.prefix != "/opt/orchard/orchard-daemon" {
			t.Errorf("prefix = %q; want the sibling path beside self", s.prefix)
		}
	})
}

func TestEvaluatePath(t *testing.T) {
	t.Run("self unresolved warns", func(t *testing.T) {
		got := evaluatePath("", "/usr/bin", nil)
		if got.Status != statusWarn {
			t.Errorf("Status = %v; want warn", got.Status)
		}
	})

	t.Run("install dir missing from PATH fails regardless of shadows", func(t *testing.T) {
		shadows := []pathShadow{{name: "orchard-daemon", resolved: "/x/orchard-daemon", prefix: "/opt/orchard/orchard-daemon"}}
		got := evaluatePath("/opt/orchard/orchard-shell", "/usr/bin:/bin", shadows)
		if got.Status != statusFail {
			t.Errorf("Status = %v; want fail", got.Status)
		}
	})

	t.Run("install dir on PATH with no shadows passes", func(t *testing.T) {
		got := evaluatePath("/opt/orchard/orchard-shell", "/usr/bin:/opt/orchard:/bin", nil)
		if got.Status != statusPass {
			t.Errorf("Status = %v; want pass", got.Status)
		}
	})

	t.Run("install dir on PATH but a shadow present warns and names it", func(t *testing.T) {
		shadows := []pathShadow{
			{name: "orchard-daemon", resolved: "/home/u/go/bin/orchard-daemon", prefix: "/opt/orchard/orchard-daemon"},
		}
		got := evaluatePath("/opt/orchard/orchard-shell", "/usr/bin:/opt/orchard:/bin", shadows)
		if got.Status != statusWarn {
			t.Errorf("Status = %v; want warn", got.Status)
		}
		if !strings.Contains(got.Detail, "orchard-daemon: /home/u/go/bin/orchard-daemon shadows /opt/orchard/orchard-daemon") {
			t.Errorf("Detail = %q; want it to name the shadow", got.Detail)
		}
		if !strings.Contains(got.Remedy, "reorder PATH or remove the stale binary") {
			t.Errorf("Remedy = %q; want the reorder/remove remedy", got.Remedy)
		}
	})
}

// @scenario orchard shell doctor — path check catches a shadowed suite binary
//
// TestCheckPath_MacBug reproduces the real-world install where
// ~/go/bin/orchard-daemon (a stale `go install`, version dev) precedes
// ~/.local/bin on $PATH and silently shadows the real orchard-daemon
// installed beside orchard-shell. The old check only asked "is
// orchard-shell's own dir on $PATH" and passed; it must now warn and name
// the shadow.
func TestCheckPath_MacBug(t *testing.T) {
	env := doctorEnv{
		self:    "/Users/u/.local/bin/orchard-shell",
		pathEnv: "/Users/u/go/bin:/Users/u/.local/bin:/usr/bin",
		lookPath: func(name string) (string, error) {
			if name == "orchard-daemon" {
				return "/Users/u/go/bin/orchard-daemon", nil
			}
			return "/Users/u/.local/bin/" + name, nil
		},
	}
	got := checkPath(env)
	if got.Status != statusWarn {
		t.Errorf("Status = %v; want warn (detail: %s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "orchard-daemon: /Users/u/go/bin/orchard-daemon shadows /Users/u/.local/bin/orchard-daemon") {
		t.Errorf("Detail = %q; want it to name the shadowing orchard-daemon", got.Detail)
	}
}

func TestCheckPath_NoShadowsPasses(t *testing.T) {
	env := doctorEnv{
		self:     "/opt/orchard/orchard-shell",
		pathEnv:  "/usr/bin:/opt/orchard:/bin",
		lookPath: func(name string) (string, error) { return "/opt/orchard/" + name, nil },
	}
	got := checkPath(env)
	if got.Status != statusPass {
		t.Errorf("Status = %v; want pass (detail: %s)", got.Status, got.Detail)
	}
}

func TestCheckPath_SelfUnresolvedWarns(t *testing.T) {
	got := checkPath(doctorEnv{self: "", pathEnv: "/usr/bin"})
	if got.Status != statusWarn {
		t.Errorf("Status = %v; want warn", got.Status)
	}
}

func TestCheckPath_InstallDirMissingFromPathFails(t *testing.T) {
	env := doctorEnv{
		self:     "/opt/orchard/orchard-shell",
		pathEnv:  "/usr/bin:/bin",
		lookPath: func(name string) (string, error) { return "/opt/orchard/" + name, nil },
	}
	got := checkPath(env)
	if got.Status != statusFail {
		t.Errorf("Status = %v; want fail", got.Status)
	}
}
