package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/release"
)

func TestStatusGlyph(t *testing.T) {
	tests := []struct {
		status checkStatus
		want   string
	}{
		{statusPass, "✓"},
		{statusWarn, "–"},
		{statusFail, "✗"},
	}
	for _, tt := range tests {
		if got := statusGlyph(tt.status); got != tt.want {
			t.Errorf("statusGlyph(%v) = %q; want %q", tt.status, got, tt.want)
		}
	}
}

func TestWriteDoctorHuman(t *testing.T) {
	checks := []checkResult{
		{ID: "tmux", Status: statusPass, Detail: "tmux 3.6a"},
		{ID: "daemon", Status: statusFail, Detail: "unreachable", Remedy: "systemctl --user start orchard"},
		{ID: "tmux-nesting", Status: statusWarn, Detail: "nested", Remedy: ""},
	}
	var buf strings.Builder
	writeDoctorHuman(&buf, checks, nil)
	out := buf.String()

	if !strings.Contains(out, "✓ tmux") || !strings.Contains(out, "tmux 3.6a") {
		t.Errorf("output missing the passing tmux line: %q", out)
	}
	if !strings.Contains(out, "✗ daemon") {
		t.Errorf("output missing the failing daemon line: %q", out)
	}
	if !strings.Contains(out, "remedy: systemctl --user start orchard") {
		t.Errorf("output missing the daemon remedy line: %q", out)
	}
	if !strings.Contains(out, "– tmux-nesting") {
		t.Errorf("output missing the warn tmux-nesting line: %q", out)
	}
	// tmux-nesting has an empty remedy: no "remedy:" line should follow it.
	if strings.Count(out, "remedy:") != 1 {
		t.Errorf("expected exactly one remedy line, got: %q", out)
	}
}

func TestWriteDoctorHuman_PrintsUpdateLineOnlyWhenAvailable(t *testing.T) {
	checks := []checkResult{{ID: "tmux", Status: statusPass, Detail: "ok"}}

	t.Run("no update info prints nothing extra", func(t *testing.T) {
		var buf strings.Builder
		writeDoctorHuman(&buf, checks, nil)
		if strings.Contains(buf.String(), "update available") {
			t.Errorf("unexpected update line: %q", buf.String())
		}
	})

	t.Run("update present but not available prints nothing extra", func(t *testing.T) {
		var buf strings.Builder
		writeDoctorHuman(&buf, checks, &updateInfo{Current: "1.0.0", Latest: "1.0.0", Available: false})
		if strings.Contains(buf.String(), "update available") {
			t.Errorf("unexpected update line: %q", buf.String())
		}
	})

	t.Run("update available prints the upgrade line", func(t *testing.T) {
		var buf strings.Builder
		writeDoctorHuman(&buf, checks, &updateInfo{Current: "1.0.0", Latest: "1.4.0", Available: true})
		out := buf.String()
		if !strings.Contains(out, "update available: v1.0.0 -> v1.4.0, run `orchard upgrade`") {
			t.Errorf("output missing the update line: %q", out)
		}
	})
}

// AC8: every check appears in --json data.checks[] with
// {id,status,detail,remedy} and status in {pass,warn,fail}.
func TestWriteDoctorJSON_Shape(t *testing.T) {
	checks := []checkResult{
		{ID: "tmux", Status: statusPass, Detail: "tmux 3.6a"},
		{ID: "daemon", Status: statusFail, Detail: "unreachable", Remedy: "systemctl --user start orchard"},
	}
	var buf strings.Builder
	writeDoctorJSON(&buf, checks, nil, true)

	var env doctorEnvelope
	if err := json.Unmarshal([]byte(buf.String()), &env); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if env.OK {
		t.Error("OK = true; want false when failed")
	}
	if env.Error == nil {
		t.Fatal("Error is nil; want it set when failed")
	}
	if env.Data == nil || len(env.Data.Checks) != 2 {
		t.Fatalf("Data.Checks = %+v; want the 2 input checks preserved", env.Data)
	}
	for _, c := range env.Data.Checks {
		switch c.Status {
		case statusPass, statusWarn, statusFail:
		default:
			t.Errorf("check %q has invalid status %q", c.ID, c.Status)
		}
	}
}

// AC8: data.checks is present whether or not the run failed — the envelope
// must not omit it on success.
func TestWriteDoctorJSON_OKTrueStillCarriesData(t *testing.T) {
	checks := []checkResult{{ID: "tmux", Status: statusPass, Detail: "tmux 3.6a"}}
	var buf strings.Builder
	writeDoctorJSON(&buf, checks, nil, false)

	var env doctorEnvelope
	if err := json.Unmarshal([]byte(buf.String()), &env); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if !env.OK {
		t.Error("OK = false; want true")
	}
	if env.Error != nil {
		t.Errorf("Error = %+v; want nil on success", env.Error)
	}
	if env.Data == nil || len(env.Data.Checks) != 1 {
		t.Fatalf("Data.Checks = %+v; want the 1 input check preserved even though ok=true", env.Data)
	}
}

func TestWriteDoctorJSON_IncludesUpdateWhenPresent(t *testing.T) {
	checks := []checkResult{{ID: "tmux", Status: statusPass, Detail: "ok"}}
	var buf strings.Builder
	writeDoctorJSON(&buf, checks, &updateInfo{Current: "1.0.0", Latest: "1.4.0", Available: true}, false)

	var env doctorEnvelope
	if err := json.Unmarshal([]byte(buf.String()), &env); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if env.Data.Update == nil || !env.Data.Update.Available || env.Data.Update.Latest != "1.4.0" {
		t.Errorf("Data.Update = %+v; want the update surfaced verbatim", env.Data.Update)
	}
}

func TestLoadUpdateInfo(t *testing.T) {
	t.Run("no cache file returns nil", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		if got := loadUpdateInfo("1.0.0"); got != nil {
			t.Errorf("loadUpdateInfo = %+v; want nil with no cache", got)
		}
	})

	t.Run("cached check is surfaced with Available computed", func(t *testing.T) {
		stateHome := t.TempDir()
		t.Setenv("XDG_STATE_HOME", stateHome)
		cachePath := filepath.Join(stateHome, "orchard", release.CheckFile)
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
			t.Fatal(err)
		}
		check := release.Check{CheckedAt: time.Now(), Current: "1.0.0", Latest: "1.4.0"}
		data, err := json.Marshal(check)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cachePath, data, 0o644); err != nil {
			t.Fatal(err)
		}

		got := loadUpdateInfo("1.0.0")
		if got == nil {
			t.Fatal("loadUpdateInfo = nil; want the cached check")
		}
		if got.Current != "1.0.0" || got.Latest != "1.4.0" || !got.Available {
			t.Errorf("loadUpdateInfo = %+v; want current=1.0.0 latest=1.4.0 available=true", got)
		}
	})
}

// AC8's exact order: tmux, tmux-nesting, daemon, suite-versions,
// inner-socket, outer-socket, systemd, path — 8 checks, one per line in the
// human renderer.
func TestRunChecks_ReturnsAllEightInDocumentedOrder(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"version":"1.0.0"}}`))
	}))
	defer daemon.Close()

	t.Setenv("PATH", t.TempDir()) // no real suite binaries findable
	t.Setenv("TMUX", "")

	f := newFakeTmux()
	env := doctorEnv{
		tmux:        f.exec,
		run:         func(ctx context.Context, name string, args ...string) (string, error) { return "", nil },
		daemonURL:   daemon.URL,
		goos:        "darwin",
		self:        filepath.Join(t.TempDir(), "orchard-shell"),
		selfVersion: "1.0.0",
		pathEnv:     "",
		conf:        "/fake/outer.conf",
	}

	got := runChecks(context.Background(), env)
	want := []string{"tmux", "tmux-nesting", "daemon", "suite-versions", "inner-socket", "outer-socket", "systemd", "path"}
	if len(got) != len(want) {
		t.Fatalf("runChecks returned %d results; want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("checks[%d].ID = %q; want %q", i, got[i].ID, id)
		}
		switch got[i].Status {
		case statusPass, statusWarn, statusFail:
		default:
			t.Errorf("checks[%d] (%s) has invalid status %q", i, id, got[i].Status)
		}
	}
}

func TestNewDoctorEnv_PopulatesProductionSeams(t *testing.T) {
	env := newDoctorEnv("1.2.3", "default", "orchard-shell")
	if env.tmux == nil {
		t.Error("tmux seam is nil")
	}
	if env.run == nil {
		t.Error("run seam is nil")
	}
	if env.daemonURL != "http://127.0.0.1:7777/graphql" {
		t.Errorf("daemonURL = %q; want the production daemon endpoint", env.daemonURL)
	}
	if env.selfVersion != "1.2.3" {
		t.Errorf("selfVersion = %q; want 1.2.3", env.selfVersion)
	}
	if env.innerSocket != "default" {
		t.Errorf("innerSocket = %q; want default", env.innerSocket)
	}
	if env.outerSocket != "orchard-shell" {
		t.Errorf("outerSocket = %q; want orchard-shell", env.outerSocket)
	}
}
