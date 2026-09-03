package release

import (
	"fmt"
	"runtime"
)

// SuitePackage is the release asset holding every orchard binary. It is
// deliberately NOT "orchard-<triple>.tar.gz": npm/install.js hardcodes that
// name for the dispatcher alone, and reusing it would break `npm i -g
// git-orchard` silently (plan §10, one-way door).
const SuitePackage = "orchard-suite"

// SumsAsset is the aggregate checksum file every release carries.
const SumsAsset = "SHA256SUMS"

// SuiteBinaries are the binaries an install directory may hold, in the order
// `orchard upgrade` replaces them: helpers first, the dispatcher last, so a
// crash mid-upgrade still leaves a working `orchard` pointing at binaries it
// can run (plan §10, version skew).
var SuiteBinaries = []string{
	"orchard-daemon",
	"orchard-sidebar",
	"orchard-shell",
	"orchard-upgrade",
	"orchard-tui",
	"orchard",
}

// triples maps Go's GOOS/GOARCH to the rust target triples the release job
// names its assets with. The rust spelling is used for every asset, Go and
// Rust alike, so one release reads the same way top to bottom.
var triples = map[string]string{
	"darwin/amd64": "x86_64-apple-darwin",
	"darwin/arm64": "aarch64-apple-darwin",
	"linux/amd64":  "x86_64-unknown-linux-gnu",
	"linux/arm64":  "aarch64-unknown-linux-gnu",
}

// Triple returns the rust target triple for a GOOS/GOARCH pair.
func Triple(goos, goarch string) (string, error) {
	t, ok := triples[goos+"/"+goarch]
	if !ok {
		return "", fmt.Errorf("no orchard release target for %s/%s", goos, goarch)
	}
	return t, nil
}

// HostTriple returns the triple for the running binary's own platform.
func HostTriple() (string, error) { return Triple(runtime.GOOS, runtime.GOARCH) }

// AssetName is the release asset naming contract: <pkg>-<triple>.tar.gz.
func AssetName(pkg, triple string) string {
	return fmt.Sprintf("%s-%s.tar.gz", pkg, triple)
}

// SuiteAsset returns the suite tarball for a triple, and the SHA256SUMS asset
// that pins it. Both must be present for a verified download; a release
// missing either is not installable.
func (r *Release) SuiteAsset(triple string) (pkg, sums Asset, err error) {
	name := AssetName(SuitePackage, triple)
	pkg, ok := r.Asset(name)
	if !ok {
		return Asset{}, Asset{}, fmt.Errorf("%s has no %s: %w", r.TagName, name, ErrNoAsset)
	}
	sums, ok = r.Asset(SumsAsset)
	if !ok {
		return Asset{}, Asset{}, fmt.Errorf("%s has no %s: %w", r.TagName, SumsAsset, ErrNoAsset)
	}
	return pkg, sums, nil
}
