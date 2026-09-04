package peerproxy

import (
	"sync"
	"time"
)

// FakeClock is a test-only debounce clock. It never fires on wall-clock
// time; the test drives the single pending debounce callback explicitly
// via FireAfterArmed, so a burst of events coalesces into exactly one
// reload by construction rather than by racing the debounce window.
type FakeClock struct {
	mu    sync.Mutex
	fn    func()
	armed chan struct{}
}

// NewFakeClock returns a FakeClock ready to inject via WithFakeClockForTest.
func NewFakeClock() *FakeClock {
	return &FakeClock{armed: make(chan struct{}, 1)}
}

// afterFunc records the (latest) debounce callback and signals that a
// timer was armed. It ignores the duration — firing is manual.
func (c *FakeClock) afterFunc(_ time.Duration, f func()) debounceTimer {
	c.mu.Lock()
	c.fn = f
	c.mu.Unlock()
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return fakeTimer{c}
}

// FireAfterArmed blocks until the watcher has armed at least one debounce
// timer, then fires the current callback exactly once. Any callback armed
// afterwards is left un-fired, so the reload count is deterministic.
func (c *FakeClock) FireAfterArmed() {
	<-c.armed
	c.mu.Lock()
	fn := c.fn
	c.fn = nil
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

type fakeTimer struct{ c *FakeClock }

// Stop reports whether a callback was armed but does NOT clear it: the run
// loop's reset immediately re-arms via afterFunc, and keeping the latest
// callback live guarantees FireAfterArmed never observes a nil mid-reset
// (which would hang the reload wait).
func (t fakeTimer) Stop() bool {
	t.c.mu.Lock()
	had := t.c.fn != nil
	t.c.mu.Unlock()
	return had
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
