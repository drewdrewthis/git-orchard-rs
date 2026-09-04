package peerproxy

import (
	"sync"
	"time"
)

// FakeClock is a test-only debounce clock. It never fires on wall-clock time;
// the test drives the pending debounce callbacks explicitly via FireAll.
//
// Unlike a single mutable callback slot, FakeClock records EVERY armed timer as
// an independent entry. Stop() cancels only its own entry (mirroring
// time.Timer.Stop). A correctly-disciplined debouncer Stops the previous timer
// before arming the next, so a burst of N events leaves N-1 cancelled entries
// and exactly one live entry — and FireAll therefore drives exactly one reload.
// A debouncer that forgot to Stop the previous timer would leave every entry
// live, and FireAll would drive N reloads — which the burst test detects.
type FakeClock struct {
	mu      sync.Mutex
	entries []*fakeEntry
	armed   chan struct{}
}

// fakeEntry is one armed debounce timer. cancelled is set by Stop(); fired is
// set by FireAll so a timer is never fired twice.
type fakeEntry struct {
	fn        func()
	cancelled bool
	fired     bool
}

// NewFakeClock returns a FakeClock ready to inject via WithFakeClockForTest.
func NewFakeClock() *FakeClock {
	return &FakeClock{armed: make(chan struct{}, 64)}
}

// afterFunc records a new armed timer entry and signals that a timer was armed.
// It ignores the duration — firing is manual, via FireAll.
func (c *FakeClock) afterFunc(_ time.Duration, f func()) debounceTimer {
	e := &fakeEntry{fn: f}
	c.mu.Lock()
	c.entries = append(c.entries, e)
	c.mu.Unlock()
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return fakeTimer{c: c, e: e}
}

// FireAll blocks until at least one debounce timer has been armed, then fires
// every timer that is still live (not cancelled by Stop, not already fired)
// exactly once. It waits for each resulting reload to complete (a receive on
// reloaded, wired by WithReloadHookForTest) before firing the next, so reloads
// are serialised and can never coalesce through the run loop's buffered
// channel. Live timer count therefore maps 1:1 onto reload count:
//   - correct debouncer  → 1 live entry → exactly 1 reload
//   - dropped stopTimer  → N live entries → N reloads (test fails)
func (c *FakeClock) FireAll(reloaded <-chan struct{}) {
	<-c.armed
	for {
		c.mu.Lock()
		var live []*fakeEntry
		for _, e := range c.entries {
			if !e.cancelled && !e.fired {
				e.fired = true
				live = append(live, e)
			}
		}
		c.mu.Unlock()
		if len(live) == 0 {
			// The only live timer was cancelled by an in-flight event whose
			// re-arm has not been recorded yet; wait for it and retry.
			<-c.armed
			continue
		}
		for _, e := range live {
			e.fn()
			<-reloaded
		}
		return
	}
}

type fakeTimer struct {
	c *FakeClock
	e *fakeEntry
}

// Stop cancels this timer's own entry so FireAll will not fire it, mirroring
// time.Timer.Stop, and reports whether the timer was still live. This is the
// crux of the burst test: the debouncer must Stop the previous timer before
// arming the next, so a burst leaves exactly one live entry to fire.
func (t fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.e.cancelled && !t.e.fired
	t.e.cancelled = true
	return was
}

// WithFakeClockForTest injects a FakeClock as the watcher's debounce clock.
func WithFakeClockForTest(c *FakeClock) ConfigWatcherOption {
	return func(cw *ConfigWatcher) { cw.afterFunc = c.afterFunc }
}

// WithReloadHookForTest registers a callback fired after each completed
// reload cycle, letting a test synchronise on the batch boundary.
func WithReloadHookForTest(h func()) ConfigWatcherOption {
	return func(cw *ConfigWatcher) { cw.onReload = h }
}
