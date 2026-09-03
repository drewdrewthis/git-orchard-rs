package main

// Regression tests for the shared-width bootstrap (#742).
//
// @orchard_sidebar_width is written only by the sidebar itself, after it has
// READ a non-zero value from that same option -- so on a machine where the
// option was never seeded, desiredWidth stays zero forever and the
// WindowSizeMsg enforcement (which is what corrects proportional
// redistribution in detached sessions, #736) never arms. The fix adopts this
// pane's actual width as the shared truth when the option reads empty, and
// publishes it once so every other session follows.

import (
	"testing"
)

func TestBootstrapAdoptsUnpublishedWidth(t *testing.T) {
	var wrote []int
	origSet := setWidthOption
	setWidthOption = func(w int) { wrote = append(wrote, w) }
	defer func() { setWidthOption = origSet }()

	t.Run("empty option adopts current width and publishes", func(t *testing.T) {
		wrote = nil
		m := &model{width: 42}
		m.Update(clientSessMsg{name: "a", width: 0})
		if m.desiredWidth != 42 {
			t.Errorf("desiredWidth = %d, want 42 adopted from current pane width", m.desiredWidth)
		}
		if len(wrote) != 1 || wrote[0] != 42 {
			t.Errorf("setWidthOption calls = %v, want [42] -- deadlock otherwise", wrote)
		}
	})

	t.Run("sub-floor pane is not adopted", func(t *testing.T) {
		wrote = nil
		m := &model{width: 20}
		m.Update(clientSessMsg{name: "a", width: 0})
		if m.desiredWidth != 0 || len(wrote) != 0 {
			t.Errorf("adopted from a %d-wide pane: desired=%d wrote=%v -- would publish junk", 20, m.desiredWidth, wrote)
		}
	})

	t.Run("known desired is not clobbered by an empty read", func(t *testing.T) {
		wrote = nil
		m := &model{width: 45, desiredWidth: 45}
		m.Update(clientSessMsg{name: "a", width: 0})
		if m.desiredWidth != 45 || len(wrote) != 0 {
			t.Errorf("desired=%d wrote=%v, want untouched 45/[]", m.desiredWidth, wrote)
		}
	})
}
