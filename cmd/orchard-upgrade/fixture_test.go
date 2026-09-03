package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// fixture is a stand-in GitHub releases API, so no test here touches the
// network (RULES T1) and every one can assert what was requested.
type fixture struct {
	server   *httptest.Server
	releases map[string]*release.Release
	assets   map[string][]byte

	mu       sync.Mutex
	requests []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		releases: map[string]*release.Release{},
		assets:   map[string][]byte{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	t.Setenv(release.RepoEnv, f.server.URL)
	return f
}

func (f *fixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fixture) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()

	switch {
	case strings.HasSuffix(r.URL.Path, "/releases/latest"):
		f.writeRelease(w, "latest")
	case strings.Contains(r.URL.Path, "/releases/tags/"):
		f.writeRelease(w, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
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
	json.NewEncoder(w).Encode(rel)
}

// publish registers a release whose suite tarball holds every orchard binary,
// each stamped with the tag so a test can tell versions apart by content.
// corrupt breaks the SHA256SUMS entry, standing in for a tampered download.
func (f *fixture) publish(t *testing.T, tag string, latest, corrupt bool) {
	t.Helper()
	triple, err := release.HostTriple()
	if err != nil {
		t.Fatalf("HostTriple: %v", err)
	}
	files := map[string]string{}
	for _, name := range release.SuiteBinaries {
		files["bin/"+name] = name + "@" + tag
	}
	pkgName := release.AssetName(release.SuitePackage, triple)
	tarball := tarGz(t, files)

	sums := fmt.Sprintf("%s  %s\n", release.SHA256(tarball), pkgName)
	if corrupt {
		sums = fmt.Sprintf("%s  %s\n", release.SHA256([]byte("not the tarball")), pkgName)
	}

	// Assets are namespaced by tag, exactly as a real release's are: two
	// releases carry the same asset NAME with different bytes, and a fixture
	// that flattened them would let a pinned download silently serve the
	// latest release's tarball.
	f.assets[tag+"/"+pkgName] = tarball
	f.assets[tag+"/"+release.SumsAsset] = []byte(sums)
	rel := &release.Release{TagName: tag, Assets: []release.Asset{
		{Name: pkgName, DownloadURL: f.server.URL + "/assets/" + tag + "/" + pkgName},
		{Name: release.SumsAsset, DownloadURL: f.server.URL + "/assets/" + tag + "/" + release.SumsAsset},
	}}
	f.releases[tag] = rel
	if latest {
		f.releases["latest"] = rel
	}
}

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
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// installDirWith creates an install directory holding the named binaries.
func installDirWith(t *testing.T, contents string, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// snapshot records every file's digest and modification time, so a test can
// prove a run changed nothing at all.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		sum, err := release.SHA256File(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = sum + "@" + info.ModTime().String()
	}
	return out
}

func assertUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	after := snapshot(t, dir)
	if len(after) != len(before) {
		t.Fatalf("install dir now holds %d files; want the original %d", len(after), len(before))
	}
	for name, want := range before {
		if after[name] != want {
			t.Errorf("%s changed: %s -> %s", name, want, after[name])
		}
	}
}

// withVersion overrides the version this binary reports for one test.
func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}
