package release_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

const testTriple = "aarch64-unknown-linux-gnu"

// suiteFixture registers a release whose suite tarball holds every orchard
// binary, plus a SHA256SUMS that pins it.
func suiteFixture(t *testing.T, tag string, latest bool, corrupt bool) *fixture {
	t.Helper()
	f := newFixture(t)
	files := map[string]string{}
	for _, name := range release.SuiteBinaries {
		files["bin/"+name] = name + "@" + tag
	}
	tarball := tarGz(t, files)

	pkgName := release.AssetName(release.SuitePackage, testTriple)
	assets := map[string][]byte{pkgName: tarball}
	sums := sumsFor(assets)
	if corrupt {
		// A digest that is well-formed but wrong — what a tampered or
		// truncated download looks like from the client's side.
		sums = []byte(release.SHA256([]byte("not the tarball")) + "  " + pkgName + "\n")
	}
	assets[release.SumsAsset] = sums
	f.addRelease(tag, latest, assets)
	return f
}

func TestFetchSuite_VerifiesAndExtractsEveryBinary(t *testing.T) {
	f := suiteFixture(t, "v1.5.0", true, false)
	f.use()

	suite, err := release.FetchSuite(context.Background(), release.NewClient(), "", testTriple)
	if err != nil {
		t.Fatalf("FetchSuite: %v", err)
	}
	if suite.Version != "1.5.0" {
		t.Errorf("Version = %q; want 1.5.0", suite.Version)
	}
	for _, name := range release.SuiteBinaries {
		body, ok := suite.Binaries[name]
		if !ok {
			t.Errorf("suite is missing %s", name)
			continue
		}
		if string(body) != name+"@v1.5.0" {
			t.Errorf("%s = %q; want the archived contents", name, body)
		}
	}
}

// @scenario Checksum mismatch aborts and leaves existing binaries untouched
//
// AC7: a checksum mismatch aborts before anything is extracted.
func TestFetchSuite_ChecksumMismatchAborts(t *testing.T) {
	f := suiteFixture(t, "v1.5.0", true, true)
	f.use()

	_, err := release.FetchSuite(context.Background(), release.NewClient(), "", testTriple)
	if err == nil {
		t.Fatal("FetchSuite accepted a tarball whose checksum does not match SHA256SUMS")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %v; want it to name the checksum mismatch", err)
	}
}

func TestFetchSuite_PinnedTagWins(t *testing.T) {
	f := suiteFixture(t, "v1.0.0", false, false)
	// A newer release the pin must be ignored in favour of.
	newer := tarGz(t, map[string]string{"orchard": "orchard@v2.0.0"})
	assets := map[string][]byte{release.AssetName(release.SuitePackage, testTriple): newer}
	assets[release.SumsAsset] = sumsFor(assets)
	f.addRelease("v2.0.0", true, assets)
	f.use()

	suite, err := release.FetchSuite(context.Background(), release.NewClient(), "v1.0.0", testTriple)
	if err != nil {
		t.Fatalf("FetchSuite: %v", err)
	}
	if suite.Tag != "v1.0.0" {
		t.Errorf("Tag = %q; want the pinned v1.0.0", suite.Tag)
	}
}

func TestFetchSuite_MissingTripleAssetIsErrNoAsset(t *testing.T) {
	f := suiteFixture(t, "v1.0.0", true, false)
	f.use()

	_, err := release.FetchSuite(context.Background(), release.NewClient(), "", "x86_64-apple-darwin")
	if !errors.Is(err, release.ErrNoAsset) {
		t.Fatalf("FetchSuite for an unbuilt platform = %v; want ErrNoAsset", err)
	}
}

// The dispatcher is renamed last so a crash mid-set leaves a working
// `orchard` pointing at helpers it can still run (plan §10).
func TestSuitePlan_OrdersHelpersFirstAndOnlyTouchesInstalledBinaries(t *testing.T) {
	dir := t.TempDir()
	installed := map[string]bool{}
	for _, n := range []string{"orchard", "orchard-shell"} {
		installed[filepath.Join(dir, n)] = true
		if err := os.WriteFile(filepath.Join(dir, n), []byte("v1"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	suite := &release.Suite{Binaries: map[string][]byte{
		"orchard":        []byte("a"),
		"orchard-shell":  []byte("b"),
		"orchard-daemon": []byte("c"), // not installed here — must be skipped
	}}

	plan := suite.Plan(dir, func(p string) bool { return installed[p] })

	var got []string
	for _, it := range plan {
		got = append(got, filepath.Base(it.Path))
	}
	want := []string{"orchard-shell", "orchard"}
	if len(got) != len(want) {
		t.Fatalf("plan = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("plan = %v; want %v (helpers first, dispatcher last)", got, want)
		}
	}
}

func TestExtractBinaries_IgnoresUnknownMembersAndPaths(t *testing.T) {
	tarball := tarGz(t, map[string]string{
		"orchard-suite/bin/orchard":     "dispatcher",
		"orchard-suite/README.md":       "docs",
		"../../../etc/orchard":          "traversal attempt",
		"orchard-suite/share/x/orchard": "later duplicate",
	})
	bins, err := release.ExtractBinaries(strings.NewReader(string(tarball)), []string{"orchard"})
	if err != nil {
		t.Fatalf("ExtractBinaries: %v", err)
	}
	if len(bins) != 1 {
		t.Fatalf("extracted %d members; want 1 (only the wanted base name)", len(bins))
	}
	if _, ok := bins["orchard"]; !ok {
		t.Errorf("orchard missing from %v", bins)
	}
}

func TestExtractBinaries_EmptyArchiveIsErrNoAsset(t *testing.T) {
	tarball := tarGz(t, map[string]string{"README.md": "nothing here"})
	_, err := release.ExtractBinaries(strings.NewReader(string(tarball)), release.SuiteBinaries)
	if !errors.Is(err, release.ErrNoAsset) {
		t.Fatalf("ExtractBinaries on a binary-free archive = %v; want ErrNoAsset", err)
	}
}
