package release

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"path"
)

// maxMemberBytes caps a single archive member. The largest orchard binary is
// tens of megabytes; anything past this is a decompression bomb, not a build.
const maxMemberBytes = 512 << 20

// MaxArchiveBytes caps the total decompressed bytes ExtractBinaries will read
// from a suite tarball across every member it walks — including ones it
// skips, whose bodies are still read off the stream to reach the next
// header. A var, not a const, so a test can lower it instead of building a
// multi-gigabyte fixture to prove the cap trips.
var MaxArchiveBytes int64 = 2 << 30 // 2 GiB

// ExtractBinaries reads a gzipped tar and returns the regular-file members
// whose base name is one of want, keyed by that base name.
//
// Members are matched on base name alone and the directory part is discarded,
// so the release job is free to ship them flat or under bin/ without this
// having to know which. Discarding the path also disposes of archive path
// traversal ("../../etc/x") outright: nothing here ever joins an
// archive-supplied path onto a filesystem path.
func ExtractBinaries(r io.Reader, want []string) (map[string][]byte, error) {
	wanted := make(map[string]bool, len(want))
	for _, w := range want {
		wanted[w] = true
	}

	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("open suite archive: %w", err)
	}
	defer gz.Close()

	// Every byte tar.Reader pulls from the decompressed stream — a wanted
	// member's body, or the padding it discards to reach the next header for
	// one nobody asked for — passes through this counter first. Without it, a
	// forged header on a *skipped* member is an unbounded decompression sink:
	// the per-member cap below never even sees that member's bytes.
	counted := &countingReader{r: gz, max: MaxArchiveBytes}

	out := map[string][]byte{}
	tr := tar.NewReader(counted)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read suite archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Base(hdr.Name)
		if !wanted[name] {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxMemberBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read %s from suite archive: %w", name, err)
		}
		if int64(len(data)) > maxMemberBytes {
			return nil, fmt.Errorf("%s exceeds the %d byte member limit", name, int64(maxMemberBytes))
		}
		out[name] = data
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("suite archive contains none of %v: %w", want, ErrNoAsset)
	}
	return out, nil
}

// countingReader wraps r and fails once more than max bytes have been read
// from it in total, regardless of how the caller above (tar.Reader) is using
// those bytes — returned to the caller, or discarded while skipping to the
// next header.
type countingReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.n >= c.max {
		return 0, fmt.Errorf("read past %d decompressed bytes from the suite archive: %w", c.max, ErrArchiveTooLarge)
	}
	if remaining := c.max - c.n; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
