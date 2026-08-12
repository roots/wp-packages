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

// DefaultMaxDeletes caps how many packages a single sync will remove from R2
// before refusing. Deactivations normally arrive as a trickle of a few per run;
// a spike means something upstream went wrong — a wp.org API degradation that
// mass-deactivates packages would otherwise translate straight into mass
// deletion, recoverable only by a full re-upload. Uploads are unaffected.
const DefaultMaxDeletes = 250

// SyncResult holds statistics from a DB-driven R2 sync.
type SyncResult struct {
	Uploaded int64
	Deleted  int64
	Skipped  int64
	// DeletesSkipped is non-zero when the deactivated count exceeded MaxDeletes
	// and deletion was refused for this run.
	DeletesSkipped int
	// Unhashed counts active packages with no content_hash. They cannot be
	// synced; `wppackages rehash` fixes them.
	Unhashed int
	Duration time.Duration
}

// SyncOptions tunes a sync run.
type SyncOptions struct {
	// DryRun reports what would be uploaded and deleted without touching R2
	// or the database.
	DryRun bool
	// MaxDeletes caps deletions per run. Zero means DefaultMaxDeletes;
	// negative means unlimited.
	MaxDeletes int
}

// Sync uploads changed packages from the database to R2.
//
// It queries for packages where content_hash != deployed_hash, serializes
// them into Composer p2 JSON files, uploads to R2 in parallel, deletes
// p2 files for deactivated packages, conditionally uploads packages.json,
// and stamps deployed_hash on success.
func Sync(ctx context.Context, db *sql.DB, cfg config.R2Config, appURL string, opts SyncOptions, logger *slog.Logger) (*SyncResult, error) {
	started := time.Now()
	client := newS3Client(cfg)

	maxDeletes := opts.MaxDeletes
	if maxDeletes == 0 {
		maxDeletes = DefaultMaxDeletes
	}

	var uploaded, deleted, skipped atomic.Int64

	// Active packages with no content_hash are excluded from the diff query and
	// will never be uploaded. Report rather than skip silently.
	unhashed, err := packages.CountUnhashedActive(ctx, db)
	if err != nil {
		return nil, err
	}
	if unhashed > 0 {
		logger.Warn("sync: active packages have no content_hash and cannot be synced",
			"count", unhashed, "fix", "run `wppackages rehash`")
	}

	// Step 1: Upload changed p2/ files
	dirty, err := packages.GetDirtyPackages(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("querying dirty packages: %w", err)
	}

	logger.Info("sync: dirty packages", "count", len(dirty), "dry_run", opts.DryRun)

	if opts.DryRun {
		for _, p := range dirty {
			files, err := composer.PackageFiles(p.Type, p.Name, p.VersionsJSON, p.ComposerMeta())
			if err != nil {
				return nil, fmt.Errorf("serializing %s/%s: %w", p.Type, p.Name, err)
			}
			uploaded.Add(int64(len(files)))
		}
	} else {
		g, gCtx := errgroup.WithContext(ctx)
		g.SetLimit(cfg.Concurrency)

		for _, p := range dirty {
			p := p
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
	}

	// Step 2: Delete p2/ files for deactivated packages
	deactivated, err := packages.GetDeactivatedDeployedPackages(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("querying deactivated packages: %w", err)
	}

	deletesSkipped := 0
	deleteBlocked := maxDeletes >= 0 && len(deactivated) > maxDeletes
	if deleteBlocked {
		deletesSkipped = len(deactivated)
		logger.Error("sync: refusing to delete, deactivated count exceeds limit",
			"deactivated", len(deactivated),
			"max_deletes", maxDeletes,
			"action", "verify the deactivations are genuine, then re-run with --max-deletes")
	}

	if !deleteBlocked {
		for _, p := range deactivated {
			if opts.DryRun {
				deleted.Add(1)
				continue
			}
			allOK := true
			for _, key := range composer.ObjectKeys(p.Type, p.Name) {
				if err := deleteObjectWithRetry(ctx, client, cfg.Bucket, key, logger); err != nil {
					logger.Warn("sync: failed to delete deactivated package file", "key", key, "error", err)
					allOK = false
				}
			}
			if !allOK {
				// Leave deployed_hash set so the next run retries this package.
				continue
			}
			if _, err := db.ExecContext(ctx,
				`UPDATE packages SET deployed_hash = NULL WHERE id = ?`, p.ID); err != nil {
				logger.Warn("sync: failed to clear deployed_hash", "package", p.Name, "error", err)
				continue
			}
			deleted.Add(1)
			logger.Info("sync: deleted deactivated package", "type", p.Type, "name", p.Name)
		}
	}

	// Step 3: Conditional packages.json upload
	packagesData, err := composer.PackagesJSON(appURL)
	if err != nil {
		return nil, fmt.Errorf("generating packages.json: %w", err)
	}

	currentETag, _ := headObject(ctx, client, cfg.Bucket, r2IndexFile)
	newETag := fmt.Sprintf(`"%x"`, md5.Sum(packagesData))
	switch {
	case currentETag == newETag:
		logger.Info("sync: packages.json unchanged, skipped")
	case opts.DryRun:
		logger.Info("sync: would upload packages.json")
	default:
		if err := putObjectWithRetry(ctx, client, cfg.Bucket, r2IndexFile, packagesData, logger); err != nil {
			return nil, fmt.Errorf("uploading packages.json: %w", err)
		}
		logger.Info("sync: uploaded packages.json")
	}

	// Step 4: Stamp deployed_hash. Always after all uploads succeed, so the DB
	// can lag R2 (costing one redundant re-sync) but never lead it.
	if !opts.DryRun && len(dirty) > 0 {
		_, err = db.ExecContext(ctx, `
			UPDATE packages SET deployed_hash = content_hash
			WHERE is_active = 1 AND content_hash IS NOT NULL
				AND (deployed_hash IS NULL OR content_hash != deployed_hash)`)
		if err != nil {
			return nil, fmt.Errorf("stamping deployed_hash: %w", err)
		}
	}

	result := &SyncResult{
		Uploaded:       uploaded.Load(),
		Deleted:        deleted.Load(),
		Skipped:        skipped.Load(),
		DeletesSkipped: deletesSkipped,
		Unhashed:       unhashed,
		Duration:       time.Since(started),
	}

	logger.Info("sync: complete",
		"uploaded", result.Uploaded,
		"deleted", result.Deleted,
		"deletes_skipped", result.DeletesSkipped,
		"unhashed", result.Unhashed,
		"dry_run", opts.DryRun,
		"duration", result.Duration.String(),
	)
	return result, nil
}
