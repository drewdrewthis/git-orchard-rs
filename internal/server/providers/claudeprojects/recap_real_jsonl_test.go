package claudeprojects

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadLatestRecap_AgainstRealJSONL drives the fold against real
// session jsonls known to contain /recap invocations. Skipped when the
// fixture files are not present (i.e. in CI). This is the real-runtime
// drive that proves the production path works against actual Claude
// Code output, not just synthetic fixtures.
func TestReadLatestRecap_AgainstRealJSONL(t *testing.T) {
	fixtures := []string{
		// 1 genuine /recap user invocation followed by a local_command
		// system record carrying the recap text.
		"/Users/hope/.claude/projects/-Users-hope--config-orchard/920a0b2f-e86f-4668-a103-59f3672bd1c2.jsonl",
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			info, err := os.Stat(fixture)
			if err != nil {
				t.Skipf("fixture not available: %v", err)
			}
			got, err := readLatestRecap(fixture, info.Size())
			if err != nil {
				t.Fatalf("readLatestRecap: %v", err)
			}
			if got == nil {
				t.Fatal("expected non-nil recap from real /recap-bearing jsonl")
			}
			if len(got.Text) == 0 {
				t.Error("recap is empty string; expected text")
			}
			t.Logf("recap (source=%s, len=%d, first 200 chars): %.200s", got.Source, len(got.Text), got.Text)
		})
	}
}
