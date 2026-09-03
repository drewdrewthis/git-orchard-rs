package main

// Step 9: `orchard shell doctor` — a real health check replacing the stub
// that used to report itself unimplemented (docs/plans/747-product-plan.md
// step 9, AC8).
//
// Every check reads from an injected doctorEnv so the whole suite runs
// hermetically in tests: nothing but the production constructors below
// (newDoctorEnv, runTmux, runCommand) ever touches a real tmux server,
// daemon, systemctl or $PATH.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// doctorTimeout bounds the whole check suite. The daemon check also caps
// itself at daemonCheckTimeout; this is the ceiling on top of every
// subprocess exec (tmux, the six --version calls, systemctl) combined.
const doctorTimeout = 10 * time.Second

// checkStatus is AC8's tri-state: warn does not fail the overall run, fail
// does.
type checkStatus string

const (
	statusPass checkStatus = "pass"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
)

// checkResult is one check's outcome, in both the human and --json
// renderings — AC8 names this shape verbatim for data.checks[].
type checkResult struct {
	ID     string      `json:"id"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail"`
	Remedy string      `json:"remedy,omitempty"`
}

// cmdRunner runs an arbitrary command and returns its combined output,
// trimmed. Injected so the suite-versions and systemd checks are testable
// without spawning real binaries or systemctl.
type cmdRunner func(ctx context.Context, name string, args ...string) (string, error)

// runCommand is the production cmdRunner.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		return trimmed, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return trimmed, nil
}

// doctorEnv bundles every check's IO seam.
type doctorEnv struct {
	tmux        tmuxExec
	run         cmdRunner
	daemonURL   string
	goos        string
	self        string // selfPath(); "" if it could not be resolved
	selfVersion string // orchard-shell's own in-process version
	pathEnv     string // $PATH
	conf        string // outer.conf path, for the outer-socket check
	confErr     error  // set when conf could not be resolved
}

// newDoctorEnv is the production doctorEnv.
func newDoctorEnv(selfVersion string) doctorEnv {
	conf, confErr := resolveConf("")
	return doctorEnv{
		tmux:        runTmux,
		run:         runCommand,
		daemonURL:   "http://127.0.0.1:7777/graphql",
		goos:        runtime.GOOS,
		self:        selfPath(),
		selfVersion: selfVersion,
		pathEnv:     os.Getenv("PATH"),
		conf:        conf,
		confErr:     confErr,
	}
}

// runDoctor is the `orchard shell doctor` subcommand.
func runDoctor(opts Options, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()

	checks := runChecks(ctx, newDoctorEnv(version))
	failed := false
	for _, c := range checks {
		if c.Status == statusFail {
			failed = true
		}
	}
	update := loadUpdateInfo(version)

	if opts.DoctorJSON {
		writeDoctorJSON(stdout, checks, update, failed)
	} else {
		writeDoctorHuman(stdout, checks, update)
	}
	if failed {
		return 1
	}
	return 0
}

// runChecks runs every check, in AC8's order.
func runChecks(ctx context.Context, env doctorEnv) []checkResult {
	return []checkResult{
		checkTmuxVersion(env),
		checkTmuxNesting(),
		checkDaemon(ctx, env.daemonURL),
		checkSuiteVersions(ctx, env),
		checkInnerSocket(env),
		checkOuterSocket(env),
		checkSystemd(ctx, env),
		checkPath(env.self, env.pathEnv),
	}
}

// updateInfo is the cached update check's result, surfaced read-only by
// doctor. orchard-shell's background check writes the cache (updatecheck.go,
// plan step 10); doctor and the sidebar only ever read it.
type updateInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
}

// loadUpdateInfo reads the update-check cache, returning nil when no check
// has ever run — the same "absent file renders nothing" contract the sidebar
// follows (plan step 10, AC9).
func loadUpdateInfo(current string) *updateInfo {
	path, err := release.CheckPath()
	if err != nil {
		return nil
	}
	c := release.LoadCheck(path)
	if c.CheckedAt.IsZero() {
		return nil
	}
	return &updateInfo{Current: c.Current, Latest: c.Latest, Available: c.UpdateAvailable()}
}

func writeDoctorHuman(w io.Writer, checks []checkResult, update *updateInfo) {
	for _, c := range checks {
		fmt.Fprintf(w, "%s %-16s %s\n", statusGlyph(c.Status), c.ID, c.Detail)
		if c.Status != statusPass && c.Remedy != "" {
			fmt.Fprintf(w, "    remedy: %s\n", c.Remedy)
		}
	}
	if update != nil && update.Available {
		fmt.Fprintf(w, "update available: v%s -> v%s, run `orchard upgrade`\n", update.Current, update.Latest)
	}
}

func statusGlyph(s checkStatus) string {
	switch s {
	case statusPass:
		return "✓" // ✓
	case statusWarn:
		return "–" // –
	default:
		return "✗" // ✗
	}
}

// doctorEnvelope is the L2 shape (RULES.md): {"ok","data","error"}.
type doctorEnvelope struct {
	OK    bool             `json:"ok"`
	Data  *doctorData      `json:"data,omitempty"`
	Error *doctorJSONError `json:"error,omitempty"`
}

// doctorData is AC8's data payload — checks are always present, whether or
// not the run failed.
type doctorData struct {
	Checks []checkResult `json:"checks"`
	Update *updateInfo   `json:"update,omitempty"`
}

type doctorJSONError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeDoctorJSON(w io.Writer, checks []checkResult, update *updateInfo, failed bool) {
	env := doctorEnvelope{OK: !failed, Data: &doctorData{Checks: checks, Update: update}}
	if failed {
		env.Error = &doctorJSONError{Code: "checks-failed", Message: "one or more doctor checks failed"}
	}
	_ = json.NewEncoder(w).Encode(env)
}
