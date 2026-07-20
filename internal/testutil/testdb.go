package testutil

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"
	"time"

	wppackagesgo "github.com/roots/wp-packages"
	"github.com/roots/wp-packages/internal/config"
	"github.com/roots/wp-packages/internal/db"
	"github.com/roots/wp-packages/internal/packages"
	"github.com/roots/wp-packages/internal/wporg"
)

// OpenTestDB opens an in-memory SQLite database and runs all migrations.
func OpenTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := db.Migrate(database, wppackagesgo.Migrations); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	return database
}

// SeedFromFixtures runs the discover + update pipeline against a mock wp.org
// server, populating the database with package data derived from fixtures.
func SeedFromFixtures(t *testing.T, database *sql.DB, mockURL string) {
	t.Helper()
	ctx := context.Background()

	cfg := config.DiscoveryConfig{
		APITimeoutS:  5,
		MaxRetries:   1,
		RetryDelayMs: 10,
		Concurrency:  2,
	}
	client := wporg.NewClient(cfg, slog.Default())
	client.SetBaseURL(mockURL)

	// Discover: fetch last_updated for each fixture slug and create shell records
	type seed struct {
		slug    string
		pkgType string
	}
	seeds := []seed{
		{"akismet", "plugin"},
		{"classic-editor", "plugin"},
		{"contact-form-7", "plugin"},
		{"astra", "theme"},
		{"twentytwentyfive", "theme"},
	}

	for _, s := range seeds {
		lastUpdated, err := client.FetchLastUpdated(ctx, s.pkgType, s.slug)
		if err != nil {
			t.Fatalf("fetching last_updated for %s: %v", s.slug, err)
		}
		if err := packages.UpsertShellPackage(ctx, database, s.pkgType, s.slug, lastUpdated); err != nil {
			t.Fatalf("upserting shell package %s: %v", s.slug, err)
		}
	}

	// Update: fetch full metadata and upsert
	syncRun, err := packages.AllocateSyncRunID(ctx, database)
	if err != nil {
		t.Fatalf("allocating sync run: %v", err)
	}

	pkgs, err := packages.GetPackagesNeedingUpdate(ctx, database, packages.UpdateQueryOpts{
		Force: true,
		Type:  "all",
	})
	if err != nil {
		t.Fatalf("getting packages needing update: %v", err)
	}

	for _, p := range pkgs {
		var data map[string]any
		var fetchErr error
		if p.Type == "plugin" {
			data, fetchErr = client.FetchPlugin(ctx, p.Name)
		} else {
			data, fetchErr = client.FetchTheme(ctx, p.Name)
		}
		if fetchErr != nil {
			t.Fatalf("fetching %s/%s: %v", p.Type, p.Name, fetchErr)
		}

		pkg := packages.PackageFromAPIData(data, p.Type)
		pkg.ID = p.ID
		if _, _, err := pkg.NormalizeAndStoreVersions(); err != nil {
			t.Fatalf("normalizing versions for %s: %v", p.Name, err)
		}
		now := time.Now().UTC()
		pkg.LastSyncedAt = &now
		pkg.LastSyncRunID = &syncRun.RunID

		if err := packages.UpsertPackage(ctx, database, pkg); err != nil {
			t.Fatalf("upserting package %s: %v", p.Name, err)
		}
	}

	if err := packages.FinishSyncRun(ctx, database, syncRun.RowID, "completed", map[string]any{"updated": len(pkgs)}); err != nil {
		t.Fatalf("finishing sync run: %v", err)
	}
}
