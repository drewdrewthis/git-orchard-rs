package claudeprojects

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeJSONL writes lines as newline-terminated JSON to a temp file.
func writeJSONL(t *testing.T, lines []string) (string, int64) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return path, info.Size()
}

func TestReadLatestRecap_ReturnsNilWhenAbsent(t *testing.T) {
	path, size := writeJSONL(t, []string{
		`{"type":"user","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"hi"}}`,
	})
	got, err := readLatestRecap(path, size)
	if err != nil {
		t.Fatalf("readLatestRecap: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil recap; got %q", got.Text)
	}
}

func TestReadLatestRecap_ExtractsLocalCommandStdout(t *testing.T) {
	path, size := writeJSONL(t, []string{
		`{"type":"user","message":{"role":"user","content":"<command-name>/recap</command-name>\n<command-message>recap</command-message>"}}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>We decided X. Next: Y.</local-command-stdout>"}`,
	})
	got, err := readLatestRecap(path, size)
	if err != nil {
		t.Fatalf("readLatestRecap: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil recap")
	}
	const want = "We decided X. Next: Y."
	if got.Text != want {
		t.Errorf("recap = %q; want %q", got.Text, want)
	}
	if got.Source != RecapSourceCommand {
		t.Errorf("source = %q; want RECAP_COMMAND", got.Source)
	}
}

func TestReadLatestRecap_LastInvocationWins(t *testing.T) {
	path, size := writeJSONL(t, []string{
		`{"type":"user","message":{"role":"user","content":"<command-name>/recap</command-name>"}}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>FIRST</local-command-stdout>"}`,
		`{"type":"user","message":{"role":"user","content":"some other prompt"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"reply"}}`,
		`{"type":"user","message":{"role":"user","content":"<command-name>/recap</command-name>"}}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>LATEST</local-command-stdout>"}`,
	})
	got, err := readLatestRecap(path, size)
	if err != nil {
		t.Fatalf("readLatestRecap: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil recap")
	}
	if got.Text != "LATEST" {
		t.Errorf("recap = %q; want LATEST", got.Text)
	}
}

func TestReadLatestRecap_IgnoresUnrelatedSlashCommands(t *testing.T) {
	path, size := writeJSONL(t, []string{
		`{"type":"user","message":{"role":"user","content":"<command-name>/status</command-name>"}}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>status output</local-command-stdout>"}`,
		`{"type":"user","message":{"role":"user","content":"<command-name>/memory</command-name>"}}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>memory dump</local-command-stdout>"}`,
	})
	got, err := readLatestRecap(path, size)
	if err != nil {
		t.Fatalf("readLatestRecap: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil; got %q", got.Text)
	}
}

func TestReadLatestRecap_HandlesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readLatestRecap(path, 0)
	if err != nil {
		t.Fatalf("readLatestRecap: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil on empty file; got %q", got.Text)
	}
}

func TestReadLatestRecap_RecapBetweenUnrelatedRecords(t *testing.T) {
	// Real-world shape: many turns precede the recap, and the recap is
	// followed by more turns. The walk-backwards-from-tail logic must
	// not mis-pair with later records.
	path, size := writeJSONL(t, []string{
		`{"type":"user","message":{"role":"user","content":"first prompt"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"first reply"}}`,
		`{"type":"user","message":{"role":"user","content":"<command-name>/recap</command-name>\n<command-args></command-args>"}}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>The recap text spans multiple words.</local-command-stdout>"}`,
		`{"type":"user","message":{"role":"user","content":"keep going"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"sure"}}`,
	})
	got, err := readLatestRecap(path, size)
	if err != nil {
		t.Fatalf("readLatestRecap: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil recap")
	}
	if got.Text != "The recap text spans multiple words." {
		t.Errorf("recap = %q", got.Text)
	}
}

func TestReadLatestRecap_OnlyAwaySummary(t *testing.T) {
	path, size := writeJSONL(t, []string{
		`{"type":"user","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"hi"}}`,
		`{"type":"system","subtype":"away_summary","content":"Auto recap: we shipped X.","timestamp":"2026-09-03T00:00:00Z","uuid":"u1"}`,
	})
	got, err := readLatestRecap(path, size)
	if err != nil {
		t.Fatalf("readLatestRecap: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil recap")
	}
	if got.Text != "Auto recap: we shipped X." {
		t.Errorf("recap = %q", got.Text)
	}
	if got.Source != RecapSourceAwaySummary {
		t.Errorf("source = %q; want AWAY_SUMMARY", got.Source)
	}
}

func TestReadLatestRecap_BothAwaySummaryNewer(t *testing.T) {
	path, size := writeJSONL(t, []string{
		`{"type":"user","message":{"role":"user","content":"<command-name>/recap</command-name>"}}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>OLD RECAP</local-command-stdout>"}`,
		`{"type":"assistant","message":{"role":"assistant","content":"more work"}}`,
		`{"type":"system","subtype":"away_summary","content":"NEWER AUTO","timestamp":"2026-09-03T01:00:00Z","uuid":"u2"}`,
	})
	got, err := readLatestRecap(path, size)
	if err != nil {
		t.Fatalf("readLatestRecap: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil recap")
	}
	if got.Text != "NEWER AUTO" || got.Source != RecapSourceAwaySummary {
		t.Errorf("got %q / %q; want NEWER AUTO / AWAY_SUMMARY", got.Text, got.Source)
	}
}

func TestReadLatestRecap_BothRecapCommandNewer(t *testing.T) {
	path, size := writeJSONL(t, []string{
		`{"type":"system","subtype":"away_summary","content":"OLD AUTO","timestamp":"2026-09-03T00:00:00Z","uuid":"u3"}`,
		`{"type":"assistant","message":{"role":"assistant","content":"work"}}`,
		`{"type":"user","message":{"role":"user","content":"<command-name>/recap</command-name>"}}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>NEWER RECAP</local-command-stdout>"}`,
	})
	got, err := readLatestRecap(path, size)
	if err != nil {
		t.Fatalf("readLatestRecap: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil recap")
	}
	if got.Text != "NEWER RECAP" || got.Source != RecapSourceCommand {
		t.Errorf("got %q / %q; want NEWER RECAP / RECAP_COMMAND", got.Text, got.Source)
	}
}

func TestReadLatestRecap_MalformedAwaySummarySkipped(t *testing.T) {
	path, size := writeJSONL(t, []string{
		// away_summary-shaped but invalid JSON (unterminated string).
		`{"type":"system","subtype":"away_summary","content":"broken`,
		`{"type":"user","message":{"role":"user","content":"<command-name>/recap</command-name>"}}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>VALID RECAP</local-command-stdout>"}`,
	})
	got, err := readLatestRecap(path, size)
	if err != nil {
		t.Fatalf("readLatestRecap raised error on malformed line: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil recap")
	}
	if got.Text != "VALID RECAP" || got.Source != RecapSourceCommand {
		t.Errorf("got %q / %q; want VALID RECAP / RECAP_COMMAND", got.Text, got.Source)
	}
}

func TestReadLatestRecap_AwaySummaryBeyondTailWindowFoundViaHead(t *testing.T) {
	// The only away_summary sits at the very start; enough filler follows
	// to push it outside the maxLatestMarkersWindow tail budget, so it is
	// found only by the full head-scan fallback.
	lines := []string{
		`{"type":"system","subtype":"away_summary","content":"FAR BACK AUTO","timestamp":"2026-09-03T00:00:00Z","uuid":"u4"}`,
	}
	filler := `{"type":"assistant","message":{"role":"assistant","content":"` + strings.Repeat("x", 1000) + `"}}`
	fillerLines := int(maxLatestMarkersWindow/int64(len(filler))) + 16
	for i := 0; i < fillerLines; i++ {
		lines = append(lines, filler)
	}
	path, size := writeJSONL(t, lines)
	if size <= maxLatestMarkersWindow {
		t.Fatalf("test setup: file size %d must exceed window %d", size, maxLatestMarkersWindow)
	}
	got, err := readLatestRecap(path, size)
	if err != nil {
		t.Fatalf("readLatestRecap: %v", err)
	}
	if got == nil {
		t.Fatal("expected away_summary found via head fallback")
	}
	if got.Text != "FAR BACK AUTO" || got.Source != RecapSourceAwaySummary {
		t.Errorf("got %q / %q; want FAR BACK AUTO / AWAY_SUMMARY", got.Text, got.Source)
	}
}
