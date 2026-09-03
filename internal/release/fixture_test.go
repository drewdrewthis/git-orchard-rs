package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// fixture is a stand-in GitHub releases API: it serves /repos/<repo>/releases
// endpoints and the asset bytes, and counts every request so a test can prove
// the real api.github.com was never contacted (AC7).
type fixture struct {
	t        *testing.T
	server   *httptest.Server
	releases map[string]*release.Release // tag → release ("latest" is an alias key)
	assets   map[string][]byte           // asset name → bytes

	mu       sync.Mutex
	requests []string
	failNext map[string]int // path suffix → status to return once
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:        t,
		releases: map[string]*release.Release{},
		assets:   map[string][]byte{},
		failNext: map[string]int{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// use points ORCHARD_RELEASE_REPO at this fixture for the duration of the test.
func (f *fixture) use() { f.t.Setenv(release.RepoEnv, f.server.URL) }

func (f *fixture) URL() string { return f.server.URL }

// failOnce makes the next request for path return status, once.
func (f *fixture) failOnce(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[path] = status
}

func (f *fixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fixture) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.requests...)
	sort.Strings(out)
	return out
}

// addRelease registers a release whose assets are served from this fixture.
// latest marks it as the one /releases/latest returns.
func (f *fixture) addRelease(tag string, latest bool, assets map[string][]byte) *release.Release {
	rel := &release.Release{TagName: tag}
	names := make([]string, 0, len(assets))
	for name := range assets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f.assets[name] = assets[name]
		rel.Assets = append(rel.Assets, release.Asset{
			Name:        name,
			DownloadURL: f.server.URL + "/assets/" + name,
			Size:        int64(len(assets[name])),
		})
	}
	f.releases[tag] = rel
	if latest {
		f.releases["latest"] = rel
	}
	return rel
}

func (f *fixture) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	if status, ok := f.failNext[r.URL.Path]; ok {
		delete(f.failNext, r.URL.Path)
		f.mu.Unlock()
		if status == http.StatusForbidden {
			w.Header().Set("X-RateLimit-Remaining", "0")
		}
		w.WriteHeader(status)
		return
	}
	f.mu.Unlock()

	switch {
	case strings.HasSuffix(r.URL.Path, "/releases/latest"):
		f.writeRelease(w, "latest")
	case strings.Contains(r.URL.Path, "/releases/tags/"):
		idx := strings.LastIndex(r.URL.Path, "/")
		f.writeRelease(w, r.URL.Path[idx+1:])
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		data, ok := f.assets[strings.TrimPrefix(r.URL.Path, "/assets/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(data)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fixture) writeRelease(w http.ResponseWriter, key string) {
	rel, ok := f.releases[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rel)
}

// tarGz builds a gzipped tar holding files (name → contents), mimicking the
// release job's orchard-suite tarball.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// sumsFor renders a SHA256SUMS body listing every named asset.
func sumsFor(assets map[string][]byte, skip ...string) []byte {
	skipped := map[string]bool{}
	for _, s := range skip {
		skipped[s] = true
	}
	names := make([]string, 0, len(assets))
	for n := range assets {
		if n == release.SumsAsset || skipped[n] {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", release.SHA256(assets[n]), n)
	}
	return []byte(b.String())
}
