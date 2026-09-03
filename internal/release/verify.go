package release

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// Sums is a parsed SHA256SUMS file: asset name → lowercase hex digest.
type Sums map[string]string

// ParseSums reads the `sha256sum` output format: a hex digest, whitespace,
// then the file name. GNU coreutils marks binary mode with a leading "*" on
// the name, which is stripped — the same file listed with and without it must
// resolve to one key, or a verify would fail on a formatting detail.
func ParseSums(r io.Reader) (Sums, error) {
	sums := Sums{}
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		digest, name, ok := strings.Cut(text, " ")
		if !ok {
			return nil, fmt.Errorf("SHA256SUMS line %d: no separator in %q", line, text)
		}
		if len(digest) != sha256.Size*2 {
			return nil, fmt.Errorf("SHA256SUMS line %d: %q is not a sha256 digest", line, digest)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return nil, fmt.Errorf("SHA256SUMS line %d: %q is not hex: %w", line, digest, err)
		}
		name = strings.TrimPrefix(strings.TrimSpace(name), "*")
		if name == "" {
			return nil, fmt.Errorf("SHA256SUMS line %d: no file name", line)
		}
		sums[name] = strings.ToLower(digest)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read SHA256SUMS: %w", err)
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("SHA256SUMS is empty")
	}
	return sums, nil
}

// Verify checks data against the digest recorded for name. A name the file
// does not list is an error, not a pass: an unlisted asset is exactly what a
// tampered release looks like.
func (s Sums) Verify(name string, data []byte) error {
	want, ok := s[name]
	if !ok {
		return fmt.Errorf("%s is not listed in %s", name, SumsAsset)
	}
	got := SHA256(data)
	if got != want {
		return fmt.Errorf("%s checksum mismatch: want %s, got %s", name, want, got)
	}
	return nil
}

// SHA256 returns the lowercase hex digest of data.
func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SHA256File returns the lowercase hex digest of a file's contents.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
