package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/roots/wp-packages/internal/db"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}

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
			rating REAL,
			num_ratings INTEGER NOT NULL DEFAULT 0,
			is_active INTEGER NOT NULL DEFAULT 1,
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
		)`)
	if err != nil {
		t.Fatalf("creating table: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestBuild(t *testing.T) {
	database := setupTestDB(t)

	// Insert test packages
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('plugin', 'akismet', 'Akismet',
			'{"5.0":"https://downloads.wordpress.org/plugin/akismet.5.0.zip","4.0":"https://downloads.wordpress.org/plugin/akismet.4.0.zip"}',
			1, 1, datetime('now'), datetime('now'))`)
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('theme', 'astra', 'Astra',
			'{"4.0":"https://downloads.wordpress.org/theme/astra.4.0.zip"}',
			1, 1, datetime('now'), datetime('now'))`)

	tmpDir := t.TempDir()

	result, err := Build(context.Background(), database, BuildOpts{
		OutputDir: tmpDir,
		AppURL:    "https://app.example.com",
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	if result.PackagesTotal != 2 {
		t.Errorf("packages_total = %d, want 2", result.PackagesTotal)
	}

	// Verify packages.json exists and is valid
	packagesPath := filepath.Join(result.BuildDir, "packages.json")
	data, err := os.ReadFile(packagesPath)
	if err != nil {
		t.Fatalf("packages.json missing: %v", err)
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("packages.json invalid: %v", err)
	}

	// Check notify-batch is absolute
	notifyBatch, ok := root["notify-batch"].(string)
	if !ok || notifyBatch != "https://app.example.com/downloads" {
		t.Errorf("notify-batch = %q, want https://app.example.com/downloads", notifyBatch)
	}

	// packages.json should NOT contain provider-includes or providers-url
	if _, ok := root["provider-includes"]; ok {
		t.Error("packages.json should not contain provider-includes")
	}
	if _, ok := root["providers-url"]; ok {
		t.Error("packages.json should not contain providers-url")
	}

	// Check metadata-url
	if root["metadata-url"] != "/p2/%package%.json" {
		t.Errorf("metadata-url = %q, want /p2/%%package%%.json", root["metadata-url"])
	}

	// Check p2 files exist
	for _, path := range []string{"p2/wp-plugin/akismet.json", "p2/wp-theme/astra.json"} {
		if _, err := os.Stat(filepath.Join(result.BuildDir, path)); err != nil {
			t.Errorf("p2 file missing: %s", path)
		}
	}

	// p/ directory should NOT exist
	if _, err := os.Stat(filepath.Join(result.BuildDir, "p")); !os.IsNotExist(err) {
		t.Error("p/ directory should not exist")
	}

	// Check manifest.json
	manifestData, err := os.ReadFile(filepath.Join(result.BuildDir, "manifest.json"))
	if err != nil {
		t.Fatal("manifest.json missing")
	}
	var manifest map[string]any
	_ = json.Unmarshal(manifestData, &manifest)
	if manifest["packages_total"].(float64) != 2 {
		t.Errorf("manifest packages_total = %v", manifest["packages_total"])
	}

	// Integrity validation should pass
	errors := ValidateIntegrity(result.BuildDir)
	if len(errors) > 0 {
		t.Errorf("integrity validation failed: %v", errors)
	}
}

func TestBuildParallelWrites(t *testing.T) {
	database := setupTestDB(t)

	// Insert many packages to exercise parallel writes
	for i := range 20 {
		slug := fmt.Sprintf("plugin-%d", i)
		_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
			VALUES ('plugin', ?, ?,
				'{"1.0":"https://downloads.wordpress.org/plugin/test.1.0.zip"}',
				1, 1, datetime('now'), datetime('now'))`, slug, slug)
	}

	tmpDir := t.TempDir()
	result, err := Build(context.Background(), database, BuildOpts{
		OutputDir: tmpDir,
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	if result.PackagesTotal != 20 {
		t.Errorf("packages_total = %d, want 20", result.PackagesTotal)
	}

	// Verify p2 files exist on disk
	for i := range 20 {
		slug := fmt.Sprintf("plugin-%d", i)
		p2Path := filepath.Join(result.BuildDir, "p2", "wp-plugin", slug+".json")
		if _, err := os.Stat(p2Path); err != nil {
			t.Errorf("p2 file missing: %s", p2Path)
		}
	}
}

func TestBuildDevTrunkSplit(t *testing.T) {
	database := setupTestDB(t)

	// Package with tagged versions
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('plugin', 'akismet', 'Akismet',
			'{"5.0":"https://downloads.wordpress.org/plugin/akismet.5.0.zip"}',
			1, 1, datetime('now'), datetime('now'))`)

	// Package with no tagged versions (trunk-only in SVN)
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('plugin', 'trunk-only', 'Trunk Only',
			'{}',
			1, 1, datetime('now'), datetime('now'))`)

	tmpDir := t.TempDir()
	result, err := Build(context.Background(), database, BuildOpts{
		OutputDir: tmpDir,
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	if result.PackagesTotal != 2 {
		t.Errorf("packages_total = %d, want 2", result.PackagesTotal)
	}

	// akismet should have both .json and ~dev.json
	for _, path := range []string{"p2/wp-plugin/akismet.json", "p2/wp-plugin/akismet~dev.json"} {
		if _, err := os.Stat(filepath.Join(result.BuildDir, path)); err != nil {
			t.Errorf("file missing: %s", path)
		}
	}

	// akismet.json should NOT contain dev-trunk
	data, _ := os.ReadFile(filepath.Join(result.BuildDir, "p2/wp-plugin/akismet.json"))
	var tagged map[string]any
	_ = json.Unmarshal(data, &tagged)
	pkgs := tagged["packages"].(map[string]any)
	versions := pkgs["wp-plugin/akismet"].(map[string]any)
	if _, ok := versions["dev-trunk"]; ok {
		t.Error("akismet.json should not contain dev-trunk")
	}
	if _, ok := versions["5.0"]; !ok {
		t.Error("akismet.json should contain version 5.0")
	}

	// akismet~dev.json should contain dev-trunk with source but no dist
	devData, _ := os.ReadFile(filepath.Join(result.BuildDir, "p2/wp-plugin/akismet~dev.json"))
	var dev map[string]any
	_ = json.Unmarshal(devData, &dev)
	devPkgs := dev["packages"].(map[string]any)
	devVersions := devPkgs["wp-plugin/akismet"].(map[string]any)
	devTrunk := devVersions["dev-trunk"].(map[string]any)
	if _, ok := devTrunk["source"]; !ok {
		t.Error("dev-trunk should have source")
	}
	if _, ok := devTrunk["dist"]; ok {
		t.Error("dev-trunk should not have dist (unversioned zip is not reproducible)")
	}
	source := devTrunk["source"].(map[string]any)
	if source["reference"] != "trunk" {
		t.Errorf("dev-trunk reference = %q, want trunk (no revision set)", source["reference"])
	}

	// trunk-only should have both .json (with dev-trunk) and ~dev.json
	for _, path := range []string{"p2/wp-plugin/trunk-only.json", "p2/wp-plugin/trunk-only~dev.json"} {
		if _, err := os.Stat(filepath.Join(result.BuildDir, path)); err != nil {
			t.Errorf("file missing: %s", path)
		}
	}

	// Integrity validation should pass
	errors := ValidateIntegrity(result.BuildDir)
	if len(errors) > 0 {
		t.Errorf("integrity validation failed: %v", errors)
	}
}

func TestBuildDevTrunkRevision(t *testing.T) {
	database := setupTestDB(t)

	// Plugin with trunk_revision set
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, trunk_revision, last_sync_run_id, created_at, updated_at)
		VALUES ('plugin', 'akismet', 'Akismet',
			'{"5.0":"https://downloads.wordpress.org/plugin/akismet.5.0.zip"}',
			1, 3470087, 1, datetime('now'), datetime('now'))`)

	// Plugin without trunk_revision (cold start)
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('plugin', 'no-rev', 'No Rev',
			'{"1.0":"https://downloads.wordpress.org/plugin/no-rev.1.0.zip"}',
			1, 1, datetime('now'), datetime('now'))`)

	tmpDir := t.TempDir()
	result, err := Build(context.Background(), database, BuildOpts{
		OutputDir: tmpDir,
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	if result.PackagesTotal != 2 {
		t.Errorf("packages_total = %d, want 2", result.PackagesTotal)
	}

	// akismet~dev.json should have trunk@3470087 reference
	devData, _ := os.ReadFile(filepath.Join(result.BuildDir, "p2/wp-plugin/akismet~dev.json"))
	var dev map[string]any
	_ = json.Unmarshal(devData, &dev)
	devPkgs := dev["packages"].(map[string]any)
	devVersions := devPkgs["wp-plugin/akismet"].(map[string]any)
	devTrunk := devVersions["dev-trunk"].(map[string]any)
	source := devTrunk["source"].(map[string]any)
	if source["reference"] != "trunk@3470087" {
		t.Errorf("reference = %q, want trunk@3470087", source["reference"])
	}
	if _, ok := devTrunk["dist"]; ok {
		t.Error("dev-trunk should not have dist")
	}

	// no-rev~dev.json should have plain "trunk" reference (cold start)
	noRevData, _ := os.ReadFile(filepath.Join(result.BuildDir, "p2/wp-plugin/no-rev~dev.json"))
	var noRev map[string]any
	_ = json.Unmarshal(noRevData, &noRev)
	noRevPkgs := noRev["packages"].(map[string]any)
	noRevVersions := noRevPkgs["wp-plugin/no-rev"].(map[string]any)
	noRevTrunk := noRevVersions["dev-trunk"].(map[string]any)
	noRevSource := noRevTrunk["source"].(map[string]any)
	if noRevSource["reference"] != "trunk" {
		t.Errorf("reference = %q, want trunk (no revision)", noRevSource["reference"])
	}
}

func TestBuildDeleteDetectionWithDevFiles(t *testing.T) {
	database := setupTestDB(t)
	tmpDir := t.TempDir()

	// First build: one package with tagged + dev, one trunk-only
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('plugin', 'akismet', 'Akismet',
			'{"5.0":"https://downloads.wordpress.org/plugin/akismet.5.0.zip","dev-trunk":"https://downloads.wordpress.org/plugin/akismet.zip"}',
			1, 1, datetime('now'), datetime('now'))`)
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('plugin', 'removed-plugin', 'Removed',
			'{"1.0":"https://downloads.wordpress.org/plugin/removed-plugin.1.0.zip","dev-trunk":"https://downloads.wordpress.org/plugin/removed-plugin.zip"}',
			1, 1, datetime('now'), datetime('now'))`)

	first, err := Build(context.Background(), database, BuildOpts{
		OutputDir: tmpDir,
		BuildID:   "build-1",
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("first build failed: %v", err)
	}

	// Deactivate removed-plugin before second build
	_, _ = database.Exec(`UPDATE packages SET is_active = 0 WHERE name = 'removed-plugin'`)

	second, err := Build(context.Background(), database, BuildOpts{
		OutputDir:        tmpDir,
		BuildID:          "build-2",
		Logger:           slog.Default(),
		PreviousBuildDir: first.BuildDir,
	})
	if err != nil {
		t.Fatalf("second build failed: %v", err)
	}

	// Should have exactly one delete for wp-plugin/removed-plugin (not a duplicate, not ~dev suffix)
	var deletes []PackageChange
	for _, c := range second.ChangedPackages {
		if c.Action == "delete" {
			deletes = append(deletes, c)
		}
	}
	if len(deletes) != 1 {
		t.Errorf("expected 1 delete, got %d: %v", len(deletes), deletes)
	}
	if len(deletes) == 1 && deletes[0].Name != "wp-plugin/removed-plugin" {
		t.Errorf("delete name = %q, want wp-plugin/removed-plugin", deletes[0].Name)
	}
}

func TestBuildDeleteTrunkOnlyPackage(t *testing.T) {
	database := setupTestDB(t)
	tmpDir := t.TempDir()

	// Trunk-only package (only ~dev.json, no .json)
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('plugin', 'trunk-pkg', 'Trunk',
			'{"dev-trunk":"https://downloads.wordpress.org/plugin/trunk-pkg.zip"}',
			1, 1, datetime('now'), datetime('now'))`)

	first, err := Build(context.Background(), database, BuildOpts{
		OutputDir: tmpDir,
		BuildID:   "build-1",
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("first build failed: %v", err)
	}

	// Remove trunk-only package
	_, _ = database.Exec(`UPDATE packages SET is_active = 0 WHERE name = 'trunk-pkg'`)

	second, err := Build(context.Background(), database, BuildOpts{
		OutputDir:        tmpDir,
		BuildID:          "build-2",
		Logger:           slog.Default(),
		PreviousBuildDir: first.BuildDir,
	})
	if err != nil {
		t.Fatalf("second build failed: %v", err)
	}

	var deletes []PackageChange
	for _, c := range second.ChangedPackages {
		if c.Action == "delete" {
			deletes = append(deletes, c)
		}
	}
	if len(deletes) != 1 {
		t.Fatalf("expected 1 delete, got %d: %v", len(deletes), deletes)
	}
	if deletes[0].Name != "wp-plugin/trunk-pkg" {
		t.Errorf("delete name = %q, want wp-plugin/trunk-pkg", deletes[0].Name)
	}
}

func TestBuildTaggedToDevOnlyNoFalseDelete(t *testing.T) {
	database := setupTestDB(t)
	tmpDir := t.TempDir()

	// Package starts with tagged + dev versions
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('plugin', 'migrating', 'Migrating',
			'{"1.0":"https://downloads.wordpress.org/plugin/migrating.1.0.zip","dev-trunk":"https://downloads.wordpress.org/plugin/migrating.zip"}',
			1, 1, datetime('now'), datetime('now'))`)

	first, err := Build(context.Background(), database, BuildOpts{
		OutputDir: tmpDir,
		BuildID:   "build-1",
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("first build failed: %v", err)
	}

	// Transition to dev-only (tagged versions removed, e.g. author untagged)
	_, _ = database.Exec(`UPDATE packages SET versions_json = '{"dev-trunk":"https://downloads.wordpress.org/plugin/migrating.zip"}' WHERE name = 'migrating'`)

	second, err := Build(context.Background(), database, BuildOpts{
		OutputDir:        tmpDir,
		BuildID:          "build-2",
		Logger:           slog.Default(),
		PreviousBuildDir: first.BuildDir,
	})
	if err != nil {
		t.Fatalf("second build failed: %v", err)
	}

	// Should NOT emit a delete — the package still exists via ~dev.json
	for _, c := range second.ChangedPackages {
		if c.Action == "delete" {
			t.Errorf("unexpected delete for %s — package still exists as dev-only", c.Name)
		}
	}
}

func TestBuildDevOnlyChangeDetection(t *testing.T) {
	database := setupTestDB(t)
	tmpDir := t.TempDir()

	// First build
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('plugin', 'mixed', 'Mixed',
			'{"1.0":"https://downloads.wordpress.org/plugin/mixed.1.0.zip","dev-trunk":"https://downloads.wordpress.org/plugin/mixed.zip"}',
			1, 1, datetime('now'), datetime('now'))`)

	first, err := Build(context.Background(), database, BuildOpts{
		OutputDir: tmpDir,
		BuildID:   "build-1",
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("first build failed: %v", err)
	}

	// Second build with same data — nothing changed
	second, err := Build(context.Background(), database, BuildOpts{
		OutputDir:        tmpDir,
		BuildID:          "build-2",
		Logger:           slog.Default(),
		PreviousBuildDir: first.BuildDir,
	})
	if err != nil {
		t.Fatalf("second build failed: %v", err)
	}
	if second.PackagesChanged != 0 {
		t.Errorf("expected 0 changes, got %d", second.PackagesChanged)
	}

	// Third build: change only the dev version (simulate trunk update via last_committed)
	_, _ = database.Exec(`UPDATE packages SET last_committed = '2099-01-01T00:00:00Z' WHERE name = 'mixed'`)

	third, err := Build(context.Background(), database, BuildOpts{
		OutputDir:        tmpDir,
		BuildID:          "build-3",
		Logger:           slog.Default(),
		PreviousBuildDir: second.BuildDir,
	})
	if err != nil {
		t.Fatalf("third build failed: %v", err)
	}

	// The dev file changed (different "time" field), so package should be marked changed
	if third.PackagesChanged != 1 {
		t.Errorf("expected 1 change, got %d", third.PackagesChanged)
	}

	// Should not be duplicated
	updateCount := 0
	for _, c := range third.ChangedPackages {
		if c.Name == "wp-plugin/mixed" && c.Action == "update" {
			updateCount++
		}
	}
	if updateCount != 1 {
		t.Errorf("expected 1 update entry for wp-plugin/mixed, got %d", updateCount)
	}
}

func TestBuildThemeNoTaggedVersions(t *testing.T) {
	database := setupTestDB(t)

	// Theme with no tagged versions — should be skipped entirely
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('theme', 'empty-theme', 'Empty Theme',
			'{}',
			1, 1, datetime('now'), datetime('now'))`)

	// Plugin with no tagged versions — should still get .json and ~dev.json
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('plugin', 'empty-plugin', 'Empty Plugin',
			'{}',
			1, 1, datetime('now'), datetime('now'))`)

	// Theme with tagged versions — should work normally (no ~dev.json)
	_, _ = database.Exec(`INSERT INTO packages (type, name, display_name, versions_json, is_active, last_sync_run_id, created_at, updated_at)
		VALUES ('theme', 'astra', 'Astra',
			'{"4.0":"https://downloads.wordpress.org/theme/astra.4.0.zip"}',
			1, 1, datetime('now'), datetime('now'))`)

	tmpDir := t.TempDir()
	result, err := Build(context.Background(), database, BuildOpts{
		OutputDir: tmpDir,
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	// Only plugin and astra should be counted (empty theme skipped)
	if result.PackagesTotal != 2 {
		t.Errorf("packages_total = %d, want 2", result.PackagesTotal)
	}

	// Empty theme should have no files
	if _, err := os.Stat(filepath.Join(result.BuildDir, "p2/wp-theme/empty-theme.json")); !os.IsNotExist(err) {
		t.Error("empty-theme.json should not exist")
	}
	if _, err := os.Stat(filepath.Join(result.BuildDir, "p2/wp-theme/empty-theme~dev.json")); !os.IsNotExist(err) {
		t.Error("empty-theme~dev.json should not exist")
	}

	// Astra should have .json but no ~dev.json
	if _, err := os.Stat(filepath.Join(result.BuildDir, "p2/wp-theme/astra.json")); err != nil {
		t.Errorf("astra.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(result.BuildDir, "p2/wp-theme/astra~dev.json")); !os.IsNotExist(err) {
		t.Error("astra~dev.json should not exist")
	}

	// Empty plugin should have both .json (with dev-trunk) and ~dev.json
	if _, err := os.Stat(filepath.Join(result.BuildDir, "p2/wp-plugin/empty-plugin.json")); err != nil {
		t.Errorf("empty-plugin.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(result.BuildDir, "p2/wp-plugin/empty-plugin~dev.json")); err != nil {
		t.Errorf("empty-plugin~dev.json missing: %v", err)
	}

	// Integrity should pass
	errors := ValidateIntegrity(result.BuildDir)
	if len(errors) > 0 {
		t.Errorf("integrity validation failed: %v", errors)
	}
}

func TestBuildEmpty(t *testing.T) {
	database := setupTestDB(t)
	tmpDir := t.TempDir()

	result, err := Build(context.Background(), database, BuildOpts{
		OutputDir: tmpDir,
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if result.PackagesTotal != 0 {
		t.Errorf("expected 0 packages, got %d", result.PackagesTotal)
	}
}
