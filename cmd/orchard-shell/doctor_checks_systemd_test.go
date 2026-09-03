package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// --- systemd -------------------------------------------------------------

// @scenario orchard shell doctor — systemd check accepts either service unit
//
// AC8 + real-world install (the sweatshop): the daemon can be active as
// either orchard-daemon.service (the unit this repo actually ships as
// active today, scripts/install.sh's SERVICE_UNITS[0]) or orchard.service
// (install_service's template name, checked second). evaluateSystemd names
// whichever unit is actually active and fails only when neither is.
func TestEvaluateSystemd(t *testing.T) {
	notFoundErr := &exec.Error{Name: "systemctl", Err: exec.ErrNotFound}
	inactiveErr := errors.New("systemctl --user is-active: exit status 3")

	tests := []struct {
		name       string
		results    []systemdUnitCheck
		want       checkStatus
		wantDetail []string // substrings Detail must contain
		wantRemedy []string // substrings Remedy must contain
	}{
		{
			name:       "orchard-daemon.service active",
			results:    []systemdUnitCheck{{unit: "orchard-daemon.service", output: "active", err: nil}},
			want:       statusPass,
			wantDetail: []string{"orchard-daemon.service"},
		},
		{
			name: "orchard-daemon.service inactive, orchard.service active",
			results: []systemdUnitCheck{
				{unit: "orchard-daemon.service", output: "inactive", err: inactiveErr},
				{unit: "orchard.service", output: "active", err: nil},
			},
			want:       statusPass,
			wantDetail: []string{"orchard.service"},
		},
		{
			name: "neither unit active fails",
			results: []systemdUnitCheck{
				{unit: "orchard-daemon.service", output: "inactive", err: inactiveErr},
				{unit: "orchard.service", output: "inactive", err: inactiveErr},
			},
			want:       statusFail,
			wantDetail: []string{"orchard-daemon.service", "orchard.service"},
			wantRemedy: []string{"install.sh", "systemctl --user start orchard"},
		},
		{
			name:    "systemctl not found warns",
			results: []systemdUnitCheck{{unit: "orchard-daemon.service", output: "", err: notFoundErr}},
			want:    statusWarn,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateSystemd(tt.results)
			if got.Status != tt.want {
				t.Errorf("evaluateSystemd(%+v) = %v; want %v (detail: %s)", tt.results, got.Status, tt.want, got.Detail)
			}
			if got.ID != "systemd" {
				t.Errorf("ID = %q; want systemd", got.ID)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("Detail = %q; want it to contain %q", got.Detail, want)
				}
			}
			for _, want := range tt.wantRemedy {
				if !strings.Contains(got.Remedy, want) {
					t.Errorf("Remedy = %q; want it to contain %q", got.Remedy, want)
				}
			}
		})
	}
}

// TestResolveSystemdUnits exercises the impure half against an injected
// doctorEnv.run — the priority-order, short-circuiting loop that mirrors
// scripts/install.sh's detect_active_service.
func TestResolveSystemdUnits(t *testing.T) {
	t.Run("stops at the first active unit", func(t *testing.T) {
		calls := 0
		env := doctorEnv{run: func(ctx context.Context, name string, args ...string) (string, error) {
			calls++
			unit := args[len(args)-1]
			if unit != "orchard-daemon.service" {
				t.Fatalf("called for %q; want it to stop after the first (active) unit", unit)
			}
			return "active", nil
		}}
		got := resolveSystemdUnits(context.Background(), env)
		if len(got) != 1 || got[0].unit != "orchard-daemon.service" || got[0].err != nil {
			t.Errorf("results = %+v; want exactly one active orchard-daemon.service result", got)
		}
		if calls != 1 {
			t.Errorf("calls = %d; want exactly 1", calls)
		}
	})

	t.Run("falls through to the second unit when the first is inactive", func(t *testing.T) {
		env := doctorEnv{run: func(ctx context.Context, name string, args ...string) (string, error) {
			unit := args[len(args)-1]
			if unit == "orchard.service" {
				return "active", nil
			}
			return "inactive", errors.New("inactive")
		}}
		got := resolveSystemdUnits(context.Background(), env)
		if len(got) != 2 {
			t.Fatalf("results = %+v; want both units tried", got)
		}
		if got[0].unit != "orchard-daemon.service" || got[1].unit != "orchard.service" {
			t.Errorf("results = %+v; want priority order orchard-daemon.service, orchard.service", got)
		}
		if got[1].err != nil {
			t.Errorf("second result err = %v; want nil (active)", got[1].err)
		}
	})

	t.Run("tries both units when both are inactive", func(t *testing.T) {
		calls := 0
		env := doctorEnv{run: func(ctx context.Context, name string, args ...string) (string, error) {
			calls++
			return "inactive", errors.New("inactive")
		}}
		got := resolveSystemdUnits(context.Background(), env)
		if len(got) != 2 || calls != 2 {
			t.Errorf("results = %+v, calls = %d; want both units tried", got, calls)
		}
	})

	t.Run("stops after systemctl-not-found on the first unit", func(t *testing.T) {
		calls := 0
		env := doctorEnv{run: func(ctx context.Context, name string, args ...string) (string, error) {
			calls++
			return "", &exec.Error{Name: "systemctl", Err: exec.ErrNotFound}
		}}
		got := resolveSystemdUnits(context.Background(), env)
		if len(got) != 1 {
			t.Errorf("results = %+v; want exactly one result", got)
		}
		if calls != 1 {
			t.Errorf("calls = %d; want exactly 1 (no point retrying a missing systemctl)", calls)
		}
	})
}

func TestCheckSystemd_NonLinuxPassesWithoutRunningAnything(t *testing.T) {
	calls := 0
	env := doctorEnv{
		goos: "darwin",
		run: func(ctx context.Context, name string, args ...string) (string, error) {
			calls++
			return "", nil
		},
	}
	got := checkSystemd(context.Background(), env)
	if got.Status != statusPass {
		t.Errorf("Status = %v; want pass", got.Status)
	}
	if calls != 0 {
		t.Errorf("checkSystemd ran a command on non-linux goos; want zero calls")
	}
}

func TestCheckSystemd_LinuxRunsSystemctl(t *testing.T) {
	env := doctorEnv{
		goos: "linux",
		run: func(ctx context.Context, name string, args ...string) (string, error) {
			if name != "systemctl" {
				t.Errorf("ran %q; want systemctl", name)
			}
			if len(args) == 0 || args[len(args)-1] != "orchard-daemon.service" {
				t.Errorf("args = %v; want the last arg to be orchard-daemon.service (priority order)", args)
			}
			return "active", nil
		},
	}
	got := checkSystemd(context.Background(), env)
	if got.Status != statusPass {
		t.Errorf("Status = %v; want pass", got.Status)
	}
}

// TestCheckSystemd_SweatshopBug reproduces the real-world install where the
// daemon runs as the user unit orchard-daemon.service (active) while
// orchard.service — the only unit the old, hardcoded check looked at — was
// never installed at all. The check must pass and name the unit that is
// actually active, not fail claiming "orchard.service is not active".
func TestCheckSystemd_SweatshopBug(t *testing.T) {
	env := doctorEnv{
		goos: "linux",
		run: func(ctx context.Context, name string, args ...string) (string, error) {
			switch unit := args[len(args)-1]; unit {
			case "orchard-daemon.service":
				return "active", nil
			case "orchard.service":
				return "inactive", errors.New("systemctl --user is-active orchard.service: exit status 3")
			default:
				t.Fatalf("unexpected unit %q", unit)
				return "", nil
			}
		},
	}
	got := checkSystemd(context.Background(), env)
	if got.Status != statusPass {
		t.Errorf("Status = %v; want pass (detail: %s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "orchard-daemon.service") {
		t.Errorf("Detail = %q; want it to name orchard-daemon.service as the active unit", got.Detail)
	}
}
