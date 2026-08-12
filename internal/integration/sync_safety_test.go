//go:build integration

package integration

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/roots/wp-packages/internal/composer"
	"github.com/roots/wp-packages/internal/config"
	"github.com/roots/wp-packages/internal/deploy"
	"github.com/roots/wp-packages/internal/packages"
	"github.com/roots/wp-packages/internal/testutil"
	"github.com/roots/wp-packages/internal/wporg"
)

// syncFixture spins up a seeded DB and an in-process S3 fake, synced to a clean state.
func syncFixture(t *testing.T) (*sql.DB, config.R2Config, *s3.Client) {
	t.Helper()
	ctx := context.Background()

	fixtureDir := filepath.Join("..", "wporg", "testdata")
	mock := wporg.NewMockServer(fixtureDir)
	t.Cleanup(mock.Close)

	db := testutil.OpenTestDB(t)
	testutil.SeedFromFixtures(t, db, mock.URL)
	rehashAll(t, db)

	backend := s3mem.New()
	faker := gofakes3.New(backend)
	ts := httptest.NewServer(faker.Server())
	t.Cleanup(ts.Close)

	client := newTestS3Client(ts.URL)
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("test-bucket"),
	}); err != nil {
		t.Fatalf("creating bucket: %v", err)
	}

	cfg := config.R2Config{
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		Bucket:          "test-bucket",
		Endpoint:        ts.URL,
		Concurrency:     1,
	}

	if _, err := deploy.Sync(ctx, db, cfg, "http://test.local", deploy.SyncOptions{}, testLogger(t)); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	return db, cfg, client
}

// rehashAll mirrors the `wppackages rehash` command.
func rehashAll(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	pkgs, err := packages.GetAllActiveForHashing(ctx, db)
	if err != nil {
		t.Fatalf("loading packages for rehash: %v", err)
	}
	for _, p := range pkgs {
		hash, err := composer.HashContent(p.Type, p.Name, p.VersionsJSON, p.ComposerMeta())
		if err != nil {
			t.Fatalf("hashing %s/%s: %v", p.Type, p.Name, err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE packages SET content_hash = ? WHERE id = ?`, hash, p.ID); err != nil {
			t.Fatalf("storing hash for %s/%s: %v", p.Type, p.Name, err)
		}
	}
}

// TestMetadataOnlyChangeTriggersReupload is the end-to-end form of the Risk 1
// regression: a package whose description changes but whose versions do not must
// still be re-uploaded. Under the old input-based hash this sync was a no-op and
// R2 kept serving the stale description indefinitely.
func TestMetadataOnlyChangeTriggersReupload(t *testing.T) {
	ctx := context.Background()
	db, cfg, _ := syncFixture(t)

	// Confirm the starting state really is clean.
	clean, err := deploy.Sync(ctx, db, cfg, "http://test.local", deploy.SyncOptions{}, testLogger(t))
	if err != nil {
		t.Fatalf("idempotent sync: %v", err)
	}
	if clean.Uploaded != 0 {
		t.Fatalf("expected a clean starting state, got %d uploads", clean.Uploaded)
	}

	// Change only the description — no version, no trunk_revision.
	if _, err := db.ExecContext(ctx,
		`UPDATE packages SET description = 'a new description'
		 WHERE type = 'plugin' AND name = 'akismet'`); err != nil {
		t.Fatalf("updating description: %v", err)
	}
	rehashAll(t, db)

	result, err := deploy.Sync(ctx, db, cfg, "http://test.local", deploy.SyncOptions{}, testLogger(t))
	if err != nil {
		t.Fatalf("sync after metadata change: %v", err)
	}
	if result.Uploaded == 0 {
		t.Error("metadata-only change did not trigger a re-upload — R2 would serve stale metadata")
	}
}

// TestMassDeletionIsRefused covers Risk 3: a wp.org degradation that mass-deactivates
// packages must not cascade into mass R2 deletion.
func TestMassDeletionIsRefused(t *testing.T) {
	ctx := context.Background()
	db, cfg, client := syncFixture(t)

	if _, err := db.ExecContext(ctx, `UPDATE packages SET is_active = 0`); err != nil {
		t.Fatalf("deactivating all packages: %v", err)
	}

	result, err := deploy.Sync(ctx, db, cfg, "http://test.local",
		deploy.SyncOptions{MaxDeletes: 2}, testLogger(t))
	if err != nil {
		t.Fatalf("sync should not fail when refusing deletes: %v", err)
	}
	if result.Deleted != 0 {
		t.Errorf("expected 0 deletions when over the limit, got %d", result.Deleted)
	}
	if result.DeletesSkipped == 0 {
		t.Error("expected DeletesSkipped to report the refusal")
	}

	// The files must still be on R2.
	if _, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("p2/wp-plugin/akismet.json"),
	}); err != nil {
		t.Errorf("akismet.json was deleted despite the guard: %v", err)
	}

	// Raising the limit lets the same deletions through.
	allowed, err := deploy.Sync(ctx, db, cfg, "http://test.local",
		deploy.SyncOptions{MaxDeletes: -1}, testLogger(t))
	if err != nil {
		t.Fatalf("sync with deletes allowed: %v", err)
	}
	if allowed.Deleted == 0 {
		t.Error("expected deletions once the limit was lifted")
	}
}

// TestDryRunTouchesNothing covers the shadow-mode step of the cutover: --dry-run
// must report work without mutating R2 or the database.
func TestDryRunTouchesNothing(t *testing.T) {
	ctx := context.Background()
	db, cfg, client := syncFixture(t)

	if _, err := db.ExecContext(ctx,
		`UPDATE packages SET description = 'changed' WHERE type = 'plugin' AND name = 'akismet'`); err != nil {
		t.Fatalf("updating description: %v", err)
	}
	rehashAll(t, db)

	before := dirtyCount(t, db)
	if before == 0 {
		t.Fatal("expected a dirty package to dry-run against")
	}

	result, err := deploy.Sync(ctx, db, cfg, "http://test.local",
		deploy.SyncOptions{DryRun: true}, testLogger(t))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if result.Uploaded == 0 {
		t.Error("dry run reported no uploads despite a dirty package")
	}

	if after := dirtyCount(t, db); after != before {
		t.Errorf("dry run stamped deployed_hash: dirty went %d → %d", before, after)
	}

	// The stale object must still be the one on R2.
	obj, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("p2/wp-plugin/akismet.json"),
	})
	if err != nil {
		t.Fatalf("reading akismet.json: %v", err)
	}
	defer func() { _ = obj.Body.Close() }()
}

// TestUnhashedPackagesAreReported covers Risk 2: rows with a NULL content_hash are
// excluded from the diff query, so the sync step must surface them rather than
// silently reporting nothing to do.
func TestUnhashedPackagesAreReported(t *testing.T) {
	ctx := context.Background()
	db, cfg, _ := syncFixture(t)

	if _, err := db.ExecContext(ctx,
		`UPDATE packages SET content_hash = NULL WHERE type = 'plugin' AND name = 'akismet'`); err != nil {
		t.Fatalf("clearing content_hash: %v", err)
	}

	result, err := deploy.Sync(ctx, db, cfg, "http://test.local", deploy.SyncOptions{}, testLogger(t))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Unhashed != 1 {
		t.Errorf("expected 1 unhashed active package to be reported, got %d", result.Unhashed)
	}
	if result.Uploaded != 0 {
		t.Errorf("expected the unhashed package to be skipped, got %d uploads", result.Uploaded)
	}

	// rehash must bring it back into the diff query.
	rehashAll(t, db)
	after, err := deploy.Sync(ctx, db, cfg, "http://test.local", deploy.SyncOptions{}, testLogger(t))
	if err != nil {
		t.Fatalf("sync after rehash: %v", err)
	}
	if after.Unhashed != 0 {
		t.Errorf("rehash left %d unhashed packages", after.Unhashed)
	}
}

func dirtyCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM packages WHERE is_active = 1
		AND content_hash IS NOT NULL
		AND (deployed_hash IS NULL OR content_hash != deployed_hash)`).Scan(&n)
	if err != nil {
		t.Fatalf("counting dirty packages: %v", err)
	}
	return n
}
