package release

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/drewdrewthis/orchardist/internal/orchpaths"
)

const (
	// NoCheckEnv disables the background update check entirely.
	NoCheckEnv = "ORCHARD_NO_UPDATE_CHECK"

	// CheckTTL is how long a recorded check stays fresh. GitHub's
	// unauthenticated limit is 60 requests/hour/IP; once a day per machine
	// keeps orchard a rounding error against it.
	CheckTTL = 24 * time.Hour

	// CheckFile is the cache's name under the orchard state directory.
	CheckFile = "update-check.json"
)

// Check is the cached result of one update check. `orchard shell` writes it;
// the sidebar only ever reads it, which is what keeps the sidebar off the
// network (plan step 10).
type Check struct {
	CheckedAt time.Time `json:"checked_at"`
	Current   string    `json:"current"`
	Latest    string    `json:"latest"`
}

// UpdateAvailable reports whether the cached check says a newer version
// exists.
func (c Check) UpdateAvailable() bool { return IsNewer(c.Latest, c.Current) }

// Fresh reports whether the check was made within ttl of now.
func (c Check) Fresh(now time.Time, ttl time.Duration) bool {
	return !c.CheckedAt.IsZero() && now.Sub(c.CheckedAt) < ttl
}

// CheckPath returns the update-check cache path.
func CheckPath() (string, error) {
	dir, err := orchpaths.StateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	return filepath.Join(dir, CheckFile), nil
}

// LoadCheck reads the cache. A missing or unreadable file is a zero Check and
// no error: a stale-or-absent cache is a normal state, not a failure worth
// reporting to a user who did not ask about updates.
func LoadCheck(path string) Check {
	data, err := os.ReadFile(path)
	if err != nil {
		return Check{}
	}
	var c Check
	if err := json.Unmarshal(data, &c); err != nil {
		return Check{}
	}
	return c
}

// LoadCheckFor reads the cache and returns it only if it was written against
// the exact version now running. A cache whose Current does not match —
// written by a build that has since been replaced, e.g. right after `orchard
// upgrade` — is exactly as stale as a missing file: a read-only caller (like
// `orchard shell doctor`, which never triggers a network refresh itself)
// must never surface a Current that does not match the binary actually
// running, so a mismatch resolves to the zero Check, same as LoadCheck's own
// missing-file case.
func LoadCheckFor(path, current string) Check {
	c := LoadCheck(path)
	if c.Current != current {
		return Check{}
	}
	return c
}

// SaveCheck writes the cache, creating the state directory if needed.
func SaveCheck(path string, c Check) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode update check: %w", err)
	}
	return Replace(path, append(data, '\n'), 0o644)
}

// InvalidateCheck removes the update-check cache file, if any. `orchard
// upgrade` calls this after a successful install so the check a pre-upgrade
// binary wrote is never read back against the new one — doctor and the
// sidebar only ever read this file (see LoadCheckFor, cmd/orchard-shell's
// doctor.go). A missing file, or any other removal error, is silently
// ignored: this is best-effort cleanup, not something that should fail an
// otherwise successful upgrade.
func InvalidateCheck(path string) {
	_ = os.Remove(path)
}

// RefreshCheck refreshes the cache when it is older than ttl, and returns the
// check that is now on disk.
//
// Every failure path returns the cached (possibly zero) value with no error:
// this runs in a background goroutine behind a terminal UI, where a failed
// update check must be invisible, never a banner (plan §10, rate limits).
func RefreshCheck(ctx context.Context, path, current string, now time.Time, ttl time.Duration) Check {
	cached := LoadCheck(path)
	if os.Getenv(NoCheckEnv) != "" {
		return cached
	}
	if cached.Fresh(now, ttl) && cached.Current == current {
		return cached
	}
	rel, err := NewClient().Latest(ctx)
	if err != nil {
		return cached
	}
	fresh := Check{CheckedAt: now, Current: current, Latest: rel.Version()}
	if err := SaveCheck(path, fresh); err != nil {
		return cached
	}
	return fresh
}
