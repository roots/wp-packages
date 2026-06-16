package packages

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/roots/wp-packages/internal/db"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}

	// Create packages table inline (avoid embed dependency in tests)
	_, err = database.Exec(`
		CREATE TABLE packages (
			id INTEGER PRIMARY KEY,
			type TEXT NOT NULL CHECK(type IN ('plugin','theme')),
			name TEXT NOT NULL,
			display_name TEXT,
			description TEXT,
			author TEXT,
			homepage TEXT,
			slug_url TEXT,
			versions_json TEXT NOT NULL DEFAULT '{}',
			downloads INTEGER NOT NULL DEFAULT 0,
			active_installs INTEGER NOT NULL DEFAULT 0,
			current_version TEXT,
			wporg_version TEXT,
			rating REAL,
			num_ratings INTEGER NOT NULL DEFAULT 0,
			is_active INTEGER NOT NULL DEFAULT 1,
			permanently_closed INTEGER NOT NULL DEFAULT 0,
			last_committed TEXT,
			last_synced_at TEXT,
			last_sync_run_id INTEGER,
			trunk_revision INTEGER,
			content_hash TEXT,
			deployed_hash TEXT,
			content_changed_at TEXT,
			wp_packages_installs_total INTEGER NOT NULL DEFAULT 0,
			wp_packages_installs_30d INTEGER NOT NULL DEFAULT 0,
			last_installed_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(type, name)
		);
			CREATE TABLE sync_runs (
				id INTEGER PRIMARY KEY,
				started_at TEXT NOT NULL,
				finished_at TEXT,
				status TEXT NOT NULL,
				meta_json TEXT NOT NULL DEFAULT '{}'
			);
			CREATE TABLE package_stats (
				id INTEGER PRIMARY KEY CHECK (id = 1),
				active_plugins INTEGER NOT NULL DEFAULT 0,
				active_themes INTEGER NOT NULL DEFAULT 0,
				plugin_installs INTEGER NOT NULL DEFAULT 0,
				theme_installs INTEGER NOT NULL DEFAULT 0,
				installs_30d INTEGER NOT NULL DEFAULT 0,
				updated_at TEXT NOT NULL DEFAULT ''
			);
		`)
	if err != nil {
		t.Fatalf("creating tables: %v", err)
	}

	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestUpsertShellPackage(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	lc := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	err := UpsertShellPackage(ctx, database, "plugin", "akismet", &lc)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Verify row exists
	var name string
	var isActive int
	err = database.QueryRow("SELECT name, is_active FROM packages WHERE type='plugin' AND name='akismet'").Scan(&name, &isActive)
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	if name != "akismet" || isActive != 1 {
		t.Errorf("got name=%s active=%d", name, isActive)
	}

	// Upsert with older date should not update last_committed
	olderLC := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	err = UpsertShellPackage(ctx, database, "plugin", "akismet", &olderLC)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var lastCommitted string
	_ = database.QueryRow("SELECT last_committed FROM packages WHERE name='akismet'").Scan(&lastCommitted)
	if lastCommitted != "2026-01-15T00:00:00Z" {
		t.Errorf("last_committed should not have been overwritten, got %s", lastCommitted)
	}

	// Upsert with newer date should update
	newerLC := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	err = UpsertShellPackage(ctx, database, "plugin", "akismet", &newerLC)
	if err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	_ = database.QueryRow("SELECT last_committed FROM packages WHERE name='akismet'").Scan(&lastCommitted)
	if lastCommitted != "2026-03-01T00:00:00Z" {
		t.Errorf("last_committed should have been updated, got %s", lastCommitted)
	}
}

func TestUpsertPackage(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	ver := "5.0"
	pkg := &Package{
		Type:           "plugin",
		Name:           "akismet",
		VersionsJSON:   `{"5.0":"https://example.com/5.0.zip"}`,
		CurrentVersion: &ver,
		IsActive:       true,
		Downloads:      1000,
	}

	err := UpsertPackage(ctx, database, pkg)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var downloads int64
	_ = database.QueryRow("SELECT downloads FROM packages WHERE name='akismet'").Scan(&downloads)
	if downloads != 1000 {
		t.Errorf("downloads = %d, want 1000", downloads)
	}

	// Update same package
	pkg.Downloads = 2000
	err = UpsertPackage(ctx, database, pkg)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	_ = database.QueryRow("SELECT downloads FROM packages WHERE name='akismet'").Scan(&downloads)
	if downloads != 2000 {
		t.Errorf("downloads = %d, want 2000", downloads)
	}
}

func TestNormalizeAndStoreVersionsUsesWporgVersion(t *testing.T) {
	wporgVersion := "10.7.0"
	pkg := &Package{
		WporgVersion: &wporgVersion,
		RawVersions: map[string]string{
			"10.6.0":    "https://example.com/10.6.0.zip",
			"10.7.0":    "https://example.com/10.7.0.zip",
			"10.8.0":    "https://example.com/10.8.0.zip",
			"dev-trunk": "https://example.com/trunk.zip",
		},
	}

	count, err := pkg.NormalizeAndStoreVersions()
	if err != nil {
		t.Fatalf("normalizing versions: %v", err)
	}
	if count != 3 {
		t.Fatalf("NormalizeAndStoreVersions returned %d entries, want 3", count)
	}
	if pkg.CurrentVersion == nil || *pkg.CurrentVersion != "10.7.0" {
		t.Fatalf("CurrentVersion = %v, want 10.7.0", pkg.CurrentVersion)
	}

	var versions map[string]string
	if err := json.Unmarshal([]byte(pkg.VersionsJSON), &versions); err != nil {
		t.Fatalf("unmarshaling versions: %v", err)
	}
	if _, ok := versions["10.8.0"]; ok {
		t.Fatal("10.8.0 should have been filtered out")
	}
	if versions["10.7.0"] == "" {
		t.Fatal("10.7.0 should remain")
	}
}

func TestDeactivatePackage(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	pkg := &Package{
		Type:         "plugin",
		Name:         "dead-plugin",
		VersionsJSON: "{}",
		IsActive:     true,
	}
	_ = UpsertPackage(ctx, database, pkg)

	var id int64
	_ = database.QueryRow("SELECT id FROM packages WHERE name='dead-plugin'").Scan(&id)

	err := DeactivatePackage(ctx, database, id)
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	var isActive int
	var contentChangedAt *string
	_ = database.QueryRow("SELECT is_active, content_changed_at FROM packages WHERE id=?", id).Scan(&isActive, &contentChangedAt)
	if isActive != 0 {
		t.Error("package should be inactive")
	}
	if contentChangedAt == nil {
		t.Error("content_changed_at should be set after deactivation")
	}
}

// TestStatusChangeUpdatesContentChangedAt simulates the check-status flow:
// an active package is deactivated (closure), then reactivated (re-opening).
// Both transitions must set content_changed_at so the changes feed and
// DB-driven sync pick them up.
func TestStatusChangeUpdatesContentChangedAt(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	pkg := &Package{
		Type: "plugin", Name: "status-test", VersionsJSON: "{}", IsActive: true,
	}
	_ = UpsertPackage(ctx, database, pkg)

	var id int64
	_ = database.QueryRow("SELECT id FROM packages WHERE name='status-test'").Scan(&id)

	// Verify content_changed_at starts unset
	var initial *string
	_ = database.QueryRow("SELECT content_changed_at FROM packages WHERE id=?", id).Scan(&initial)
	if initial != nil {
		t.Fatal("content_changed_at should be nil before any status change")
	}

	// Deactivate (simulates check-status detecting a closure via wp.org 404)
	if err := DeactivatePackage(ctx, database, id); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	var afterDeactivate string
	var isActive int
	_ = database.QueryRow("SELECT is_active, content_changed_at FROM packages WHERE id=?", id).Scan(&isActive, &afterDeactivate)
	if isActive != 0 {
		t.Error("expected inactive after deactivation")
	}
	if afterDeactivate == "" {
		t.Fatal("content_changed_at should be set after deactivation")
	}

	// Reactivate (simulates check-status detecting a re-opening)
	if err := ReactivatePackage(ctx, database, id); err != nil {
		t.Fatalf("reactivate: %v", err)
	}

	var afterReactivate string
	_ = database.QueryRow("SELECT is_active, content_changed_at FROM packages WHERE id=?", id).Scan(&isActive, &afterReactivate)
	if isActive != 1 {
		t.Error("expected active after reactivation")
	}
	if afterReactivate == "" {
		t.Fatal("content_changed_at should be set after reactivation")
	}
	if afterReactivate < afterDeactivate {
		t.Errorf("reactivation timestamp (%s) should be >= deactivation timestamp (%s)",
			afterReactivate, afterDeactivate)
	}
}

func TestRefreshSiteStats(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	cur := "1.0.0"
	p1 := &Package{
		Type:                    "plugin",
		Name:                    "plugin-one",
		VersionsJSON:            "{}",
		CurrentVersion:          &cur,
		IsActive:                true,
		WpPackagesInstallsTotal: 100,
		WpPackagesInstalls30d:   25,
	}
	p2 := &Package{
		Type:                    "plugin",
		Name:                    "plugin-two",
		VersionsJSON:            "{}",
		CurrentVersion:          &cur,
		IsActive:                false,
		WpPackagesInstallsTotal: 999,
		WpPackagesInstalls30d:   999,
	}
	t1 := &Package{
		Type:                    "theme",
		Name:                    "theme-one",
		VersionsJSON:            "{}",
		CurrentVersion:          &cur,
		IsActive:                true,
		WpPackagesInstallsTotal: 50,
		WpPackagesInstalls30d:   5,
	}

	if err := UpsertPackage(ctx, database, p1); err != nil {
		t.Fatalf("upserting plugin-one: %v", err)
	}
	if err := UpsertPackage(ctx, database, p2); err != nil {
		t.Fatalf("upserting plugin-two: %v", err)
	}
	if err := UpsertPackage(ctx, database, t1); err != nil {
		t.Fatalf("upserting theme-one: %v", err)
	}

	// Install counters are maintained by telemetry aggregation, not package upserts.
	if _, err := database.Exec(`UPDATE packages SET wp_packages_installs_total = 100, wp_packages_installs_30d = 25 WHERE name = 'plugin-one'`); err != nil {
		t.Fatalf("updating plugin-one counters: %v", err)
	}
	if _, err := database.Exec(`UPDATE packages SET wp_packages_installs_total = 999, wp_packages_installs_30d = 999 WHERE name = 'plugin-two'`); err != nil {
		t.Fatalf("updating plugin-two counters: %v", err)
	}
	if _, err := database.Exec(`UPDATE packages SET wp_packages_installs_total = 50, wp_packages_installs_30d = 5 WHERE name = 'theme-one'`); err != nil {
		t.Fatalf("updating theme-one counters: %v", err)
	}

	if err := RefreshSiteStats(ctx, database); err != nil {
		t.Fatalf("refreshing site stats: %v", err)
	}

	var activePlugins, activeThemes, pluginInstalls, themeInstalls, installs30d int
	err := database.QueryRow(`SELECT active_plugins, active_themes, plugin_installs, theme_installs, installs_30d FROM package_stats WHERE id = 1`).Scan(
		&activePlugins, &activeThemes, &pluginInstalls, &themeInstalls, &installs30d,
	)
	if err != nil {
		t.Fatalf("querying package_stats: %v", err)
	}

	if activePlugins != 1 {
		t.Errorf("active_plugins = %d, want 1", activePlugins)
	}
	if activeThemes != 1 {
		t.Errorf("active_themes = %d, want 1", activeThemes)
	}
	if pluginInstalls != 100 {
		t.Errorf("plugin_installs = %d, want 100", pluginInstalls)
	}
	if themeInstalls != 50 {
		t.Errorf("theme_installs = %d, want 50", themeInstalls)
	}
	if installs30d != 30 {
		t.Errorf("installs_30d = %d, want 30", installs30d)
	}
}

func TestGetPackagesNeedingUpdate(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	// Package with no last_synced_at — needs update
	lc := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = UpsertShellPackage(ctx, database, "plugin", "needs-update", &lc)

	// Package already synced recently — does not need update
	synced := time.Now().UTC()
	pkg := &Package{
		Type:          "plugin",
		Name:          "up-to-date",
		VersionsJSON:  "{}",
		IsActive:      true,
		LastCommitted: &lc,
		LastSyncedAt:  &synced,
	}
	_ = UpsertPackage(ctx, database, pkg)

	pkgs, err := GetPackagesNeedingUpdate(ctx, database, UpdateQueryOpts{Type: "plugin"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("expected 1 package needing update, got %d", len(pkgs))
	}
	if pkgs[0].Name != "needs-update" {
		t.Errorf("got name=%s, want needs-update", pkgs[0].Name)
	}
}

func TestGetAllPackages(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	_ = UpsertPackage(ctx, database, &Package{
		Type: "plugin", Name: "akismet", VersionsJSON: "{}", IsActive: true,
	})
	_ = UpsertPackage(ctx, database, &Package{
		Type: "theme", Name: "astra", VersionsJSON: "{}", IsActive: true,
	})
	_ = UpsertPackage(ctx, database, &Package{
		Type: "plugin", Name: "closed-plugin", VersionsJSON: "{}", IsActive: false,
	})

	t.Run("all types", func(t *testing.T) {
		pkgs, err := GetAllPackages(ctx, database, "all")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(pkgs) != 3 {
			t.Errorf("expected 3 packages, got %d", len(pkgs))
		}
	})

	t.Run("filter by type", func(t *testing.T) {
		pkgs, err := GetAllPackages(ctx, database, "plugin")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(pkgs) != 2 {
			t.Errorf("expected 2 plugins, got %d", len(pkgs))
		}
	})

	t.Run("includes inactive", func(t *testing.T) {
		pkgs, err := GetAllPackages(ctx, database, "plugin")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		var found bool
		for _, p := range pkgs {
			if p.Name == "closed-plugin" && !p.IsActive {
				found = true
			}
		}
		if !found {
			t.Error("expected inactive plugin to be included")
		}
	})
}

func TestReactivatePackage(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	_ = UpsertPackage(ctx, database, &Package{
		Type: "plugin", Name: "closed-plugin", VersionsJSON: "{}", IsActive: false,
	})

	pkgs, _ := GetAllPackages(ctx, database, "")
	if pkgs[0].IsActive {
		t.Fatal("expected package to start inactive")
	}

	if err := ReactivatePackage(ctx, database, pkgs[0].ID); err != nil {
		t.Fatalf("reactivate: %v", err)
	}

	pkgs, _ = GetAllPackages(ctx, database, "")
	if !pkgs[0].IsActive {
		t.Error("expected package to be active after reactivation")
	}

	var contentChangedAt *string
	_ = database.QueryRow("SELECT content_changed_at FROM packages WHERE id=?", pkgs[0].ID).Scan(&contentChangedAt)
	if contentChangedAt == nil {
		t.Error("content_changed_at should be set after reactivation")
	}
}

func TestBatchUpsertShellPackages(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	lc1 := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	lc2 := time.Date(2026, 2, 20, 0, 0, 0, 0, time.UTC)

	entries := []ShellEntry{
		{Type: "plugin", Name: "akismet", LastCommitted: &lc1},
		{Type: "theme", Name: "astra", LastCommitted: &lc2},
	}

	err := BatchUpsertShellPackages(ctx, database, entries)
	if err != nil {
		t.Fatalf("batch upsert: %v", err)
	}

	var count int
	_ = database.QueryRow("SELECT COUNT(*) FROM packages").Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 packages, got %d", count)
	}

	// Verify individual values
	var name string
	var isActive int
	_ = database.QueryRow("SELECT name, is_active FROM packages WHERE type='plugin' AND name='akismet'").Scan(&name, &isActive)
	if name != "akismet" || isActive != 1 {
		t.Errorf("got name=%s active=%d", name, isActive)
	}

	// Batch upsert with older dates should not overwrite
	olderLC := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	entries2 := []ShellEntry{
		{Type: "plugin", Name: "akismet", LastCommitted: &olderLC},
	}
	err = BatchUpsertShellPackages(ctx, database, entries2)
	if err != nil {
		t.Fatalf("second batch upsert: %v", err)
	}

	var lastCommitted string
	_ = database.QueryRow("SELECT last_committed FROM packages WHERE name='akismet'").Scan(&lastCommitted)
	if lastCommitted != "2026-01-15T00:00:00Z" {
		t.Errorf("last_committed should not have been overwritten, got %s", lastCommitted)
	}
}

func TestBatchUpsertShellPackagesEmpty(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	err := BatchUpsertShellPackages(ctx, database, nil)
	if err != nil {
		t.Fatalf("empty batch upsert should not fail: %v", err)
	}

	err = BatchUpsertShellPackages(ctx, database, []ShellEntry{})
	if err != nil {
		t.Fatalf("empty slice batch upsert should not fail: %v", err)
	}

	var count int
	_ = database.QueryRow("SELECT COUNT(*) FROM packages").Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 packages, got %d", count)
	}
}

func TestBatchUpsertPackages(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	ver1 := "5.0"
	ver2 := "4.0"
	pkgs := []*Package{
		{
			Type:           "plugin",
			Name:           "akismet",
			VersionsJSON:   `{"5.0":"https://example.com/5.0.zip"}`,
			CurrentVersion: &ver1,
			IsActive:       true,
			Downloads:      1000,
		},
		{
			Type:           "theme",
			Name:           "astra",
			VersionsJSON:   `{"4.0":"https://example.com/4.0.zip"}`,
			CurrentVersion: &ver2,
			IsActive:       true,
			Downloads:      500,
		},
	}

	err := BatchUpsertPackages(ctx, database, pkgs)
	if err != nil {
		t.Fatalf("batch upsert: %v", err)
	}

	var count int
	_ = database.QueryRow("SELECT COUNT(*) FROM packages").Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 packages, got %d", count)
	}

	// Update and re-upsert
	pkgs[0].Downloads = 2000
	err = BatchUpsertPackages(ctx, database, pkgs[:1])
	if err != nil {
		t.Fatalf("second batch upsert: %v", err)
	}

	var downloads int64
	_ = database.QueryRow("SELECT downloads FROM packages WHERE name='akismet'").Scan(&downloads)
	if downloads != 2000 {
		t.Errorf("downloads = %d, want 2000", downloads)
	}

	// Empty batch should be no-op
	err = BatchUpsertPackages(ctx, database, nil)
	if err != nil {
		t.Fatalf("empty batch should not fail: %v", err)
	}
}

func TestAllocateSyncRunID(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	run1, err := AllocateSyncRunID(ctx, database)
	if err != nil {
		t.Fatalf("first alloc: %v", err)
	}
	if run1.RunID != 1 {
		t.Errorf("first run ID = %d, want 1", run1.RunID)
	}

	// Simulate a package with sync_run_id=1
	_, _ = database.Exec("INSERT INTO packages (type, name, versions_json, last_sync_run_id, created_at, updated_at) VALUES ('plugin', 'test', '{}', 1, datetime('now'), datetime('now'))")

	run2, err := AllocateSyncRunID(ctx, database)
	if err != nil {
		t.Fatalf("second alloc: %v", err)
	}
	if run2.RunID != 2 {
		t.Errorf("second run ID = %d, want 2", run2.RunID)
	}

	// Verify sync_runs rows
	var count int
	_ = database.QueryRow("SELECT COUNT(*) FROM sync_runs").Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 sync_runs rows, got %d", count)
	}

	// FinishSyncRun should work
	err = FinishSyncRun(ctx, database, run2.RowID, "completed", map[string]any{"updated": 5})
	if err != nil {
		t.Fatalf("finish: %v", err)
	}

	var status string
	_ = database.QueryRow("SELECT status FROM sync_runs WHERE id=?", run2.RowID).Scan(&status)
	if status != "completed" {
		t.Errorf("status = %q, want completed", status)
	}
}
