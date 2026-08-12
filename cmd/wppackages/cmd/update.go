package cmd

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/roots/wp-packages/internal/composer"
	"github.com/roots/wp-packages/internal/packages"
	"github.com/roots/wp-packages/internal/wporg"
)

// staleRetryWindow is how long to keep retrying a package when the wp.org API
// returns unchanged versions after an SVN commit is detected. After this window,
// we assume the commit was a non-version change (readme, assets, etc.).
// Set high (24h) to account for extended wp.org API cache delays.
const staleRetryWindow = 24 * time.Hour

// pendingStableRetryWindow is how long to keep retrying a theme whose SVN tags
// include a stable version above what the wp.org directory reports as current.
// Theme updates sit in a review queue after the tag lands, routinely for days,
// so the standard window would expire before the release goes live.
const pendingStableRetryWindow = 7 * 24 * time.Hour

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Fetch and update package metadata from WordPress.org",
	RunE:  runUpdate,
}

func runUpdate(cmd *cobra.Command, args []string) error {
	pkgType, _ := cmd.Flags().GetString("type")
	name, _ := cmd.Flags().GetString("name")
	force, _ := cmd.Flags().GetBool("force")
	limit, _ := cmd.Flags().GetInt("limit")
	includeInactive, _ := cmd.Flags().GetBool("include-inactive")
	concurrency, _ := cmd.Flags().GetInt("concurrency")

	if concurrency <= 0 {
		concurrency = application.Config.Discovery.Concurrency
	}

	ctx := cmd.Context()

	syncRun, err := packages.AllocateSyncRunID(ctx, application.DB)
	if err != nil {
		return fmt.Errorf("allocating sync run: %w", err)
	}
	application.Logger.Info("starting update", "sync_run_id", syncRun.RunID)

	pkgs, err := packages.GetPackagesNeedingUpdate(ctx, application.DB, packages.UpdateQueryOpts{
		Type:            pkgType,
		Name:            name,
		Force:           force,
		IncludeInactive: includeInactive,
		Limit:           limit,
	})
	if err != nil {
		return fmt.Errorf("querying packages: %w", err)
	}

	if len(pkgs) == 0 {
		application.Logger.Info("no packages need updating")
		if err := packages.FinishSyncRun(ctx, application.DB, syncRun.RowID, "completed", map[string]any{"updated": 0}); err != nil {
			return fmt.Errorf("finishing sync run: %w", err)
		}
		return nil
	}

	application.Logger.Info("updating packages", "count", len(pkgs), "concurrency", concurrency)

	client := wporg.NewClient(application.Config.Discovery, application.Logger)

	const writeBatchSize = 100

	var succeeded, failed, deactivated, tombstoned, changed, staleRetried, staleExpired atomic.Int64
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	// Writer goroutine batches DB writes
	writeCh := make(chan *packages.Package, concurrency*2)
	writeErrCh := make(chan error, 1)
	go func() {
		defer close(writeErrCh)
		batch := make([]*packages.Package, 0, writeBatchSize)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := packages.BatchUpsertPackages(ctx, application.DB, batch); err != nil {
				application.Logger.Warn("batch upsert failed, falling back to individual", "error", err)
				for _, pkg := range batch {
					if err := packages.UpsertPackage(ctx, application.DB, pkg); err != nil {
						application.Logger.Warn("failed to store", "type", pkg.Type, "name", pkg.Name, "error", err)
						failed.Add(1)
						succeeded.Add(-1)
					}
				}
			}
			batch = batch[:0]
		}
		for pkg := range writeCh {
			batch = append(batch, pkg)
			if len(batch) >= writeBatchSize {
				flush()
			}
		}
		flush()
	}()

	for _, p := range pkgs {
		p := p
		g.Go(func() error {
			var data map[string]any
			var fetchErr error

			if p.Type == "plugin" {
				data, fetchErr = client.FetchPlugin(gCtx, p.Name)
			} else {
				data, fetchErr = client.FetchTheme(gCtx, p.Name)
			}

			if fetchErr != nil {
				if errors.Is(fetchErr, wporg.ErrClosedPermanent) {
					if err := packages.MarkPermanentlyClosed(gCtx, application.DB, p.ID); err != nil {
						application.Logger.Warn("failed to tombstone package", "type", p.Type, "name", p.Name, "error", err)
						failed.Add(1)
					} else {
						tombstoned.Add(1)
					}
				} else if errors.Is(fetchErr, wporg.ErrNotFound) {
					if err := packages.DeactivatePackage(gCtx, application.DB, p.ID); err != nil {
						application.Logger.Warn("failed to deactivate 404 package", "type", p.Type, "name", p.Name, "error", err)
					}
					deactivated.Add(1)
				} else {
					application.Logger.Warn("failed to fetch", "type", p.Type, "name", p.Name, "error", fetchErr)
					failed.Add(1)
				}
				total := succeeded.Load() + failed.Load() + deactivated.Load() + tombstoned.Load()
				if total%500 == 0 {
					application.Logger.Info("update progress",
						"completed", total,
						"total", len(pkgs),
						"succeeded", succeeded.Load(),
						"failed", failed.Load(),
						"deactivated", deactivated.Load(),
						"tombstoned", tombstoned.Load(),
					)
				}
				return nil
			}

			pkg := packages.PackageFromAPIData(data, p.Type)
			pkg.ID = p.ID

			validVersions, pendingStable, err := pkg.NormalizeAndStoreVersions()
			if err != nil {
				application.Logger.Warn("version normalization failed", "type", p.Type, "name", p.Name, "error", err)
				failed.Add(1)
				return nil
			}

			if validVersions == 0 {
				application.Logger.Debug("package has no tagged versions", "type", p.Type, "name", p.Name)
			}

			// Carry forward trunk_revision from DB (set by discover step)
			pkg.TrunkRevision = p.TrunkRevision

			// UpsertPackage keeps the greater of the stored and incoming
			// last_committed. Mirror that here so the hash is computed over the
			// value that will actually be in the row — otherwise a wp.org
			// last_updated that moves backwards produces a hash the sync step
			// can never satisfy, and the package re-uploads on every run.
			if p.LastCommitted != nil && (pkg.LastCommitted == nil || pkg.LastCommitted.Before(*p.LastCommitted)) {
				pkg.LastCommitted = p.LastCommitted
			}

			// Hash the serialized output, not its inputs — see composer.HashContent.
			newHash, err := composer.HashContent(pkg.Type, pkg.Name, pkg.VersionsJSON, pkg.ComposerMeta())
			if err != nil {
				application.Logger.Warn("content hash failed", "type", p.Type, "name", p.Name, "error", err)
				failed.Add(1)
				return nil
			}
			pkg.ContentHash = &newHash
			if p.ContentHash == nil || *p.ContentHash != newHash {
				now := time.Now().UTC()
				pkg.ContentChangedAt = &now
			}

			now := time.Now().UTC()
			pkg.LastSyncRunID = &syncRun.RunID

			decision := shouldAdvanceSyncedAt(pkg.VersionsJSON, p.VersionsJSON, pendingStable, p.Type, p.LastCommitted, now)
			if pkg.VersionsJSON != p.VersionsJSON {
				changed.Add(1)
			}
			switch decision {
			case syncAdvance:
				pkg.LastSyncedAt = &now
			case syncRetry:
				pkg.LastSyncedAt = p.LastSyncedAt
				staleRetried.Add(1)
				application.Logger.Debug("versions unchanged, keeping dirty for retry",
					"type", p.Type, "name", p.Name, "last_committed", p.LastCommitted)
			case syncExpire:
				pkg.LastSyncedAt = &now
				staleExpired.Add(1)
				application.Logger.Debug("versions unchanged, retry window expired",
					"type", p.Type, "name", p.Name)
			}

			succeeded.Add(1)
			writeCh <- pkg

			total := succeeded.Load() + failed.Load() + deactivated.Load() + tombstoned.Load()
			if total%500 == 0 {
				application.Logger.Info("update progress",
					"completed", total,
					"total", len(pkgs),
					"succeeded", succeeded.Load(),
					"failed", failed.Load(),
					"deactivated", deactivated.Load(),
					"tombstoned", tombstoned.Load(),
				)
			}
			application.Logger.Debug("updated package", "type", p.Type, "name", p.Name, "versions", validVersions)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	close(writeCh)
	<-writeErrCh // wait for writer to finish

	stats := map[string]any{
		"updated":       succeeded.Load(),
		"changed":       changed.Load(),
		"failed":        failed.Load(),
		"deactivated":   deactivated.Load(),
		"tombstoned":    tombstoned.Load(),
		"stale_retried": staleRetried.Load(),
		"stale_expired": staleExpired.Load(),
	}

	status := "completed"
	if failed.Load() > 0 {
		status = "completed_with_errors"
	}

	if err := packages.FinishSyncRun(ctx, application.DB, syncRun.RowID, status, stats); err != nil {
		return fmt.Errorf("finishing sync run: %w", err)
	}

	if err := packages.RefreshSiteStats(ctx, application.DB); err != nil {
		return fmt.Errorf("refreshing package stats: %w", err)
	}

	application.Logger.Info("update complete",
		"updated", succeeded.Load(),
		"changed", changed.Load(),
		"failed", failed.Load(),
		"deactivated", deactivated.Load(),
		"tombstoned", tombstoned.Load(),
		"stale_retried", staleRetried.Load(),
		"stale_expired", staleExpired.Load(),
	)
	return nil
}

type syncDecision int

const (
	syncAdvance syncDecision = iota // versions changed — advance last_synced_at
	syncRetry                       // versions unchanged, within retry window — keep dirty
	syncExpire                      // versions unchanged, window expired — advance last_synced_at
)

// shouldAdvanceSyncedAt decides whether to advance last_synced_at after an update.
// A pending stable release (a tag above the wp.org stable version, awaiting
// directory publication) keeps the package dirty within its retry window even
// when versions changed — the change may be an incidental catch-up from an
// earlier commit, and advancing would strand the package on the old version
// once the release goes live. Otherwise: if versions changed, advance; if
// unchanged, keep dirty within the retry window to handle wp.org API cache
// delays, then advance to avoid infinite retries from non-version SVN changes
// (readme, assets).
func shouldAdvanceSyncedAt(newVersions, oldVersions string, pendingStable bool, pkgType string, lastCommitted *time.Time, now time.Time) syncDecision {
	window := staleRetryWindow
	if pendingStable && pkgType == "theme" {
		window = pendingStableRetryWindow
	}
	withinWindow := lastCommitted != nil && now.Sub(*lastCommitted) <= window

	if pendingStable && withinWindow {
		return syncRetry
	}
	if newVersions != oldVersions {
		return syncAdvance
	}
	if withinWindow {
		return syncRetry
	}
	return syncExpire
}

func init() {
	appCommand(updateCmd)
	updateCmd.Flags().String("type", "all", "package type (plugin, theme, or all)")
	updateCmd.Flags().String("name", "", "specific package slug to update")
	updateCmd.Flags().Bool("force", false, "force update all packages")
	updateCmd.Flags().Int("limit", 0, "maximum packages to update (0 = unlimited)")
	updateCmd.Flags().Bool("include-inactive", false, "include inactive packages")
	updateCmd.Flags().Int("concurrency", 0, "concurrent API requests (0 = use config default)")
	rootCmd.AddCommand(updateCmd)
}
