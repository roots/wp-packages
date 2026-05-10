package reports

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/roots/wp-packages/internal/testutil"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"bPlugins", "bplugins"},
		{"WPFactory", "wpfactory"},
		{"Liton Arefin", "liton-arefin"},
		{"Tom & Jerry", "tom-jerry"},
		{"  spaced  ", "spaced"},
		{"---hyphens---", "hyphens"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := slugify(tt.in); got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGroupByVendor_StripsHTMLAndIgnoresEmpty(t *testing.T) {
	now := time.Now()
	in := []closure{
		{Slug: "a", Author: "Acme", Time: now},
		{Slug: "b", Author: "acme", Time: now},
		{Slug: "c", Author: "", Time: now},
		{Slug: "d", Author: "Other", Time: now},
	}
	got := groupByVendor(in)

	if len(got["Acme"]) != 2 {
		t.Errorf("expected 2 closures for Acme, got %d", len(got["Acme"]))
	}
	if len(got["Other"]) != 1 {
		t.Errorf("expected 1 closure for Other, got %d", len(got["Other"]))
	}
	if _, exists := got[""]; exists {
		t.Error("empty author should be skipped")
	}
}

func TestTrackMassClosures_CreatesEventAtThreshold(t *testing.T) {
	db := testutil.OpenTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	insertPackageWithAuthor(t, db, "plugin", "alpha", "Acme Inc")
	insertPackageWithAuthor(t, db, "plugin", "beta", "Acme Inc")
	runID := insertStatusCheck(t, db, now)
	insertChange(t, db, runID, "plugin", "alpha", "deactivated", now)
	insertChange(t, db, runID, "plugin", "beta", "deactivated", now)

	if err := TrackMassClosures(ctx, db); err != nil {
		t.Fatalf("TrackMassClosures: %v", err)
	}

	var count int
	var slugsJSON string
	err := db.QueryRowContext(ctx,
		"SELECT plugin_count, plugin_slugs FROM closure_events WHERE vendor_slug = ?",
		"acme-inc").Scan(&count, &slugsJSON)
	if err != nil {
		t.Fatalf("selecting event: %v", err)
	}
	if count != 2 {
		t.Errorf("plugin_count = %d, want 2", count)
	}
	var slugs []string
	if err := json.Unmarshal([]byte(slugsJSON), &slugs); err != nil {
		t.Fatalf("unmarshaling slugs: %v", err)
	}
	if len(slugs) != 2 {
		t.Errorf("len(slugs) = %d, want 2", len(slugs))
	}
}

func TestTrackMassClosures_BelowThreshold_NoEvent(t *testing.T) {
	db := testutil.OpenTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	insertPackageWithAuthor(t, db, "plugin", "lonely", "Solo Dev")
	runID := insertStatusCheck(t, db, now)
	insertChange(t, db, runID, "plugin", "lonely", "deactivated", now)

	if err := TrackMassClosures(ctx, db); err != nil {
		t.Fatalf("TrackMassClosures: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM closure_events WHERE vendor_slug = ?",
		"solo-dev").Scan(&count); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 events for solo-dev, got %d", count)
	}
}

func TestTrackMassClosures_UpdatesExistingEventInWindow(t *testing.T) {
	db := testutil.OpenTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	insertPackageWithAuthor(t, db, "plugin", "alpha", "Acme Inc")
	insertPackageWithAuthor(t, db, "plugin", "beta", "Acme Inc")
	insertPackageWithAuthor(t, db, "plugin", "gamma", "Acme Inc")

	earlier := now.Add(-1 * time.Hour)
	runID1 := insertStatusCheck(t, db, earlier)
	insertChange(t, db, runID1, "plugin", "alpha", "deactivated", earlier)
	insertChange(t, db, runID1, "plugin", "beta", "deactivated", earlier)
	if err := TrackMassClosures(ctx, db); err != nil {
		t.Fatalf("first TrackMassClosures: %v", err)
	}

	runID2 := insertStatusCheck(t, db, now)
	insertChange(t, db, runID2, "plugin", "gamma", "deactivated", now)
	if err := TrackMassClosures(ctx, db); err != nil {
		t.Fatalf("second TrackMassClosures: %v", err)
	}

	var count, pluginCount int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*), MAX(plugin_count) FROM closure_events WHERE vendor_slug = ?",
		"acme-inc").Scan(&count, &pluginCount); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 event (existing should be updated), got %d", count)
	}
	if pluginCount != 3 {
		t.Errorf("expected plugin_count=3 after merge, got %d", pluginCount)
	}
}

func insertPackageWithAuthor(t *testing.T, db *sql.DB, pkgType, name, author string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO packages (type, name, author, last_committed, is_active, versions_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 1, '{}', ?, ?)`,
		pkgType, name, author, now, now, now)
	if err != nil {
		t.Fatalf("inserting package %s: %v", name, err)
	}
}

func insertStatusCheck(t *testing.T, db *sql.DB, started time.Time) int64 {
	t.Helper()
	res, err := db.ExecContext(context.Background(),
		`INSERT INTO status_checks (started_at) VALUES (?)`, started.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("inserting status_check: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

func insertChange(t *testing.T, db *sql.DB, runID int64, pkgType, name, action string, ts time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO status_check_changes (status_check_id, package_type, package_name, action, created_at) VALUES (?, ?, ?, ?, ?)`,
		runID, pkgType, name, action, ts.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("inserting status_check_change: %v", err)
	}
}
