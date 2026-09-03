package main

import (
	"testing"
	"time"
)

// TestApplyFastPaneMapOwnership is the unit guard for subscribe.go's
// documented split: the push lane owns paneToSess while it's live, and a
// stale fast poll landing after it must not revert the map.
func TestApplyFastPaneMapOwnership(t *testing.T) {
	t.Run("live subscription: fast poll does not overwrite the map", func(t *testing.T) {
		m := &model{
			subAt:      time.Now(),
			paneToSess: map[string]string{"%0": "live-session"},
		}
		m.applyFast(fastDataMsg{paneToSess: map[string]string{"%0": "stale-session"}})
		if got := m.paneToSess["%0"]; got != "live-session" {
			t.Errorf("paneToSess[%%0] = %q, want unchanged %q (subscription owns the map while live)",
				got, "live-session")
		}
	})

	t.Run("no live subscription: fast poll populates the map", func(t *testing.T) {
		m := &model{}
		m.applyFast(fastDataMsg{paneToSess: map[string]string{"%0": "polled-session"}})
		if got := m.paneToSess["%0"]; got != "polled-session" {
			t.Errorf("paneToSess[%%0] = %q, want %q (poll owns the map without a live push lane)",
				got, "polled-session")
		}
	})
}
