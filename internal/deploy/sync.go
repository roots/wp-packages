package deploy

import (
	"context"
	"crypto/md5"
	"database/sql"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/roots/wp-packages/internal/composer"
	"github.com/roots/wp-packages/internal/config"
	"github.com/roots/wp-packages/internal/packages"
)

// SyncResult holds statistics from a DB-driven R2 sync.
type SyncResult struct {
	Uploaded int64
	Deleted  int64
	Skipped  int64
	Duration time.Duration
}

// Sync uploads changed packages from the database to R2.
//
// It queries for packages where content_hash != deployed_hash, serializes
// them into Composer p2 JSON files, uploads to R2 in parallel, deletes
// p2 files for deactivated packages, conditionally uploads packages.json,
// and stamps deployed_hash on success.
func Sync(ctx context.Context, db *sql.DB, cfg config.R2Config, appURL string, logger *slog.Logger) (*SyncResult, error) {
	started := time.Now()
	client := newS3Client(cfg)

	var uploaded, deleted, skipped atomic.Int64

	// Step 1: Upload changed p2/ files
	dirty, err := packages.GetDirtyPackages(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("querying dirty packages: %w", err)
	}

	logger.Info("sync: dirty packages", "count", len(dirty))

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(cfg.Concurrency)

	for _, p := range dirty {
		g.Go(func() error {
			files, err := composer.PackageFiles(p.Type, p.Name, p.VersionsJSON, p.ComposerMeta())
			if err != nil {
				return fmt.Errorf("serializing %s/%s: %w", p.Type, p.Name, err)
			}
			for _, f := range files {
				if err := putObjectWithRetry(gCtx, client, cfg.Bucket, f.Key, f.Data, logger); err != nil {
					return fmt.Errorf("uploading %s: %w", f.Key, err)
				}
				uploaded.Add(1)
			}

			n := uploaded.Load() + skipped.Load()
			if n%500 == 0 && n > 0 {
				logger.Info("sync: upload progress", "uploaded", uploaded.Load(), "total_dirty", len(dirty))
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Step 2: Delete p2/ files for deactivated packages
	deactivated, err := packages.GetDeactivatedDeployedPackages(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("querying deactivated packages: %w", err)
	}

	for _, p := range deactivated {
		for _, key := range composer.ObjectKeys(p.Type, p.Name) {
			if err := deleteObjectWithRetry(ctx, client, cfg.Bucket, key, logger); err != nil {
				logger.Warn("sync: failed to delete deactivated package file", "key", key, "error", err)
				continue
			}
		}
		deleted.Add(1)
		logger.Info("sync: deleted deactivated package", "type", p.Type, "name", p.Name)
	}

	// Step 3: Conditional packages.json upload
	packagesData, err := composer.PackagesJSON(appURL)
	if err != nil {
		return nil, fmt.Errorf("generating packages.json: %w", err)
	}

	currentETag, _ := headObject(ctx, client, cfg.Bucket, r2IndexFile)
	newETag := fmt.Sprintf(`"%x"`, md5.Sum(packagesData))
	if currentETag != newETag {
		if err := putObjectWithRetry(ctx, client, cfg.Bucket, r2IndexFile, packagesData, logger); err != nil {
			return nil, fmt.Errorf("uploading packages.json: %w", err)
		}
		logger.Info("sync: uploaded packages.json")
	} else {
		logger.Info("sync: packages.json unchanged, skipped")
	}

	// Step 4: Stamp deployed_hash
	if len(dirty) > 0 {
		_, err = db.ExecContext(ctx, `
			UPDATE packages SET deployed_hash = content_hash
			WHERE is_active = 1 AND content_hash IS NOT NULL
				AND (deployed_hash IS NULL OR content_hash != deployed_hash)`)
		if err != nil {
			return nil, fmt.Errorf("stamping deployed_hash: %w", err)
		}
	}

	if len(deactivated) > 0 {
		_, err = db.ExecContext(ctx, `
			UPDATE packages SET deployed_hash = NULL
			WHERE is_active = 0 AND deployed_hash IS NOT NULL`)
		if err != nil {
			return nil, fmt.Errorf("clearing deployed_hash for deactivated: %w", err)
		}
	}

	result := &SyncResult{
		Uploaded: uploaded.Load(),
		Deleted:  deleted.Load(),
		Skipped:  skipped.Load(),
		Duration: time.Since(started),
	}

	logger.Info("sync: complete",
		"uploaded", result.Uploaded,
		"deleted", result.Deleted,
		"duration", result.Duration.String(),
	)
	return result, nil
}
