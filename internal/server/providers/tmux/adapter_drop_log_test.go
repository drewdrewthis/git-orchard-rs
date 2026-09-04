// Regression for #701 AC12: a `-F` row that fails the field-count check must
// be logged at WARN (with raw-vs-expected counts) before being dropped — not
// silently `continue`d as before. Reproduces the D3 mangling by replacing the
// TAB separator with `_`, which is exactly what a non-UTF-8 tmux client emits.

package tmux

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestListAll_MangledRow_LogsWarnAndDrops(t *testing.T) {
	// A well-formed row, then mangled: TAB -> `_` collapses it to one field.
	good := buildListAllLine(
		"alpha", "1700000000", "1", "1700001000", "1", "0",
		"0", "bash", "1", "1", "%1",
		"%1", "Editor", "vim", "12345", "120", "30", "0",
	)
	mangled := strings.ReplaceAll(good, fieldSep, "_")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	a := NewAdapter("h").
		WithRunner(fakeRunner{out: map[string][]byte{"tmux list-panes": []byte(mangled)}}).
		WithLogger(logger)

	sessions, windows, panes, err := a.listAll(context.Background())
	if err != nil {
		t.Fatalf("listAll: %v", err)
	}
	if len(sessions) != 0 || len(windows) != 0 || len(panes) != 0 {
		t.Fatalf("mangled row must yield zero entries; got s=%d w=%d p=%d",
			len(sessions), len(windows), len(panes))
	}

	log := buf.String()
	if !strings.Contains(log, "level=WARN") {
		t.Errorf("expected a WARN log line, got: %q", log)
	}
	for _, want := range []string{"dropped=1", "expected_fields=18", "got_fields=1"} {
		if !strings.Contains(log, want) {
			t.Errorf("WARN log missing %q; got: %q", want, log)
		}
	}
}

// A clean run must not emit a WARN — guards against a spurious drop signal.
func TestListAll_CleanRun_NoWarn(t *testing.T) {
	good := buildListAllLine(
		"alpha", "1700000000", "1", "1700001000", "1", "0",
		"0", "bash", "1", "1", "%1",
		"%1", "Editor", "vim", "12345", "120", "30", "0",
	)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	a := NewAdapter("h").
		WithRunner(fakeRunner{out: map[string][]byte{"tmux list-panes": []byte(good)}}).
		WithLogger(logger)

	sessions, _, _, err := a.listAll(context.Background())
	if err != nil {
		t.Fatalf("listAll: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	if strings.Contains(buf.String(), "WARN") {
		t.Errorf("clean run must not warn; got: %q", buf.String())
	}
}
