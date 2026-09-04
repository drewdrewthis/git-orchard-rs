// Unit tests for the tmux child env: utf8Env (D3 locale fix, #701) and
// stripTmuxSocketEnv (parent socket inheritance, #699).

package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ctypeOf extracts the LC_CTYPE value from an env slice, "" if absent.
func ctypeOf(env []string) string {
	val := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "LC_CTYPE" {
			val = v
		}
	}
	return val
}

// hasKey reports whether env contains any entry for key.
func hasKey(env []string, key string) bool {
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k == key {
			return true
		}
	}
	return false
}

func TestUtf8Env(t *testing.T) {
	cases := []struct {
		name          string
		in            []string
		wantUnchanged bool     // env returned byte-identical
		wantCtype     string   // expected LC_CTYPE when not unchanged
		wantNoLCAll   bool     // LC_ALL must be absent when not unchanged
		wantKeep      []string // KEY=VALUE entries that must survive
	}{
		{
			name:          "LANG C.UTF-8 unchanged",
			in:            []string{"LANG=C.UTF-8", "PATH=/bin"},
			wantUnchanged: true,
		},
		{
			name:          "LANG en_US.UTF-8 unchanged",
			in:            []string{"LANG=en_US.UTF-8"},
			wantUnchanged: true,
		},
		{
			name:        "empty env gains C.UTF-8",
			in:          []string{},
			wantCtype:   "C.UTF-8",
			wantNoLCAll: true,
		},
		{
			name:        "LC_ALL=C removed and ctype forced",
			in:          []string{"LC_ALL=C", "PATH=/bin"},
			wantCtype:   "C.UTF-8",
			wantNoLCAll: true,
			wantKeep:    []string{"PATH=/bin"},
		},
		{
			name:        "LC_ALL=C LC_CTYPE=C.UTF-8 strips LC_ALL",
			in:          []string{"LC_ALL=C", "LC_CTYPE=C.UTF-8"},
			wantCtype:   "C.UTF-8",
			wantNoLCAll: true,
		},
		{
			name:      "LC_CTYPE POSIX beats LANG en_US.UTF-8",
			in:        []string{"LC_CTYPE=POSIX", "LANG=en_US.UTF-8"},
			wantCtype: "C.UTF-8",
			wantKeep:  []string{"LANG=en_US.UTF-8"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := utf8Env(tc.in)
			if tc.wantUnchanged {
				if len(got) != len(tc.in) {
					t.Fatalf("expected env unchanged, got %v (in %v)", got, tc.in)
				}
				for i := range tc.in {
					if got[i] != tc.in[i] {
						t.Fatalf("expected env unchanged; index %d: got %q want %q", i, got[i], tc.in[i])
					}
				}
				return
			}
			if c := ctypeOf(got); c != tc.wantCtype {
				t.Errorf("LC_CTYPE: got %q want %q (env %v)", c, tc.wantCtype, got)
			}
			if tc.wantNoLCAll && hasKey(got, "LC_ALL") {
				t.Errorf("LC_ALL must be stripped, got %v", got)
			}
			for _, keep := range tc.wantKeep {
				found := false
				for _, kv := range got {
					if kv == keep {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected %q to survive, got %v", keep, got)
				}
			}
		})
	}
}

func TestStripTmuxSocketEnv(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		wantDrop []string // keys that must be absent
		wantKeep []string // KEY=VALUE entries that must survive
	}{
		{
			name:     "drops TMUX and TMUX_PANE keeps rest",
			in:       []string{"TMUX=/tmp/sock,1,0", "TMUX_PANE=%1", "PATH=/bin", "TMUX_TMPDIR=/run/user/1000"},
			wantDrop: []string{"TMUX", "TMUX_PANE"},
			wantKeep: []string{"PATH=/bin", "TMUX_TMPDIR=/run/user/1000"},
		},
		{
			name:     "TMUX_TMPDIR survives when no TMUX present",
			in:       []string{"TMUX_TMPDIR=/run/user/1000", "LANG=C.UTF-8"},
			wantKeep: []string{"TMUX_TMPDIR=/run/user/1000", "LANG=C.UTF-8"},
		},
		{
			name: "empty env ok",
			in:   []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripTmuxSocketEnv(tc.in)
			for _, drop := range tc.wantDrop {
				if hasKey(got, drop) {
					t.Errorf("%s must be dropped, got %v", drop, got)
				}
			}
			for _, keep := range tc.wantKeep {
				found := false
				for _, kv := range got {
					if kv == keep {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected %q to survive, got %v", keep, got)
				}
			}
		})
	}
}

// TestExecRunner_StripsParentTmuxSocketFromChild proves the production
// execRunner does NOT leak the launching shell's TMUX/TMUX_PANE into the tmux
// child (#699) — otherwise a daemon started inside a non-default tmux socket
// would silently address that socket instead of the default server. A fake
// `tmux` shim on PATH echoes the two vars it received.
func TestExecRunner_StripsParentTmuxSocketFromChild(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\necho \"$TMUX|$TMUX_PANE\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "/tmp/fake,1,0")
	t.Setenv("TMUX_PANE", "%1")

	out, err := execRunner{}.Run(context.Background(), "tmux", "list-sessions")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.TrimSpace(string(out))
	if got != "|" {
		t.Errorf("child should see no TMUX/TMUX_PANE, got %q (want %q)", got, "|")
	}
}

// TestExecRunner_PassesUTF8CtypeToChild proves the production execRunner spawns
// its child with a UTF-8 LC_CTYPE even when the daemon's own environment has no
// UTF-8 locale — the systemd `--user` / launchd class (#701 D3). A fake `tmux`
// shim on PATH echoes the locale vars it received.
func TestExecRunner_PassesUTF8CtypeToChild(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\necho \"$LC_ALL|$LC_CTYPE|$LANG\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}

	// Scrub the locale in the test process env — utf8Env reads os.Environ().
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", "")

	out, err := execRunner{}.Run(context.Background(), "tmux", "list-sessions")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) != 3 {
		t.Fatalf("shim output malformed: %q", out)
	}
	lcAll, lcCtype := parts[0], parts[1]
	if lcCtype != "C.UTF-8" {
		t.Errorf("child LC_CTYPE: got %q want C.UTF-8", lcCtype)
	}
	if lcAll != "" {
		t.Errorf("child LC_ALL should be empty (stripped), got %q", lcAll)
	}
}
