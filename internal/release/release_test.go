package release_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

func TestResolveTarget_DefaultsToGitHubAndOrchardist(t *testing.T) {
	t.Setenv(release.RepoEnv, "")
	c := release.NewClient()
	if c.API != release.DefaultAPI || c.Repo != release.DefaultRepo {
		t.Fatalf("NewClient() = api %q repo %q; want %q / %q", c.API, c.Repo, release.DefaultAPI, release.DefaultRepo)
	}
}

func TestResolveTarget_SlugOverridesRepoOnly(t *testing.T) {
	t.Setenv(release.RepoEnv, "someone/fork")
	c := release.NewClient()
	if c.API != release.DefaultAPI {
		t.Errorf("API = %q; want %q — a slug must not move the API root", c.API, release.DefaultAPI)
	}
	if c.Repo != "someone/fork" {
		t.Errorf("Repo = %q; want someone/fork", c.Repo)
	}
}

func TestResolveTarget_URLOverridesAPIRoot(t *testing.T) {
	t.Setenv(release.RepoEnv, "http://127.0.0.1:1/fixture/")
	c := release.NewClient()
	if c.API != "http://127.0.0.1:1/fixture" {
		t.Errorf("API = %q; want the fixture root with its trailing slash trimmed", c.API)
	}
}

func TestLatest_ReadsTagAndAssetsFromFixture(t *testing.T) {
	f := newFixture(t)
	f.use()
	f.addRelease("v1.4.0", true, map[string][]byte{"orchard-suite-aarch64-unknown-linux-gnu.tar.gz": []byte("payload")})

	rel, err := release.NewClient().Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.TagName != "v1.4.0" {
		t.Errorf("TagName = %q; want v1.4.0", rel.TagName)
	}
	if rel.Version() != "1.4.0" {
		t.Errorf("Version() = %q; want 1.4.0 (tag without the v)", rel.Version())
	}
	if _, ok := rel.Asset("orchard-suite-aarch64-unknown-linux-gnu.tar.gz"); !ok {
		t.Errorf("asset missing from %+v", rel.Assets)
	}
}

// AC7: ORCHARD_RELEASE_REPO is honoured — the real GitHub API is not
// contacted, assertable by counting requests at the fixture.
func TestLatest_OnlyTalksToTheConfiguredFixture(t *testing.T) {
	f := newFixture(t)
	f.use()
	f.addRelease("v2.0.0", true, nil)

	if _, err := release.NewClient().Latest(context.Background()); err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got := f.count(); got != 1 {
		t.Fatalf("fixture saw %d requests; want exactly 1", got)
	}
	if paths := f.paths(); paths[0] != "/repos/"+release.DefaultRepo+"/releases/latest" {
		t.Errorf("requested %q; want the latest-release endpoint", paths[0])
	}
}

func TestByTag_PinsAnExactRelease(t *testing.T) {
	f := newFixture(t)
	f.use()
	f.addRelease("v1.0.0", false, nil)
	f.addRelease("v3.0.0", true, nil)

	rel, err := release.NewClient().ByTag(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatalf("ByTag: %v", err)
	}
	if rel.TagName != "v1.0.0" {
		t.Errorf("TagName = %q; want the pinned v1.0.0, not the latest", rel.TagName)
	}
}

func TestByTag_UnknownTagIsErrNotFound(t *testing.T) {
	f := newFixture(t)
	f.use()
	f.addRelease("v1.0.0", true, nil)

	_, err := release.NewClient().ByTag(context.Background(), "v9.9.9")
	if !errors.Is(err, release.ErrNotFound) {
		t.Fatalf("ByTag(v9.9.9) error = %v; want ErrNotFound", err)
	}
}

func TestLatest_RateLimitIsTyped(t *testing.T) {
	f := newFixture(t)
	f.use()
	f.addRelease("v1.0.0", true, nil)
	f.failOnce("/repos/"+release.DefaultRepo+"/releases/latest", http.StatusForbidden)

	_, err := release.NewClient().Latest(context.Background())
	if !errors.Is(err, release.ErrRateLimited) {
		t.Fatalf("Latest error = %v; want ErrRateLimited", err)
	}
}

func TestDownload_StreamsAssetBytes(t *testing.T) {
	f := newFixture(t)
	f.use()
	rel := f.addRelease("v1.0.0", true, map[string][]byte{"blob.tar.gz": []byte("hello-orchard")})

	asset, _ := rel.Asset("blob.tar.gz")
	var buf bytes.Buffer
	n, err := release.NewClient().Download(context.Background(), asset.DownloadURL, &buf)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := buf.String(); got != "hello-orchard" {
		t.Errorf("downloaded %q; want hello-orchard", got)
	}
	if n != int64(len("hello-orchard")) {
		t.Errorf("Download reported %d bytes; want %d", n, len("hello-orchard"))
	}
}

// @scenario Linux arm64 is a supported install target
func TestTriple_CoversEveryReleaseTarget(t *testing.T) {
	want := map[[2]string]string{
		{"darwin", "amd64"}: "x86_64-apple-darwin",
		{"darwin", "arm64"}: "aarch64-apple-darwin",
		{"linux", "amd64"}:  "x86_64-unknown-linux-gnu",
		{"linux", "arm64"}:  "aarch64-unknown-linux-gnu",
	}
	for k, expect := range want {
		got, err := release.Triple(k[0], k[1])
		if err != nil {
			t.Errorf("Triple(%s,%s): %v", k[0], k[1], err)
			continue
		}
		if got != expect {
			t.Errorf("Triple(%s,%s) = %q; want %q", k[0], k[1], got, expect)
		}
	}
	if _, err := release.Triple("plan9", "mips"); err == nil {
		t.Error("Triple(plan9,mips) succeeded; want an error naming the unsupported platform")
	}
}

func TestSuiteAsset_NamesTheSuiteTarballAndSums(t *testing.T) {
	f := newFixture(t)
	assets := map[string][]byte{
		"orchard-suite-x86_64-unknown-linux-gnu.tar.gz": []byte("suite"),
		release.SumsAsset: []byte("sums"),
	}
	rel := f.addRelease("v1.0.0", true, assets)

	pkg, sums, err := rel.SuiteAsset("x86_64-unknown-linux-gnu")
	if err != nil {
		t.Fatalf("SuiteAsset: %v", err)
	}
	if pkg.Name != "orchard-suite-x86_64-unknown-linux-gnu.tar.gz" {
		t.Errorf("pkg = %q", pkg.Name)
	}
	if sums.Name != release.SumsAsset {
		t.Errorf("sums = %q", sums.Name)
	}

	if _, _, err := rel.SuiteAsset("aarch64-apple-darwin"); !errors.Is(err, release.ErrNoAsset) {
		t.Errorf("SuiteAsset for a missing triple = %v; want ErrNoAsset", err)
	}
}
