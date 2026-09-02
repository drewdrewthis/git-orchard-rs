package claudeaccount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ShellAdapter implements adapter.Adapter[AccountID, Account] by
// shelling out to the local `claude` and `ccusage` CLIs.
//
// Stateless per ADR-011 §3 — the calling Provider owns the cache and
// the watcher loop. Each Fetch / FetchAll triggers fresh shellouts;
// throttling is the Provider's job.
//
// hostID is constant for the lifetime of one daemon process; tests
// inject a fixture id so the GraphQL responses are deterministic.
//
// `runner` is overridable for tests so we can avoid touching a real
// `claude` binary in unit tests. Production wires execRunner.
type ShellAdapter struct {
	hostID string
	logger *slog.Logger
	runner CommandRunner
}

// CommandRunner is the test seam. Production wires execRunner, which
// kills the entire process tree on context cancellation. Tests can
// inject a stub that emits canned output without touching exec.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner is the real implementation. It uses exec.CommandContext
// with a Cancel that signals SIGKILL to the entire process group, so
// `bunx ccusage ...` (which forks bun) cannot leak children when the
// daemon's context is cancelled.
//
// The tool name is resolved to an absolute path before exec — see
// toolpath.go for why bare-name lookup is not enough under launchd.
type execRunner struct {
	locator *toolLocator
}

// newExecRunner constructs the production runner. Logger may be nil.
func newExecRunner(logger *slog.Logger) *execRunner {
	return &execRunner{locator: newToolLocator(logger)}
}

// Run shells out and returns the merged stdout. On context cancel,
// SIGKILL is sent to the negative pid (the process group) so any
// helpers `claude` / `ccusage` forked die with their parent.
//
// Stdout-bearing failures: if the command exits non-zero with stdout,
// we still return ErrToolNotInstalled when the underlying error is
// exec.ErrNotFound; otherwise we return a generic shell error. The
// caller is responsible for inspecting the error vs the bytes.
//
// We deliberately do not log stdout — `claude auth status` includes
// the user's email and PII rules say no raw stdout in logs.
func (r *execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	bin, err := r.locator.Locate(name)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// New process group so Cancel can target all descendants with one
	// signal. Without this, killing the parent would orphan helpers
	// like the bun runtime that bunx forks.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the process group. Best-effort — if
		// the group is already gone, syscall.Kill returns ESRCH which
		// the caller doesn't care about.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, &ToolNotInstalledError{Tool: name}
		}
		// Surface the exit code without echoing stdout (PII). stderr
		// is already captured into ExitError.Stderr; we keep that
		// because it does not contain the auth subject.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("%s exited %d: %s", name, ee.ExitCode(), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// NewShellAdapter constructs a ShellAdapter rooted at the given host
// id. Logger may be nil; defaults to slog.Default().
func NewShellAdapter(hostID string, logger *slog.Logger) *ShellAdapter {
	if logger == nil {
		logger = slog.Default()
	}
	return &ShellAdapter{
		hostID: hostID,
		logger: logger,
		runner: newExecRunner(logger),
	}
}

// HostID exposes the host prefix every AccountID this adapter mints
// will carry.
func (a *ShellAdapter) HostID() string { return a.hostID }

// WithRunner returns a copy of a using the given runner. For tests.
func (a *ShellAdapter) WithRunner(r CommandRunner) *ShellAdapter {
	cp := *a
	cp.runner = r
	return &cp
}

// Fetch returns the Account for one AccountID. v1 only knows the local
// account; any other key returns a not-found error. The shellout cost
// is identical to FetchAll, so we just delegate.
func (a *ShellAdapter) Fetch(ctx context.Context, id AccountID) (Account, error) {
	all, err := a.FetchAll(ctx)
	if err != nil {
		return Account{}, err
	}
	acc, ok := all[id]
	if !ok {
		return Account{}, fmt.Errorf("claudeaccount: account %s not found", id.GraphQLID())
	}
	return acc, nil
}

// FetchAll runs `claude auth status --json` and `ccusage blocks --json`
// in sequence and merges the results into the single Account v1
// surfaces. Errors when neither tool is installed; degrades to
// quota-less Account when ccusage alone is missing (claude is the
// load-bearing one — without it we have no identity to attach quota
// to).
func (a *ShellAdapter) FetchAll(ctx context.Context) (map[AccountID]Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	email, err := a.fetchEmail(ctx)
	if err != nil {
		return nil, err
	}
	id := AccountID{HostID: a.hostID, Email: email}

	acc := Account{
		ID:             id,
		QuotaEstimated: true, // ccusage is the only source v1 supports
	}

	// Quota is best-effort. A missing ccusage is reported up so the
	// resolver can choose to surface a per-field error, but it does
	// not blank the email field.
	used, cap_, resets, qErr := a.fetchQuota(ctx)
	if qErr != nil {
		// Wrap so callers can inspect with errors.Is, but also keep
		// the partial Account so callers can render the email field.
		return map[AccountID]Account{id: acc}, qErr
	}
	acc.QuotaUsed = used
	acc.QuotaCap = cap_
	acc.QuotaResetsAt = resets

	return map[AccountID]Account{id: acc}, nil
}

// authStatus is the JSON shape we expect from `claude auth status
// --json`. We pick out the fields we need and ignore the rest — claude
// adds cosmetic fields between releases and we do not want a parser
// crash to erase the user's email from the daemon.
type authStatus struct {
	Email   string `json:"email"`
	Whoami  string `json:"whoami"` // older claude versions used `whoami`
	Account struct {
		Email string `json:"email"`
	} `json:"account"`
}

// fetchEmail runs `claude auth status --json` and returns the email
// field. Tries the well-known field locations in order:
// `email`, `account.email`, `whoami`. Stops at the first non-empty
// match. Returns an empty string if claude is signed out (no error —
// the account exists, just unauthenticated).
func (a *ShellAdapter) fetchEmail(ctx context.Context) (string, error) {
	out, err := a.runner.Run(ctx, "claude", "auth", "status", "--json")
	if err != nil {
		return "", err
	}
	var s authStatus
	if err := json.Unmarshal(out, &s); err != nil {
		// Avoid logging `out` directly — it contains the email if the
		// JSON is well-formed. Keep the parser context terse.
		return "", fmt.Errorf("claudeaccount: parse auth status: %w", err)
	}
	switch {
	case s.Email != "":
		return s.Email, nil
	case s.Account.Email != "":
		return s.Account.Email, nil
	case s.Whoami != "":
		return s.Whoami, nil
	default:
		return "", nil
	}
}

// ccusageBlocks is the JSON shape we expect from
// `ccusage blocks --json`. ccusage emits one record per quota window
// (a "block"); we want the active one.
type ccusageBlocks struct {
	Blocks []struct {
		Active    bool     `json:"active"`
		Used      *float64 `json:"used"`
		Cap       *float64 `json:"cap"`
		Limit     *float64 `json:"limit"`     // newer ccusage names it `limit`
		ResetsAt  string   `json:"resetsAt"`  // RFC 3339
		ResetTime string   `json:"resetTime"` // alias on older versions
	} `json:"blocks"`
}

// fetchQuota runs `ccusage blocks --json` and returns the
// (used, cap, resetsAt) triple for the currently active block. Each
// piece is returned as a pointer so callers can distinguish "ccusage
// reported nothing" from "ccusage reported zero".
//
// If ccusage is not installed, returns (nil, nil, nil,
// *ToolNotInstalledError) so callers can map to a per-field error.
func (a *ShellAdapter) fetchQuota(ctx context.Context) (*float64, *float64, *time.Time, error) {
	out, err := a.runner.Run(ctx, "ccusage", "blocks", "--json")
	if err != nil {
		return nil, nil, nil, err
	}
	var c ccusageBlocks
	if err := json.Unmarshal(out, &c); err != nil {
		return nil, nil, nil, fmt.Errorf("claudeaccount: parse ccusage blocks: %w", err)
	}
	for _, b := range c.Blocks {
		if !b.Active {
			continue
		}
		cap_ := b.Cap
		if cap_ == nil {
			cap_ = b.Limit
		}
		var resets *time.Time
		if t, err := parseResets(b.ResetsAt, b.ResetTime); err == nil && !t.IsZero() {
			tt := t
			resets = &tt
		}
		return b.Used, cap_, resets, nil
	}
	// No active block — ccusage ran cleanly but has no quota window
	// to report. Return zero values without an error.
	return nil, nil, nil, nil
}

// parseResets accepts either field name from ccusage and returns the
// first one that parses as RFC 3339. Empty strings produce a
// (zero-time, nil) pair so callers can treat absence as "unknown".
func parseResets(primary, fallback string) (time.Time, error) {
	for _, raw := range []string{primary, fallback} {
		if raw == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, nil
}
