package release_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

func TestParseSums_ReadsCoreutilsFormat(t *testing.T) {
	body := release.SHA256([]byte("a")) + "  one.tar.gz\n" +
		release.SHA256([]byte("b")) + " *two.tar.gz\n" +
		"\n# a comment\n"

	sums, err := release.ParseSums(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseSums: %v", err)
	}
	if len(sums) != 2 {
		t.Fatalf("parsed %d entries; want 2 (blank lines and comments skipped)", len(sums))
	}
	if err := sums.Verify("two.tar.gz", []byte("b")); err != nil {
		t.Errorf("binary-mode '*' prefix was not stripped from the name: %v", err)
	}
}

func TestParseSums_RejectsMalformedInput(t *testing.T) {
	cases := map[string]string{
		"no separator": "deadbeef\n",
		"short digest": "abc  one.tar.gz\n",
		"non-hex":      strings.Repeat("z", 64) + "  one.tar.gz\n",
		"no name":      release.SHA256(nil) + "  \n",
		"empty file":   "",
	}
	for name, body := range cases {
		if _, err := release.ParseSums(strings.NewReader(body)); err == nil {
			t.Errorf("ParseSums(%s) succeeded; want an error", name)
		}
	}
}

func TestSumsVerify_MismatchNamesBothDigests(t *testing.T) {
	sums, err := release.ParseSums(strings.NewReader(release.SHA256([]byte("good")) + "  pkg.tar.gz\n"))
	if err != nil {
		t.Fatalf("ParseSums: %v", err)
	}
	if err := sums.Verify("pkg.tar.gz", []byte("good")); err != nil {
		t.Fatalf("matching content rejected: %v", err)
	}
	err = sums.Verify("pkg.tar.gz", []byte("tampered"))
	if err == nil {
		t.Fatal("tampered content accepted")
	}
	if !strings.Contains(err.Error(), release.SHA256([]byte("tampered"))) {
		t.Errorf("mismatch error %q does not report the digest actually seen", err)
	}
}

// An asset SHA256SUMS does not list is exactly what a tampered release looks
// like, so an unlisted name must fail rather than pass unverified.
func TestSumsVerify_UnlistedAssetFails(t *testing.T) {
	sums, _ := release.ParseSums(strings.NewReader(release.SHA256([]byte("x")) + "  known.tar.gz\n"))
	if err := sums.Verify("surprise.tar.gz", []byte("x")); err == nil {
		t.Fatal("an asset missing from SHA256SUMS verified successfully")
	}
}

func TestSHA256File_MatchesSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := release.SHA256File(path)
	if err != nil {
		t.Fatalf("SHA256File: %v", err)
	}
	if want := release.SHA256([]byte("contents")); got != want {
		t.Errorf("SHA256File = %s; want %s", got, want)
	}
}
