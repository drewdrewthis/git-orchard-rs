package release_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/release"
)

func TestCheckPath_LivesUnderTheStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/state-fixture")
	got, err := release.CheckPath()
	if err != nil {
		t.Fatalf("CheckPath: %v", err)
	}
	if want := "/tmp/state-fixture/orchard/" + release.CheckFile; got != want {
		t.Errorf("CheckPath() = %q; want %q", got, want)
	}
}

func TestLoadCheck_MissingOrCorruptFileIsAZeroCheck(t *testing.T) {
	dir := t.TempDir()
	if got := release.LoadCheck(filepath.Join(dir, "absent.json")); got != (release.Check{}) {
		t.Errorf("LoadCheck(missing) = %+v; want the zero Check", got)
	}
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := release.LoadCheck(corrupt); got != (release.Check{}) {
		t.Errorf("LoadCheck(corrupt) = %+v; want the zero Check", got)
	}
}

func TestCheck_UpdateAvailableComparesLatestAgainstCurrent(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"1.2.3", "1.3.0", true},
		{"1.2.3", "1.2.3", false},
		{"1.3.0", "1.2.3", false},
		{release.DevVersion, "0.0.1", true},
		{"1.0.0", "", false},
	}
	for _, c := range cases {
		got := release.Check{Current: c.current, Latest: c.latest}.UpdateAvailable()
		if got != c.want {
			t.Errorf("Check{current:%q latest:%q}.UpdateAvailable() = %v; want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestRefreshCheck_WritesTheCacheOnFirstRun(t *testing.T) {
	f := newFixture(t)
	f.use()
	f.addRelease("v2.1.0", true, nil)
	path := filepath.Join(t.TempDir(), release.CheckFile)
	now := time.Now()

	got := release.RefreshCheck(context.Background(), path, "1.0.0", now, release.CheckTTL)

	if got.Latest != "2.1.0" {
		t.Errorf("Latest = %q; want 2.1.0", got.Latest)
	}
	if !got.UpdateAvailable() {
		t.Error("UpdateAvailable() = false; 2.1.0 is newer than 1.0.0")
	}
	if reread := release.LoadCheck(path); reread.Latest != "2.1.0" {
		t.Errorf("re-read from disk = %+v; want the refreshed check to be persisted", reread)
	}
}

// @scenario The update-check cache is refreshed at most once per 24 hours
func TestRefreshCheck_FreshCacheSkipsTheNetwork(t *testing.T) {
	f := newFixture(t)
	f.use()
	f.addRelease("v2.1.0", true, nil)
	path := filepath.Join(t.TempDir(), release.CheckFile)
	now := time.Now()
	if err := release.SaveCheck(path, release.Check{CheckedAt: now.Add(-time.Hour), Current: "1.0.0", Latest: "1.0.1"}); err != nil {
		t.Fatal(err)
	}

	got := release.RefreshCheck(context.Background(), path, "1.0.0", now, release.CheckTTL)

	if f.count() != 0 {
		t.Errorf("fixture saw %d requests; a check made an hour ago must not hit the API again", f.count())
	}
	if got.Latest != "1.0.1" {
		t.Errorf("Latest = %q; want the cached 1.0.1", got.Latest)
	}
}

func TestRefreshCheck_StaleCacheRefreshes(t *testing.T) {
	f := newFixture(t)
	f.use()
	f.addRelease("v3.0.0", true, nil)
	path := filepath.Join(t.TempDir(), release.CheckFile)
	now := time.Now()
	if err := release.SaveCheck(path, release.Check{CheckedAt: now.Add(-25 * time.Hour), Current: "1.0.0", Latest: "1.0.1"}); err != nil {
		t.Fatal(err)
	}

	got := release.RefreshCheck(context.Background(), path, "1.0.0", now, release.CheckTTL)

	if f.count() != 1 {
		t.Errorf("fixture saw %d requests; a 25-hour-old check must refresh", f.count())
	}
	if got.Latest != "3.0.0" {
		t.Errorf("Latest = %q; want 3.0.0", got.Latest)
	}
}

// @scenario ORCHARD_NO_UPDATE_CHECK disables the background refresh
//
// AC9: ORCHARD_NO_UPDATE_CHECK=1 means the file is never written or refreshed.
func TestRefreshCheck_NoUpdateCheckEnvSkipsEverything(t *testing.T) {
	f := newFixture(t)
	f.use()
	f.addRelease("v3.0.0", true, nil)
	t.Setenv(release.NoCheckEnv, "1")
	path := filepath.Join(t.TempDir(), release.CheckFile)

	got := release.RefreshCheck(context.Background(), path, "1.0.0", time.Now(), release.CheckTTL)

	if f.count() != 0 {
		t.Errorf("fixture saw %d requests; the check is disabled", f.count())
	}
	if got != (release.Check{}) {
		t.Errorf("returned %+v; want the zero Check", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("update-check.json was written despite %s being set", release.NoCheckEnv)
	}
}

// A failed check must be invisible: the cached value comes back, no error,
// nothing written — the sidebar renders normally with no banner (AC9).
func TestRefreshCheck_APIFailureKeepsTheCachedValue(t *testing.T) {
	f := newFixture(t)
	f.use()
	path := filepath.Join(t.TempDir(), release.CheckFile)
	now := time.Now()
	if err := release.SaveCheck(path, release.Check{CheckedAt: now.Add(-48 * time.Hour), Current: "1.0.0", Latest: "1.0.1"}); err != nil {
		t.Fatal(err)
	}
	// No release registered at all: /releases/latest 404s.

	got := release.RefreshCheck(context.Background(), path, "1.0.0", now, release.CheckTTL)

	if got.Latest != "1.0.1" {
		t.Errorf("Latest = %q; want the stale-but-known 1.0.1 after a failed refresh", got.Latest)
	}
	if reread := release.LoadCheck(path); reread.Latest != "1.0.1" {
		t.Errorf("on-disk check = %+v; a failed refresh must not overwrite it", reread)
	}
}

// A version bump since the last check invalidates it even inside the TTL:
// after an upgrade the cached "update available" would otherwise persist for
// a day against the version the user just left behind.
func TestRefreshCheck_CurrentVersionChangeInvalidatesAFreshCache(t *testing.T) {
	f := newFixture(t)
	f.use()
	f.addRelease("v3.0.0", true, nil)
	path := filepath.Join(t.TempDir(), release.CheckFile)
	now := time.Now()
	if err := release.SaveCheck(path, release.Check{CheckedAt: now, Current: "1.0.0", Latest: "3.0.0"}); err != nil {
		t.Fatal(err)
	}

	got := release.RefreshCheck(context.Background(), path, "3.0.0", now, release.CheckTTL)

	if f.count() != 1 {
		t.Errorf("fixture saw %d requests; a changed current version must re-check", f.count())
	}
	if got.Current != "3.0.0" || got.UpdateAvailable() {
		t.Errorf("check = %+v; want current 3.0.0 with no update available", got)
	}
}
