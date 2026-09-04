package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

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
