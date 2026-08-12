//go:build integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
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

// backfillContentHashes computes content_hash for all packages that don't have one.
func backfillContentHashes(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx := context.Background()

	pkgs, err := packages.GetAllActiveForHashing(ctx, database)
	if err != nil {
		t.Fatalf("getting packages for hash backfill: %v", err)
	}

	for _, p := range pkgs {
		hash, err := composer.HashContent(p.Type, p.Name, p.VersionsJSON, p.ComposerMeta())
		if err != nil {
			t.Fatalf("hashing %s/%s: %v", p.Type, p.Name, err)
		}
		_, err = database.ExecContext(ctx,
			`UPDATE packages SET content_hash = ? WHERE id = ?`, hash, p.ID)
		if err != nil {
			t.Fatalf("backfilling hash for %s/%s: %v", p.Type, p.Name, err)
		}
	}
}

func TestDBDrivenSync(t *testing.T) {
	ctx := context.Background()

	// 1. Seed DB from fixtures
	fixtureDir := filepath.Join("..", "wporg", "testdata")
	mock := wporg.NewMockServer(fixtureDir)
	defer mock.Close()

	db := testutil.OpenTestDB(t)
	testutil.SeedFromFixtures(t, db, mock.URL)
	backfillContentHashes(t, db)

	// 2. Start gofakes3 in-process
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	ts := httptest.NewServer(faker.Server())
	defer ts.Close()

	s3Client := newTestS3Client(ts.URL)
	_, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("test-bucket"),
	})
	if err != nil {
		t.Fatalf("creating bucket: %v", err)
	}

	r2Cfg := config.R2Config{
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		Bucket:          "test-bucket",
		Endpoint:        ts.URL,
		Concurrency:     1,
	}

	// 3. First sync — all packages should be uploaded
	result, err := deploy.Sync(ctx, db, r2Cfg, "http://test.local", deploy.SyncOptions{}, testLogger(t))
	if err != nil {
		t.Fatalf("first sync failed: %v", err)
	}
	if result.Uploaded == 0 {
		t.Error("expected uploads on first sync")
	}
	t.Logf("first sync: uploaded=%d deleted=%d", result.Uploaded, result.Deleted)

	// Verify packages.json exists and is valid
	rootObj, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("packages.json"),
	})
	if err != nil {
		t.Fatalf("packages.json not found after sync: %v", err)
	}
	rootData, _ := io.ReadAll(rootObj.Body)
	_ = rootObj.Body.Close()

	var rootJSON map[string]any
	if err := json.Unmarshal(rootData, &rootJSON); err != nil {
		t.Fatalf("invalid packages.json: %v", err)
	}
	if _, ok := rootJSON["metadata-url"]; !ok {
		t.Error("packages.json missing metadata-url")
	}

	// Verify p2/ files exist for a known plugin
	p2Key := "p2/wp-plugin/akismet.json"
	p2Obj, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(p2Key),
	})
	if err != nil {
		t.Fatalf("p2 file %s not found: %v", p2Key, err)
	}
	p2Data, _ := io.ReadAll(p2Obj.Body)
	_ = p2Obj.Body.Close()

	// Verify p2 content is valid Composer JSON
	var p2JSON map[string]any
	if err := json.Unmarshal(p2Data, &p2JSON); err != nil {
		t.Fatalf("invalid p2 JSON for %s: %v", p2Key, err)
	}
	pkgsField, ok := p2JSON["packages"].(map[string]any)
	if !ok {
		t.Fatalf("p2 file missing 'packages' key")
	}
	if _, ok := pkgsField["wp-plugin/akismet"]; !ok {
		t.Error("p2 file missing wp-plugin/akismet entry")
	}

	// Verify ~dev.json exists for plugins
	devKey := "p2/wp-plugin/akismet~dev.json"
	devObj, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(devKey),
	})
	if err != nil {
		t.Fatalf("dev file %s not found: %v", devKey, err)
	}
	_ = devObj.Body.Close()

	// Verify deployed_hash is stamped (no dirty packages remain)
	var dirtyCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM packages WHERE is_active = 1
			AND content_hash IS NOT NULL
			AND (deployed_hash IS NULL OR content_hash != deployed_hash)`).Scan(&dirtyCount)
	if err != nil {
		t.Fatalf("counting dirty packages: %v", err)
	}
	if dirtyCount != 0 {
		t.Errorf("expected 0 dirty packages after sync, got %d", dirtyCount)
	}

	// 4. Second sync — idempotent, nothing to upload
	result2, err := deploy.Sync(ctx, db, r2Cfg, "http://test.local", deploy.SyncOptions{}, testLogger(t))
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if result2.Uploaded != 0 {
		t.Errorf("expected 0 uploads on idempotent sync, got %d", result2.Uploaded)
	}

	// 5. Deactivate a package, sync again, verify deletion
	var akismetID int64
	err = db.QueryRowContext(ctx,
		`SELECT id FROM packages WHERE type='plugin' AND name='akismet'`).Scan(&akismetID)
	if err != nil {
		t.Fatalf("finding akismet: %v", err)
	}

	if err := packages.DeactivatePackage(ctx, db, akismetID); err != nil {
		t.Fatalf("deactivating akismet: %v", err)
	}

	result3, err := deploy.Sync(ctx, db, r2Cfg, "http://test.local", deploy.SyncOptions{}, testLogger(t))
	if err != nil {
		t.Fatalf("sync after deactivation failed: %v", err)
	}
	if result3.Deleted == 0 {
		t.Error("expected deletions after deactivating akismet")
	}

	// Verify akismet p2 files are gone from R2
	_, err = s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(p2Key),
	})
	if err == nil {
		t.Error("akismet.json should have been deleted from R2")
	}

	// Verify deployed_hash is cleared for deactivated package
	var deployedHash *string
	err = db.QueryRowContext(ctx,
		`SELECT deployed_hash FROM packages WHERE id = ?`, akismetID).Scan(&deployedHash)
	if err != nil {
		t.Fatalf("checking deployed_hash: %v", err)
	}
	if deployedHash != nil {
		t.Error("deployed_hash should be NULL for deactivated package")
	}
}
