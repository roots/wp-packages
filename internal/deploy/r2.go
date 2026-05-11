package deploy

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/roots/wp-packages/internal/composer"
	"github.com/roots/wp-packages/internal/config"
	"github.com/roots/wp-packages/internal/packages"
)

const (
	r2MaxRetries  = 3
	r2RetryBaseMs = 1000
	r2IndexFile   = "packages.json"
)

// SyncToR2 uploads build files to R2. Only p2/ files and packages.json are uploaded.
// p2/ files are skipped if unchanged from the previous build (byte-compared locally).
// packages.json is uploaded last.
//
// After uploads complete, deployed_hash is stamped for active packages (so we know
// the file is on R2), and any inactive packages whose deployed_hash is still set
// get their R2 files deleted. deployed_hash is only cleared per-package on full
// delete success, so transient failures are retried on the next sync.
func SyncToR2(ctx context.Context, db *sql.DB, cfg config.R2Config, buildDir, buildID, previousBuildDir string, logger *slog.Logger) error {
	client := newS3Client(cfg)

	// Collect file paths only (not data) to avoid loading everything into memory.
	var filePaths []string
	err := filepath.Walk(buildDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(buildDir, path)
		if err != nil {
			return err
		}
		relPath := strings.ReplaceAll(rel, string(os.PathSeparator), "/")
		// Only upload p2/ files and packages.json
		if strings.HasPrefix(relPath, "p2/") || relPath == r2IndexFile {
			filePaths = append(filePaths, relPath)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking build files: %w", err)
	}

	total := len(filePaths)

	// Upload p2/ files in parallel, packages.json last.
	var uploaded, skipped atomic.Int64
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(cfg.Concurrency)

	for _, relPath := range filePaths {
		relPath := relPath
		if relPath == r2IndexFile {
			continue // upload last
		}
		g.Go(func() error {
			// Skip unchanged p2/ files
			if previousBuildDir != "" && fileUnchanged(previousBuildDir, buildDir, relPath) {
				skipped.Add(1)
				return nil
			}
			data, err := os.ReadFile(filepath.Join(buildDir, relPath))
			if err != nil {
				return fmt.Errorf("reading %s: %w", relPath, err)
			}
			if err := putObjectWithRetry(gCtx, client, cfg.Bucket, relPath, data, logger); err != nil {
				return fmt.Errorf("R2 sync: %w", err)
			}
			n := uploaded.Add(1)
			if (n+skipped.Load())%500 == 0 {
				logger.Info("R2 upload progress", "uploaded", n, "skipped", skipped.Load(), "total", total)
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	// Stamp deployed_hash for active packages so we can detect deactivated
	// packages whose files still need to be removed from R2. Use content_hash
	// when available, falling back to a sentinel since not all rows have one.
	if _, err := db.ExecContext(ctx, `
		UPDATE packages SET deployed_hash = COALESCE(content_hash, '1')
		WHERE is_active = 1
			AND (deployed_hash IS NULL
				OR (content_hash IS NOT NULL AND deployed_hash != content_hash))`); err != nil {
		return fmt.Errorf("stamping deployed_hash: %w", err)
	}

	// Delete R2 files for deactivated packages.
	deactivated, err := packages.GetDeactivatedDeployedPackages(ctx, db)
	if err != nil {
		return fmt.Errorf("querying deactivated packages: %w", err)
	}

	var deletedCount, deleteErrors int
	for _, p := range deactivated {
		allOK := true
		for _, key := range composer.ObjectKeys(p.Type, p.Name) {
			if err := deleteObjectWithRetry(ctx, client, cfg.Bucket, key, logger); err != nil {
				logger.Warn("R2 sync: failed to delete deactivated package file", "key", key, "error", err)
				allOK = false
			}
		}
		if !allOK {
			deleteErrors++
			continue
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE packages SET deployed_hash = NULL WHERE id = ?`, p.ID); err != nil {
			logger.Warn("R2 sync: failed to clear deployed_hash", "package", p.Name, "error", err)
			continue
		}
		deletedCount++
		logger.Info("R2 sync: deleted deactivated package", "type", p.Type, "name", p.Name)
	}
	if deletedCount > 0 || deleteErrors > 0 {
		logger.Info("R2 sync: deactivated package cleanup",
			"deleted", deletedCount, "errors", deleteErrors, "candidates", len(deactivated))
	}

	// Upload packages.json last.
	packagesData, err := os.ReadFile(filepath.Join(buildDir, r2IndexFile))
	if err != nil {
		return fmt.Errorf("R2 sync: reading packages.json: %w", err)
	}
	if err := putObjectWithRetry(ctx, client, cfg.Bucket, r2IndexFile, packagesData, logger); err != nil {
		return fmt.Errorf("R2 sync (root packages.json): %w", err)
	}

	logger.Info("R2 sync complete", "uploaded", uploaded.Load(), "skipped", skipped.Load())
	return nil
}

// withRetry executes fn up to r2MaxRetries times with exponential backoff.
// The label is used in log messages to identify the operation.
func withRetry(ctx context.Context, logger *slog.Logger, label string, fn func() error) error {
	var lastErr error
	for attempt := range r2MaxRetries {
		if attempt > 0 {
			delay := time.Duration(float64(r2RetryBaseMs)*math.Pow(2, float64(attempt-1))) * time.Millisecond
			logger.Warn("retrying R2 operation", "op", label, "attempt", attempt+1, "delay", delay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
	}
	return fmt.Errorf("%s after %d attempts: %w", label, r2MaxRetries, lastErr)
}

// putObjectWithRetry uploads a single file to R2 with exponential backoff retry.
func putObjectWithRetry(ctx context.Context, client *s3.Client, bucket, key string, data []byte, logger *slog.Logger) error {
	contentType := "application/json"
	cacheControl := CacheControlForPath(key)

	return withRetry(ctx, logger, "uploading "+key, func() error {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:       aws.String(bucket),
			Key:          aws.String(key),
			Body:         bytes.NewReader(data),
			ContentType:  aws.String(contentType),
			CacheControl: aws.String(cacheControl),
		})
		return err
	})
}

// fileUnchanged returns true if relPath exists in both directories with identical content.
func fileUnchanged(prevDir, curDir, relPath string) bool {
	if prevDir == "" {
		return false
	}
	prevPath := filepath.Join(prevDir, filepath.FromSlash(relPath))
	curPath := filepath.Join(curDir, filepath.FromSlash(relPath))

	prevData, err := os.ReadFile(prevPath)
	if err != nil {
		return false
	}
	curData, err := os.ReadFile(curPath)
	if err != nil {
		return false
	}
	return bytes.Equal(prevData, curData)
}

// CacheControlForPath returns the appropriate Cache-Control header for a given file path.
func CacheControlForPath(path string) string {
	if path == "packages.json" {
		return "public, max-age=300"
	}
	// All p2/ files are mutable
	return "public, max-age=300"
}

// deleteObjectWithRetry deletes a single object from R2 with exponential backoff retry.
func deleteObjectWithRetry(ctx context.Context, client *s3.Client, bucket, key string, logger *slog.Logger) error {
	return withRetry(ctx, logger, "deleting "+key, func() error {
		_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		return err
	})
}

// headObject returns the ETag of an object, or "" if the object doesn't exist.
func headObject(ctx context.Context, client *s3.Client, bucket, key string) (string, error) {
	resp, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// NoSuchKey or similar — object doesn't exist
		return "", nil
	}
	if resp.ETag != nil {
		return *resp.ETag, nil
	}
	return "", nil
}

func newS3Client(cfg config.R2Config) *s3.Client {
	return s3.New(s3.Options{
		Region: "auto",
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID,
			cfg.SecretAccessKey,
			"",
		),
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true,
	})
}
