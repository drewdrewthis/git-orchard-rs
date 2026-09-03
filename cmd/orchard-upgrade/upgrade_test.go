package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// @scenario upgrade --check reports without mutating anything
//
// AC7: --check exits 0, prints current and latest, and modifies no file.
func TestCheck_ReportsWithoutWriting(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v2.0.0", true, false)
	dir := installDirWith(t, "v1", "orchard", "orchard-shell")
	before := snapshot(t, dir)
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--check", "--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--check) = %d; want 0. stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"current: 1.0.0", "latest:  2.0.0", "an update is available"} {
		if !strings.Contains(out, want) {
			t.Errorf("--check output %q is missing %q", out, want)
		}
	}
	assertUnchanged(t, dir, before)
}

// AC: --check against the real release-please tag shape ("orchard-v1.1.0",
// manifest mode's component-prefixed tag — see release-please-config.json
// and internal/release.Release.Version) reports the bare semver, not the
// tag verbatim. Regression test for the doctor/upgrade bug where "latest"
// showed "orchard-v1.1.0" instead of "1.1.0".
func TestCheck_RealReleasePleaseTagReportsBareVersion(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "orchard-v1.1.0", true, false)
	dir := installDirWith(t, "v1", "orchard")
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--check", "--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--check) = %d; want 0. stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"current: 1.0.0", "latest:  1.1.0", "an update is available"} {
		if !strings.Contains(out, want) {
			t.Errorf("--check output %q is missing %q", out, want)
		}
	}
	if strings.Contains(out, "orchard-v1.1.0") {
		t.Errorf("--check output %q leaks the raw release tag instead of the stripped version", out)
	}
}

// AC: --check on a dev build hints that the comparison is not meaningful,
// since "dev" always sorts older than any real release (see release.Compare)
// and would otherwise read as a real, actionable update.
func TestCheck_DevBuildHintsComparisonIsNotMeaningful(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v2.0.0", true, false)
	dir := installDirWith(t, "v1", "orchard")
	withVersion(t, "dev")

	var stdout, stderr strings.Builder
	if code := run([]string{"--check", "--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--check) = %d; want 0. stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "current version is a dev build; comparison is not meaningful") {
		t.Errorf("--check output %q does not hint at the dev build", out)
	}
}

func TestCheck_UpToDateSaysSo(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v1.0.0", true, false)
	dir := installDirWith(t, "v1", "orchard")
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--check", "--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--check) = %d; want 0. stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "up to date") {
		t.Errorf("output %q does not report an up-to-date install", stdout.String())
	}
}

// AC7: vN -> vN+1 leaves every installed binary reporting the new version.
func TestUpgrade_ReplacesEveryInstalledBinary(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v2.0.0", true, false)
	dir := installDirWith(t, "old", "orchard", "orchard-shell", "orchard-daemon")
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0. stderr: %s", code, stderr.String())
	}
	for _, name := range []string{"orchard", "orchard-shell", "orchard-daemon"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != name+"@v2.0.0" {
			t.Errorf("%s = %q; want the v2.0.0 build", name, got)
		}
	}
	// One release lookup, one SHA256SUMS, one tarball. GitHub allows 60
	// anonymous requests an hour; a second lookup for the same release would
	// be half an upgrade's budget spent on nothing.
	if f.count() != 3 {
		t.Errorf("fixture saw %d requests; want 3 (release, SHA256SUMS, tarball)", f.count())
	}
}

// A successful install invalidates the update-check cache: doctor and the
// sidebar only ever read that file (internal/release.LoadCheckFor), and a
// Current left over from the binary this upgrade just replaced must not
// outlive it — the exact staleness reported against `orchard shell doctor`
// after an upgrade from a dev build.
func TestUpgrade_SuccessfulInstallInvalidatesTheUpdateCheckCache(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v2.0.0", true, false)
	dir := installDirWith(t, "v1", "orchard", "orchard-shell")
	withVersion(t, "1.0.0")
	cachePath := seedUpdateCheckCache(t, "dev", "2.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0. stderr: %s", code, stderr.String())
	}

	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("update-check cache at %s still exists after a successful upgrade", cachePath)
	}
}

// --check changes nothing per its own contract ("not the update-check
// cache" — see upgrade.go's check()); --dry-run's contract is the same for
// the install dir (assertUnchanged) and must hold here too.
func TestUpgrade_CheckAndDryRunLeaveTheUpdateCheckCacheAlone(t *testing.T) {
	for _, flag := range []string{"--check", "--dry-run"} {
		t.Run(flag, func(t *testing.T) {
			f := newFixture(t)
			f.publish(t, "v2.0.0", true, false)
			dir := installDirWith(t, "v1", "orchard")
			withVersion(t, "1.0.0")
			cachePath := seedUpdateCheckCache(t, "dev", "2.0.0")

			var stdout, stderr strings.Builder
			if code := run([]string{flag, "--prefix", dir}, &stdout, &stderr); code != 0 {
				t.Fatalf("run(%s) = %d; want 0. stderr: %s", flag, code, stderr.String())
			}

			got := release.LoadCheck(cachePath)
			if got.Current != "dev" || got.Latest != "2.0.0" {
				t.Errorf("%s mutated the update-check cache: %+v", flag, got)
			}
		})
	}
}

// @scenario upgrade is a no-op when every binary is already byte-identical
//
// AC: a real upgrade run where the download matches what's already installed
// must not claim to have installed anything — it reports every binary
// unchanged and prints a single up-to-date summary.
func TestUpgrade_ByteIdenticalBinariesReportAlreadyUpToDate(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v2.0.0", true, false)
	dir := installDirWithVersion(t, "v2.0.0", "orchard", "orchard-shell")
	before := snapshot(t, dir)
	// current is older than latest so the version-string short-circuit above
	// does not fire before the download; the no-op is discovered by content.
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0. stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "already up to date (2 unchanged)") {
		t.Errorf("output %q does not report the no-op upgrade", out)
	}
	for _, name := range []string{"orchard", "orchard-shell"} {
		if !strings.Contains(out, name+": unchanged") {
			t.Errorf("output %q does not report %s as unchanged", out, name)
		}
	}
	assertUnchanged(t, dir, before)
}

// A mixed batch — one binary already current, one genuinely stale — must
// report each by its own action, and must not claim to be fully up to date.
func TestUpgrade_MixedBatchReportsPerBinaryActions(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v2.0.0", true, false)
	dir := installDirWithVersion(t, "v2.0.0", "orchard")
	if err := os.WriteFile(filepath.Join(dir, "orchard-shell"), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0. stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "orchard: unchanged") {
		t.Errorf("output %q does not report orchard as unchanged", out)
	}
	if !strings.Contains(out, "orchard-shell: updated") {
		t.Errorf("output %q does not report orchard-shell as updated", out)
	}
	if strings.Contains(out, "already up to date") {
		t.Errorf("output %q claims fully up to date despite orchard-shell actually changing", out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "orchard-shell"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "orchard-shell@v2.0.0" {
		t.Errorf("orchard-shell = %q; want the v2.0.0 build", got)
	}
}

// An upgrade installs what is installed. A binary the user never had must not
// appear — that is the installer's job, not upgrade's.
func TestUpgrade_LeavesUninstalledBinariesAlone(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v2.0.0", true, false)
	dir := installDirWith(t, "old", "orchard")
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0. stderr: %s", code, stderr.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("install dir holds %v; want only the binary that was already there", names)
	}
}

// @scenario Checksum mismatch aborts and leaves existing binaries untouched
//
// AC7: a tampered download replaces nothing and exits non-zero.
func TestUpgrade_ChecksumMismatchInstallsNothing(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v2.0.0", true, true)
	dir := installDirWith(t, "v1", "orchard", "orchard-shell")
	before := snapshot(t, dir)
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--prefix", dir}, &stdout, &stderr); code == 0 {
		t.Fatal("run() = 0 despite a checksum mismatch")
	}
	if !strings.Contains(stderr.String(), "checksum mismatch") {
		t.Errorf("stderr %q does not name the checksum mismatch", stderr.String())
	}
	assertUnchanged(t, dir, before)
}

// @scenario upgrade refuses when the install directory is not writable
//
// AC7: a read-only install directory exits non-zero, names the directory, and
// leaves every binary at its original digest.
func TestUpgrade_ReadOnlyInstallDirIsRefusedByName(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not gate root")
	}
	f := newFixture(t)
	f.publish(t, "v2.0.0", true, false)
	dir := installDirWith(t, "v1", "orchard")
	before := snapshot(t, dir)
	withVersion(t, "1.0.0")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	var stdout, stderr strings.Builder
	if code := run([]string{"--prefix", dir}, &stdout, &stderr); code == 0 {
		t.Fatal("run() = 0 against a read-only install directory")
	}
	if !strings.Contains(stderr.String(), dir) {
		t.Errorf("stderr %q does not name the directory", stderr.String())
	}
	if f.count() != 0 {
		t.Errorf("fixture saw %d requests; the writability refusal must come before the download", f.count())
	}
	os.Chmod(dir, 0o700)
	assertUnchanged(t, dir, before)
}

// @scenario --version pins a specific release
//
// AC7: --version pins, including to an older release.
func TestUpgrade_PinnedVersionDowngrades(t *testing.T) {
	f := newFixture(t)
	// Real release-please tags carry the "orchard-" component prefix (see
	// release-please-config.json); a v-prefixed --version pin goes through
	// release.NormalizeTag before it reaches Client.ByTag, so the fixture
	// must be published under the same real shape the pin resolves to.
	f.publish(t, "orchard-v1.0.0", false, false)
	f.publish(t, "orchard-v3.0.0", true, false)
	dir := installDirWith(t, "v3", "orchard")
	withVersion(t, "3.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--version", "v1.0.0", "--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--version v1.0.0) = %d; want 0. stderr: %s", code, stderr.String())
	}
	got, _ := os.ReadFile(filepath.Join(dir, "orchard"))
	if string(got) != "orchard@orchard-v1.0.0" {
		t.Errorf("orchard = %q; want the pinned orchard-v1.0.0 build", got)
	}
}

// AC: --version accepts a bare semver or v-prefixed pin and resolves it
// against the real release-please tag shape via release.NormalizeTag.
// Before that fix, --version 1.1.0 (or v1.1.0) 404'd against a release
// actually tagged "orchard-v1.1.0", because Client.ByTag matches tags
// exactly and never saw the real tag.
func TestUpgrade_BareVersionPinResolvesAgainstTheRealTagShape(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "orchard-v1.1.0", true, false)
	dir := installDirWith(t, "old", "orchard")
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--version", "1.1.0", "--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--version 1.1.0) = %d; want 0. stderr: %s", code, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, "orchard"))
	if err != nil {
		t.Fatalf("read orchard: %v", err)
	}
	if string(got) != "orchard@orchard-v1.1.0" {
		t.Errorf("orchard = %q; want the pinned orchard-v1.1.0 build", got)
	}
}

func TestUpgrade_DryRunVerifiesAndReportsWithoutInstalling(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v2.0.0", true, false)
	dir := installDirWith(t, "v1", "orchard", "orchard-shell")
	before := snapshot(t, dir)
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--dry-run", "--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--dry-run) = %d; want 0. stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "would install orchard 2.0.0") {
		t.Errorf("--dry-run output %q does not say what it would install", out)
	}
	for _, name := range []string{"orchard-shell", "orchard"} {
		if !strings.Contains(out, name) {
			t.Errorf("--dry-run output %q does not list %s", out, name)
		}
	}
	assertUnchanged(t, dir, before)
	if f.count() == 0 {
		t.Error("--dry-run made no requests; it must still download and verify")
	}
}

// An install that is already current must not download the release again.
func TestUpgrade_AlreadyLatestSkipsTheDownload(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v1.0.0", true, false)
	dir := installDirWith(t, "v1", "orchard")
	before := snapshot(t, dir)
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--prefix", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0. stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already the latest") {
		t.Errorf("output %q does not report an already-current install", stdout.String())
	}
	if f.count() != 1 {
		t.Errorf("fixture saw %d requests; want only the latest-release lookup", f.count())
	}
	assertUnchanged(t, dir, before)
}

func TestUpgrade_EmptyInstallDirIsAnHonestError(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v2.0.0", true, false)
	dir := t.TempDir()
	withVersion(t, "1.0.0")

	var stdout, stderr strings.Builder
	if code := run([]string{"--prefix", dir}, &stdout, &stderr); code == 0 {
		t.Fatal("run() = 0 with no orchard binaries to replace")
	}
	if !strings.Contains(stderr.String(), dir) {
		t.Errorf("stderr %q does not name the directory it looked in", stderr.String())
	}
}

func TestRun_MissingPrefixDirectoryIsRejected(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := run([]string{"--check", "--prefix", filepath.Join(t.TempDir(), "nope")}, &stdout, &stderr); code == 0 {
		t.Fatal("run() = 0 with a --prefix that does not exist")
	}
}
