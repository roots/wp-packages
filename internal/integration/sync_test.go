//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/roots/wp-packages/internal/config"
	"github.com/roots/wp-packages/internal/deploy"
	"github.com/roots/wp-packages/internal/repository"
	"github.com/roots/wp-packages/internal/testutil"
	"github.com/roots/wp-packages/internal/wporg"
)

func TestR2Sync(t *testing.T) {
	ctx := context.Background()

	// 1. Seed DB from fixtures
	fixtureDir := filepath.Join("..", "wporg", "testdata")
	mock := wporg.NewMockServer(fixtureDir)
	defer mock.Close()

	db := testutil.OpenTestDB(t)
	testutil.SeedFromFixtures(t, db, mock.URL)

	// 2. Build artifacts
	buildOutputDir := filepath.Join(t.TempDir(), "builds")
	result, err := repository.Build(ctx, db, repository.BuildOpts{
		OutputDir: buildOutputDir,
		AppURL:    "http://test.local",
		Force:     true,
		Logger:    testLogger(t),
	})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	buildDir := filepath.Join(buildOutputDir, result.BuildID)

	// 3. Start gofakes3 in-process
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	ts := httptest.NewServer(faker.Server())
	defer ts.Close()

	// Create the bucket
	s3Client := newTestS3Client(ts.URL)
	_, err = s3Client.CreateBucket(ctx, &s3.CreateBucketInput{
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

	// 4. First sync — all packages uploaded
	err = deploy.SyncToR2(ctx, db, r2Cfg, buildDir, result.BuildID, "", testLogger(t))
	if err != nil {
		t.Fatalf("first sync failed: %v", err)
	}

	// Verify packages.json exists in bucket
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
	// packages.json should have metadata-url but no v1 fields
	if _, ok := rootJSON["metadata-url"]; !ok {
		t.Error("packages.json missing metadata-url")
	}
	if _, ok := rootJSON["provider-includes"]; ok {
		t.Error("packages.json should not contain provider-includes")
	}
	if _, ok := rootJSON["providers-url"]; ok {
		t.Error("packages.json should not contain providers-url")
	}

	// Verify p2/ files exist
	p2Key := "p2/wp-plugin/akismet.json"
	p2Obj, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(p2Key),
	})
	if err != nil {
		t.Fatalf("p2 file %s not found: %v", p2Key, err)
	}
	_ = p2Obj.Body.Close()

	// Verify p/ content-addressed files do NOT exist
	listResp, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String("test-bucket"),
		Prefix: aws.String("p/"),
	})
	if err != nil {
		t.Fatalf("listing p/ objects: %v", err)
	}
	if len(listResp.Contents) > 0 {
		t.Errorf("expected no p/ files after v1 removal, found %d", len(listResp.Contents))
	}

	// Count total uploaded objects for the idempotency check
	allResp, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String("test-bucket"),
	})
	if err != nil {
		t.Fatalf("listing all objects: %v", err)
	}
	firstSyncCount := len(allResp.Contents)
	t.Logf("first sync: %d objects in bucket", firstSyncCount)

	// 5. Second sync — same build, nothing should change
	// (pass buildDir as previousBuildDir so unchanged files are skipped)
	err = deploy.SyncToR2(ctx, db, r2Cfg, buildDir, result.BuildID, buildDir, testLogger(t))
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}

	// Object count should be the same (idempotent)
	allResp2, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String("test-bucket"),
	})
	if err != nil {
		t.Fatalf("listing all objects after second sync: %v", err)
	}
	secondSyncCount := len(allResp2.Contents)
	if secondSyncCount != firstSyncCount {
		t.Errorf("second sync changed object count: %d -> %d", firstSyncCount, secondSyncCount)
	}
}

func newTestS3Client(endpoint string) *s3.Client {
	return s3.New(s3.Options{
		Region: "auto",
		Credentials: credentials.NewStaticCredentialsProvider(
			"test", "test", "",
		),
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
	})
}
