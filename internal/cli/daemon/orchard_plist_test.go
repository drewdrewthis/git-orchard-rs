// Structural + render tests for the macOS launchd unit (issue #749).
//
// launchd does NOT expand `~` in StandardOutPath / StandardErrorPath: it
// open(2)s the string verbatim. The shipped unit therefore cannot name the
// documented ~/.local/state/orchard location directly, which is how it drifted
// to /tmp/orchard.{out,err}.log while its own comment claimed otherwise — the
// daemon had no log where anyone looked for it.
//
// The unit ships as a template with an __ORCHARD_STATE_DIR__ placeholder that
// scripts/init/launchd-install.sh substitutes at install time. These tests
// parse the template as plain text and drive the installer with a scratch
// HOME, so they need neither launchd nor a real install.

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// stateDirPlaceholder is the token the installer substitutes. Duplicated here
// deliberately: the test is the contract between the template and the script,
// so it must fail if either side renames the token unilaterally.
const stateDirPlaceholder = "__ORCHARD_STATE_DIR__"

// repoRoot resolves the repository root from this test file's location so the
// relative-path math holds regardless of go test's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot determine test file location")
	}
	// thisFile is internal/cli/daemon/orchard_plist_test.go — root is three up.
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

func plistTemplatePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "scripts", "init", "com.gitorchard.orchard.plist.template")
}

func launchdInstallScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "scripts", "init", "launchd-install.sh")
}

func readPlistTemplate(t *testing.T) string {
	t.Helper()
	path := plistTemplatePath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read launchd template at %s: %v\nEnsure scripts/init/com.gitorchard.orchard.plist.template exists.", path, err)
	}
	return string(data)
}

// plistKeyValueRe matches a `<key>NAME</key>` immediately followed by a
// `<string>VALUE</string>`, tolerating whitespace and newlines between them.
// That covers every path directive the unit sets; the plist has no nested
// dict whose keys collide with the ones under test.
var plistKeyValueRe = regexp.MustCompile(`(?s)<key>\s*([^<]+?)\s*</key>\s*<string>\s*([^<]*?)\s*</string>`)

// parsePlistStrings returns the string-valued entries of a launchd plist,
// keyed by their <key>. Array and boolean values are ignored — the tests only
// assert on path strings.
func parsePlistStrings(content string) map[string]string {
	out := make(map[string]string)
	for _, m := range plistKeyValueRe.FindAllStringSubmatch(content, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// TestLaunchdTemplateLogPathsMatchDocumentedLocation asserts the template
// points StandardOutPath / StandardErrorPath at the state dir the comment
// block documents — the exact contradiction #749 reported.
func TestLaunchdTemplateLogPathsMatchDocumentedLocation(t *testing.T) {
	t.Parallel()
	values := parsePlistStrings(readPlistTemplate(t))

	for key, wantSuffix := range map[string]string{
		"StandardOutPath":   "orchard.out.log",
		"StandardErrorPath": "orchard.err.log",
	} {
		got, ok := values[key]
		if !ok {
			t.Errorf("launchd template does not set %s", key)
			continue
		}
		if !strings.HasPrefix(got, stateDirPlaceholder+"/") {
			t.Errorf("%s = %q, want it to start with %q so the installer can substitute an absolute path",
				key, got, stateDirPlaceholder+"/")
		}
		if !strings.HasSuffix(got, wantSuffix) {
			t.Errorf("%s = %q, want it to end with %q (the documented filename)", key, got, wantSuffix)
		}
		if strings.Contains(got, "/tmp/") {
			t.Errorf("%s = %q still points into /tmp; #749 moved the daemon logs under the state dir", key, got)
		}
	}
}

// TestLaunchdTemplateHasNoTildePaths asserts no string value relies on `~`.
// launchd passes these to open(2) verbatim — a tilde yields a literal
// "./~/..." relative path, not the user's home.
func TestLaunchdTemplateHasNoTildePaths(t *testing.T) {
	t.Parallel()
	for key, val := range parsePlistStrings(readPlistTemplate(t)) {
		if strings.Contains(val, "~") {
			t.Errorf("%s = %q contains `~`; launchd does not expand it — use %s and let the installer substitute",
				key, val, stateDirPlaceholder)
		}
	}
}

// TestLaunchdTemplateLaunchesTheInstalledDaemonBinary asserts ProgramArguments
// names the binary `make install-daemon` actually installs.
//
// The template previously launched `/usr/local/bin/orchard daemon start`. That
// dispatcher routes `daemon start` to a control script rather than running the
// daemon itself, so launchd had nothing to redirect and the log-path fix alone
// would still have produced empty files.
func TestLaunchdTemplateLaunchesTheInstalledDaemonBinary(t *testing.T) {
	t.Parallel()

	const wantProgram = "/usr/local/bin/orchard-daemon"
	content := readPlistTemplate(t)

	if !strings.Contains(content, "<string>"+wantProgram+"</string>") {
		t.Errorf("launchd template ProgramArguments does not name %s", wantProgram)
	}

	// Tie the plist to the install layout: if the Makefile stops installing
	// this path, this test says so rather than leaving a dangling unit.
	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("cannot read Makefile: %v", err)
	}
	if !strings.Contains(string(makefile), wantProgram) {
		t.Errorf("Makefile does not install %s, but the launchd template launches it", wantProgram)
	}
}

// TestLaunchdPlainPlistNoLongerShips guards the regression path: a leftover
// non-template com.gitorchard.orchard.plist would still be `cp`-able straight
// into ~/Library/LaunchAgents with whatever stale paths it carried.
func TestLaunchdPlainPlistNoLongerShips(t *testing.T) {
	t.Parallel()
	stale := filepath.Join(repoRoot(t), "scripts", "init", "com.gitorchard.orchard.plist")
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("%s still exists; the unit ships as .plist.template and is rendered by scripts/init/launchd-install.sh", stale)
	}
}

// runInstaller executes scripts/init/launchd-install.sh with a scratch HOME
// and returns its stdout. XDG_STATE_HOME is cleared so the script exercises
// the same $HOME/.local/state/orchard default orchpaths.StateDir() resolves.
func runInstaller(t *testing.T, home string, args ...string) string {
	t.Helper()
	cmd := exec.Command(launchdInstallScript(t), args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launchd-install.sh %v failed: %v\noutput:\n%s", args, err, out)
	}
	return string(out)
}

// TestLaunchdInstallerRendersAbsoluteLogPaths is the core #749 assertion: what
// the installer writes contains no placeholder and no `~`, and both log paths
// are absolute and rooted at the documented state directory.
func TestLaunchdInstallerRendersAbsoluteLogPaths(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	rendered := runInstaller(t, home, "--print")

	if strings.Contains(rendered, stateDirPlaceholder) {
		t.Fatalf("rendered plist still contains %s:\n%s", stateDirPlaceholder, rendered)
	}

	values := parsePlistStrings(rendered)
	wantDir := filepath.Join(home, ".local", "state", "orchard")
	for key, file := range map[string]string{
		"StandardOutPath":   "orchard.out.log",
		"StandardErrorPath": "orchard.err.log",
	} {
		got, ok := values[key]
		if !ok {
			t.Errorf("rendered plist does not set %s", key)
			continue
		}
		if !filepath.IsAbs(got) {
			t.Errorf("%s = %q is not an absolute path; launchd cannot open it", key, got)
		}
		if strings.Contains(got, "~") {
			t.Errorf("%s = %q contains `~`; launchd does not expand it", key, got)
		}
		if want := filepath.Join(wantDir, file); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// TestLaunchdInstallerCreatesStateDir asserts the installer makes the log
// directory. launchd opens StandardOutPath before exec'ing the program, so a
// missing parent directory means the redirect fails and the daemon starts with
// no log at all — silently.
func TestLaunchdInstallerCreatesStateDir(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dest := t.TempDir()
	runInstaller(t, home, "--dest", dest)

	stateDir := filepath.Join(home, ".local", "state", "orchard")
	info, err := os.Stat(stateDir)
	if err != nil {
		t.Fatalf("installer did not create the state dir %s: %v", stateDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s exists but is not a directory", stateDir)
	}
}

// TestLaunchdInstallerWritesPlistToDest asserts the installed file lands under
// the requested destination with the launchd Label as its basename, and that
// it is the rendered form rather than the raw template.
func TestLaunchdInstallerWritesPlistToDest(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dest := t.TempDir()
	runInstaller(t, home, "--dest", dest)

	installed := filepath.Join(dest, "com.gitorchard.orchard.plist")
	data, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("installer did not write %s: %v", installed, err)
	}
	if strings.Contains(string(data), stateDirPlaceholder) {
		t.Errorf("installed plist at %s still contains the %s placeholder", installed, stateDirPlaceholder)
	}
	if got := parsePlistStrings(string(data))["Label"]; got != "com.gitorchard.orchard" {
		t.Errorf("installed plist Label = %q, want %q", got, "com.gitorchard.orchard")
	}
}

// TestLaunchdInstallerHonoursXDGStateHome asserts the installer resolves the
// state dir the same way orchpaths.StateDir() does, so the plist's log paths
// and the daemon's pidfile never land in different directories.
func TestLaunchdInstallerHonoursXDGStateHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	xdg := t.TempDir()

	cmd := exec.Command(launchdInstallScript(t), "--print")
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+xdg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launchd-install.sh --print failed: %v\noutput:\n%s", err, out)
	}

	want := filepath.Join(xdg, "orchard", "orchard.out.log")
	if got := parsePlistStrings(string(out))["StandardOutPath"]; got != want {
		t.Errorf("StandardOutPath = %q, want %q (XDG_STATE_HOME must win, matching orchpaths.StateDir)", got, want)
	}
}
