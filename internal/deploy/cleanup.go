package deploy

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/roots/wp-packages/internal/composer"
	"github.com/roots/wp-packages/internal/config"
	"github.com/roots/wp-packages/internal/packages"
)

// r2DeleteBatchSize is the max number of keys per S3 DeleteObjects call (S3/R2 limit is 1000).
const r2DeleteBatchSize = 1000

// BulkDeleteResult holds stats from a bulk cleanup run.
type BulkDeleteResult struct {
	Packages    int
	KeysDeleted int
	KeysFailed  int
}

// BulkDeleteDeactivated deletes R2 files for all packages where
// is_active=0 AND deployed_hash IS NOT NULL, using S3 DeleteObjects
// (batches of up to 1000 keys) parallelized across cfg.Concurrency workers.
// After each batch succeeds, deployed_hash is cleared for the packages whose
// keys all succeeded in that batch.
//
// Intended for one-off cleanup of historical orphans (the regular per-deploy
// path in SyncToR2 uses a serial loop which is fine for the small number of
// deactivations per 5-min pipeline cycle).
func BulkDeleteDeactivated(ctx context.Context, db *sql.DB, cfg config.R2Config, logger *slog.Logger) (*BulkDeleteResult, error) {
	deactivated, err := packages.GetDeactivatedDeployedPackages(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("querying deactivated packages: %w", err)
	}
	if len(deactivated) == 0 {
		return &BulkDeleteResult{}, nil
	}

	client := newS3Client(cfg)

	type keyRef struct {
		PkgID int64
		Key   string
	}
	keys := make([]keyRef, 0, len(deactivated)*2)
	pkgKeyCount := make(map[int64]int, len(deactivated))
	for _, p := range deactivated {
		for _, k := range composer.ObjectKeys(p.Type, p.Name) {
			keys = append(keys, keyRef{PkgID: p.ID, Key: k})
			pkgKeyCount[p.ID]++
		}
	}

	logger.Info("bulk delete starting",
		"packages", len(deactivated), "keys", len(keys), "concurrency", cfg.Concurrency)

	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 50
	}

	var (
		keysDeleted atomic.Int64
		keysFailed  atomic.Int64
		mu          sync.Mutex
		pkgFailed   = make(map[int64]int)
	)

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for start := 0; start < len(keys); start += r2DeleteBatchSize {
		end := start + r2DeleteBatchSize
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[start:end]
		batchStart := start
		g.Go(func() error {
			objs := make([]s3types.ObjectIdentifier, len(batch))
			for i, k := range batch {
				objs[i] = s3types.ObjectIdentifier{Key: aws.String(k.Key)}
			}
			resp, err := client.DeleteObjects(gCtx, &s3.DeleteObjectsInput{
				Bucket: aws.String(cfg.Bucket),
				Delete: &s3types.Delete{Objects: objs, Quiet: aws.Bool(true)},
			})
			if err != nil {
				logger.Warn("bulk delete: batch failed", "batch_start", batchStart, "size", len(batch), "error", err)
				mu.Lock()
				for _, k := range batch {
					pkgFailed[k.PkgID]++
				}
				mu.Unlock()
				keysFailed.Add(int64(len(batch)))
				return nil
			}

			failedKeys := make(map[string]struct{}, len(resp.Errors))
			for _, e := range resp.Errors {
				if e.Key == nil {
					continue
				}
				failedKeys[*e.Key] = struct{}{}
				logger.Warn("bulk delete: key failed",
					"key", *e.Key,
					"code", aws.ToString(e.Code),
					"msg", aws.ToString(e.Message))
			}

			batchSuccessIDs := make([]int64, 0, len(batch))
			for _, k := range batch {
				if _, bad := failedKeys[k.Key]; bad {
					mu.Lock()
					pkgFailed[k.PkgID]++
					mu.Unlock()
					keysFailed.Add(1)
					continue
				}
				batchSuccessIDs = append(batchSuccessIDs, k.PkgID)
				keysDeleted.Add(1)
			}

			// Best-effort: clear deployed_hash for packages whose every key in
			// THIS batch succeeded. A package may have keys across multiple
			// batches; we re-check overall success at the end and re-clear if
			// needed (idempotent).
			if err := clearDeployedHash(gCtx, db, batchSuccessIDs); err != nil {
				logger.Warn("bulk delete: failed to clear deployed_hash for batch", "error", err)
			}

			done := keysDeleted.Load() + keysFailed.Load()
			logger.Info("bulk delete: batch done",
				"deleted_so_far", keysDeleted.Load(),
				"failed_so_far", keysFailed.Load(),
				"progress", fmt.Sprintf("%d/%d", done, len(keys)))
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("bulk delete: %w", err)
	}

	// Final pass: any package with zero failed keys gets deployed_hash cleared.
	// (The per-batch clear above handles the common case; this covers packages
	// whose keys spanned multiple batches.)
	allOKIDs := make([]int64, 0, len(deactivated))
	for _, p := range deactivated {
		if pkgFailed[p.ID] == 0 {
			allOKIDs = append(allOKIDs, p.ID)
		}
	}
	if err := clearDeployedHash(ctx, db, allOKIDs); err != nil {
		return nil, fmt.Errorf("bulk delete: final clear deployed_hash: %w", err)
	}

	return &BulkDeleteResult{
		Packages:    len(deactivated),
		KeysDeleted: int(keysDeleted.Load()),
		KeysFailed:  int(keysFailed.Load()),
	}, nil
}

// clearDeployedHash sets deployed_hash = NULL for the given package IDs.
// Chunks the IN clause to stay under SQLite's parameter limit.
func clearDeployedHash(ctx context.Context, db *sql.DB, ids []int64) error {
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		slice := ids[start:end]
		placeholders := strings.Repeat("?,", len(slice))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(slice))
		for i, id := range slice {
			args[i] = id
		}
		q := fmt.Sprintf(`UPDATE packages SET deployed_hash = NULL WHERE id IN (%s)`, placeholders)
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}
