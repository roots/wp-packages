package cmd

import (
	"testing"
	"time"
)

func TestShouldAdvanceSyncedAt(t *testing.T) {
	now := time.Now().UTC()

	t.Run("versions changed", func(t *testing.T) {
		committed := now.Add(-5 * time.Minute)
		got := shouldAdvanceSyncedAt(`{"1.0":"url"}`, `{}`, false, "plugin", &committed, now)
		if got != syncAdvance {
			t.Errorf("got %d, want syncAdvance", got)
		}
	})

	t.Run("versions unchanged within window", func(t *testing.T) {
		committed := now.Add(-10 * time.Minute)
		got := shouldAdvanceSyncedAt(`{"1.0":"url"}`, `{"1.0":"url"}`, false, "plugin", &committed, now)
		if got != syncRetry {
			t.Errorf("got %d, want syncRetry", got)
		}
	})

	t.Run("versions unchanged after window", func(t *testing.T) {
		committed := now.Add(-25 * time.Hour)
		got := shouldAdvanceSyncedAt(`{"1.0":"url"}`, `{"1.0":"url"}`, false, "plugin", &committed, now)
		if got != syncExpire {
			t.Errorf("got %d, want syncExpire", got)
		}
	})

	t.Run("versions unchanged nil last_committed", func(t *testing.T) {
		got := shouldAdvanceSyncedAt(`{"1.0":"url"}`, `{"1.0":"url"}`, false, "plugin", nil, now)
		if got != syncExpire {
			t.Errorf("got %d, want syncExpire", got)
		}
	})

	t.Run("versions unchanged at window boundary", func(t *testing.T) {
		committed := now.Add(-staleRetryWindow)
		got := shouldAdvanceSyncedAt(`{"1.0":"url"}`, `{"1.0":"url"}`, false, "plugin", &committed, now)
		if got != syncRetry {
			t.Errorf("got %d, want syncRetry (at boundary)", got)
		}
	})

	t.Run("versions unchanged just past window", func(t *testing.T) {
		committed := now.Add(-staleRetryWindow - 1*time.Second)
		got := shouldAdvanceSyncedAt(`{"1.0":"url"}`, `{"1.0":"url"}`, false, "plugin", &committed, now)
		if got != syncExpire {
			t.Errorf("got %d, want syncExpire (just past window)", got)
		}
	})

	// The neve case: the sync triggered by a new tag's commit picks up an
	// incidental change (a previous release finally going live) while the new
	// tag itself is still filtered out as above-stable. Advancing here would
	// strand the package once the release is published.
	t.Run("pending stable overrides versions changed", func(t *testing.T) {
		committed := now.Add(-5 * time.Minute)
		got := shouldAdvanceSyncedAt(`{"1.0":"url"}`, `{}`, true, "theme", &committed, now)
		if got != syncRetry {
			t.Errorf("got %d, want syncRetry", got)
		}
	})

	t.Run("pending stable theme uses extended window", func(t *testing.T) {
		committed := now.Add(-3 * 24 * time.Hour)
		got := shouldAdvanceSyncedAt(`{"1.0":"url"}`, `{"1.0":"url"}`, true, "theme", &committed, now)
		if got != syncRetry {
			t.Errorf("got %d, want syncRetry (3 days is within theme window)", got)
		}
	})

	t.Run("pending stable theme past extended window", func(t *testing.T) {
		committed := now.Add(-pendingStableRetryWindow - 1*time.Second)
		got := shouldAdvanceSyncedAt(`{"1.0":"url"}`, `{"1.0":"url"}`, true, "theme", &committed, now)
		if got != syncExpire {
			t.Errorf("got %d, want syncExpire (past extended window)", got)
		}
	})

	// Plugins keep the standard window even with a pending stable: a stable
	// tag above the Stable Tag is often intentional there, not directory lag.
	t.Run("pending stable plugin keeps standard window", func(t *testing.T) {
		committed := now.Add(-25 * time.Hour)
		got := shouldAdvanceSyncedAt(`{"1.0":"url"}`, `{}`, true, "plugin", &committed, now)
		if got != syncAdvance {
			t.Errorf("got %d, want syncAdvance (past standard window, versions changed)", got)
		}
	})

	t.Run("pending stable plugin within standard window", func(t *testing.T) {
		committed := now.Add(-10 * time.Minute)
		got := shouldAdvanceSyncedAt(`{"1.0":"url"}`, `{}`, true, "plugin", &committed, now)
		if got != syncRetry {
			t.Errorf("got %d, want syncRetry", got)
		}
	})
}
