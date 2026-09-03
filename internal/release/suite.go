package release

import (
	"bytes"
	"context"
	"fmt"
)

// Suite is a downloaded, checksum-verified set of orchard binaries.
type Suite struct {
	Tag      string            // the release tag, e.g. "v1.2.3"
	Version  string            // the tag without its leading "v"
	Asset    string            // the tarball asset the binaries came out of
	Binaries map[string][]byte // base name → contents
}

// Resolve returns the release to install: the one tagged tag, or the latest
// published release when tag is empty.
func Resolve(ctx context.Context, c *Client, tag string) (*Release, error) {
	if tag == "" {
		return c.Latest(ctx)
	}
	return c.ByTag(ctx, tag)
}

// FetchSuite resolves a release and downloads its suite for one platform.
func FetchSuite(ctx context.Context, c *Client, tag, triple string) (*Suite, error) {
	rel, err := Resolve(ctx, c, tag)
	if err != nil {
		return nil, err
	}
	return FetchSuiteFromRelease(ctx, c, rel, triple)
}

// FetchSuiteFromRelease downloads and verifies an already-resolved release's
// suite tarball, then extracts the binaries from it. Callers that have
// already looked the release up use this rather than FetchSuite, so one
// upgrade costs one release lookup against GitHub's 60/hour anonymous limit.
//
// The checksum is verified BEFORE anything is extracted, and nothing is
// written to the install directory here at all — the caller installs from
// memory. A tampered or truncated download therefore fails with every
// installed binary still untouched (AC7).
func FetchSuiteFromRelease(ctx context.Context, c *Client, rel *Release, triple string) (*Suite, error) {
	pkg, sumsAsset, err := rel.SuiteAsset(triple)
	if err != nil {
		return nil, err
	}

	rawSums, err := c.Fetch(ctx, sumsAsset.DownloadURL)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", SumsAsset, err)
	}
	sums, err := ParseSums(bytes.NewReader(rawSums))
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if _, err := c.Download(ctx, pkg.DownloadURL, &buf); err != nil {
		return nil, fmt.Errorf("download %s: %w", pkg.Name, err)
	}
	if err := sums.Verify(pkg.Name, buf.Bytes()); err != nil {
		return nil, err
	}

	bins, err := ExtractBinaries(&buf, SuiteBinaries)
	if err != nil {
		return nil, err
	}
	return &Suite{
		Tag:      rel.TagName,
		Version:  rel.Version(),
		Asset:    pkg.Name,
		Binaries: bins,
	}, nil
}

// Plan pairs each binary present in dir with its replacement, in
// SuiteBinaries order — helpers first, the dispatcher last.
//
// Only binaries that ALREADY exist in dir are replaced. An upgrade installs
// what is installed; adding binaries a user never had is `install.sh`'s job,
// not upgrade's.
func (s *Suite) Plan(dir string, present func(path string) bool) []ReplaceItem {
	var items []ReplaceItem
	for _, name := range SuiteBinaries {
		data, ok := s.Binaries[name]
		if !ok {
			continue
		}
		path := dir + "/" + name
		if !present(path) {
			continue
		}
		items = append(items, ReplaceItem{Path: path, Data: data})
	}
	return items
}
