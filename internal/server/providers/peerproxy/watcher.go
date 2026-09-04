package peerproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// defaultDebounce is the coalescing window for bursty fsnotify events.
// Editor save bursts (typically 3-10 events within ~200ms) collapse into
// a single LoadFederationConfig + ApplyPeers call.
const defaultDebounce = 1 * time.Second

// debounceTimer is the minimal timer surface the run loop needs; both
// *time.Timer and the test clock satisfy it.
type debounceTimer interface{ Stop() bool }

// ConfigWatcherOption configures a ConfigWatcher at construction time.
// Mirror the ProviderOption pattern so callers use the same idiom for both.
type ConfigWatcherOption func(*ConfigWatcher)

// WithDebounce overrides the debounce window. The primary use case is
// tests: passing a short duration (e.g. 100ms) avoids sleeping for the
// full 1-second default.
func WithDebounce(d time.Duration) ConfigWatcherOption {
	return func(cw *ConfigWatcher) { cw.debounce = d }
}

// ConfigWatcher watches the federation config file for changes and calls
// Provider.ApplyPeers whenever the file is written, created, or renamed.
//
// The watcher monitors the parent directory (not the file directly) to
// survive atomic-rename edits that editors and `orchard config` use. Only
// events whose basename matches the configured file are forwarded.
//
// Bursty editor saves (multiple fsnotify events within the debounce
// window) are coalesced into a single LoadFederationConfig + ApplyPeers
// call. The default debounce window is 1 second; use WithDebounce to
// override in tests.
//
// Lifetime: Start spawns a goroutine that runs until ctx is cancelled.
// Close shuts the underlying fsnotify watcher and waits for the goroutine
// to exit. Callers should tie the ctx to the server lifetime and call
// Close in a defer or cleanup hook.
type ConfigWatcher struct {
	path     string
	provider *Provider
	logger   *slog.Logger
	watcher  *fsnotify.Watcher
	closedCh chan struct{}
	debounce time.Duration

	// afterFunc arms the debounce timer (default time.AfterFunc); tests inject
	// a controllable clock so a burst coalesces by construction, not by racing
	// a wall-clock window (issue #773). onReload, when set, fires after each
	// completed reload so a test can sync on the batch boundary, not sleep.
	afterFunc func(time.Duration, func()) debounceTimer
	onReload  func()

	// reloadCount is incremented each time a successful LoadFederationConfig
	// + ApplyPeers cycle completes. Parse errors do NOT increment this
	// counter. Exposed via ReloadCount() as a test seam.
	reloadCount atomic.Int64

	// applyPeersCount is incremented immediately before each ApplyPeers call.
	// A parse error keeps this counter unchanged because ApplyPeers is never
	// called on a broken config. Exposed via ApplyPeersInvocationCount() as a
	// test seam for the parse-error scenario.
	applyPeersCount atomic.Int64
}

// NewConfigWatcher constructs a ConfigWatcher that reloads the federation
// config at path and applies changes to provider whenever the file is
// modified. Pass WithDebounce to shorten the coalescing window in tests.
func NewConfigWatcher(path string, provider *Provider, logger *slog.Logger, opts ...ConfigWatcherOption) *ConfigWatcher {
	if logger == nil {
		logger = slog.Default()
	}
	cw := &ConfigWatcher{
		path:     path,
		provider: provider,
		logger:   logger,
		closedCh: make(chan struct{}),
		debounce: defaultDebounce,
		afterFunc: func(d time.Duration, f func()) debounceTimer {
			return time.AfterFunc(d, f)
		},
	}
	for _, opt := range opts {
		opt(cw)
	}
	return cw
}

// ReloadCount returns the number of successful LoadFederationConfig +
// ApplyPeers cycles. Parse failures do not increment this counter.
// Intended for tests that assert burst coalescing — a count of 1 after
// N rapid events means the debouncer worked correctly.
func (cw *ConfigWatcher) ReloadCount() int {
	return int(cw.reloadCount.Load())
}

// ApplyPeersInvocationCount returns how many times ApplyPeers has been
// called by the watcher. On a config parse error the watcher skips
// ApplyPeers entirely, so this counter is unaffected by bad writes.
// The primary use case is the parse-error scenario: after writing malformed
// JSON, this count must remain unchanged — i.e., the broken config never
// reaches the provider.
func (cw *ConfigWatcher) ApplyPeersInvocationCount() int {
	return int(cw.applyPeersCount.Load())
}

// Start creates an fsnotify watcher on the parent directory of the config
// file, ensures the directory exists, and spawns a goroutine that reloads
// and applies the federation config on every matching filesystem event.
//
// Start is not idempotent — call it once per ConfigWatcher.
func (cw *ConfigWatcher) Start(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("peerproxy watcher: fsnotify new: %w", err)
	}
	cw.watcher = w

	dir := filepath.Dir(cw.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		_ = w.Close()
		return fmt.Errorf("peerproxy watcher: ensure config dir %s: %w", dir, err)
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return fmt.Errorf("peerproxy watcher: watch %s: %w", dir, err)
	}

	go cw.run(ctx)
	return nil
}

// run drains fsnotify events, filters to the config basename, and calls
// LoadFederationConfig + ApplyPeers once per debounce window. Bursty
// saves (multiple events within debounce) collapse into a single reload.
//
// Debounce: each matching event resets the timer to d-from-now; when it
// fires it sends on reloadCh and the select performs the reload. ctx.Done
// stops any pending timer and exits, dropping the pending reload.
//
// A dedicated reloadCh (not a timer channel) avoids the time.Timer.Reset
// footgun: the goroutine only ever reads from reloadCh.
func (cw *ConfigWatcher) run(ctx context.Context) {
	defer close(cw.closedCh)

	base := filepath.Base(cw.path)

	// reloadCh is written by the AfterFunc goroutine and read by the
	// select below. Buffered with 1 so the AfterFunc never blocks even
	// if a previous signal hasn't been consumed yet.
	reloadCh := make(chan struct{}, 1)

	var pendingTimer debounceTimer
	stopTimer := func() {
		if pendingTimer != nil {
			pendingTimer.Stop()
			pendingTimer = nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			stopTimer()
			_ = cw.watcher.Close()
			return

		case ev, ok := <-cw.watcher.Events:
			if !ok {
				stopTimer()
				return
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			cw.logger.Debug("peerproxy: fsnotify event; debouncing config reload",
				"op", ev.Op.String(), "name", ev.Name)

			// Reset the debounce window: stop any running timer, start a
			// fresh one. When it fires it will send on reloadCh.
			stopTimer()
			pendingTimer = cw.afterFunc(cw.debounce, func() {
				select {
				case reloadCh <- struct{}{}:
				default:
					// A previous signal is still pending; skip — one
					// reload will handle all accumulated events.
				}
			})

		case <-reloadCh:
			pendingTimer = nil // timer already fired; nothing to stop
			cw.logger.Debug("peerproxy: debounce window elapsed; reloading config")

			cfg, err := LoadFederationConfig(cw.path)
			if err != nil {
				// Log at Warn — this is operator-actionable. Include the path
				// and, when available, the exact line:column from a JSON syntax
				// error so the operator can pinpoint the mistake.
				logParseError(cw.logger, cw.path, err)
				continue // ApplyPeers is intentionally NOT called on parse failure
			}
			cw.applyPeersCount.Add(1)
			if err := cw.provider.ApplyPeers(cfg); err != nil {
				cw.logger.Warn("peerproxy: ApplyPeers error after config reload",
					"err", err)
			}
			cw.reloadCount.Add(1)
			if cw.onReload != nil {
				cw.onReload()
			}

		case err, ok := <-cw.watcher.Errors:
			if !ok {
				stopTimer()
				return
			}
			cw.logger.Warn("peerproxy: fsnotify error", "err", err)
		}
	}
}

// logParseError logs a config parse failure at Warn level. When the
// underlying error is a *json.SyntaxError, the offset is translated to a
// 1-based line:column pair by reading the file and counting newlines —
// giving operators an exact location to fix. For I/O errors or other
// non-syntax failures, only the path and error string are logged.
func logParseError(logger *slog.Logger, path string, err error) {
	var synErr *json.SyntaxError
	if errors.As(err, &synErr) && synErr.Offset > 0 {
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			// Clamp offset to file length — SyntaxError can report len(data)
			// when the EOF itself is the problem.
			offset := synErr.Offset
			if offset > int64(len(data)) {
				offset = int64(len(data))
			}
			line, col := offsetToLineCol(data, offset)
			logger.Warn("peerproxy: config parse error; keeping existing peers",
				"path", path, "line", line, "column", col, "err", err)
			return
		}
	}
	logger.Warn("peerproxy: config reload failed; keeping existing peers",
		"path", path, "err", err)
}

// offsetToLineCol converts a byte offset (1-indexed as json.SyntaxError
// reports) into a 1-based line:column pair. The column is the number of
// bytes after the last newline before the offset (not rune count — keeping
// it simple for operator display).
func offsetToLineCol(data []byte, offset int64) (line, col int) {
	// json.SyntaxError.Offset is 1-indexed; convert to 0-indexed for slicing.
	pos := int(offset) - 1
	if pos < 0 {
		pos = 0
	}
	if pos > len(data) {
		pos = len(data)
	}
	line = 1 + bytes.Count(data[:pos], []byte{'\n'})
	lastNL := bytes.LastIndexByte(data[:pos], '\n')
	col = pos - lastNL // lastNL is -1 when no newline found: col = pos+1 = correct
	return line, col
}

// Close shuts down the fsnotify watcher and waits for the run goroutine
// to exit. Idempotent: a second call is a no-op.
func (cw *ConfigWatcher) Close() error {
	if cw.watcher == nil {
		return nil
	}
	err := cw.watcher.Close()
	<-cw.closedCh
	cw.watcher = nil
	if err != nil {
		return fmt.Errorf("peerproxy watcher: close: %w", err)
	}
	return nil
}
