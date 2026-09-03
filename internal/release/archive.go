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

	out := map[string][]byte{}
	tr := tar.NewReader(gz)
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
