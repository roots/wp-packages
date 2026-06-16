package cmd

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/roots/wp-packages/internal/packages"
	"github.com/roots/wp-packages/internal/wporg"
)

// svnLogChunkSize bounds how many revisions each changelog REPORT covers, so the
// watermark advances in verifiable steps and a large catch-up after downtime
// can't exceed the server's per-response log-item limit.
const svnLogChunkSize = 10000

var discoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "Discover packages from WordPress.org",
	Long:  "Creates shell package records (type, name, last_committed). Use 'update' to fetch full metadata.",
	RunE:  runDiscover,
}

func runDiscover(cmd *cobra.Command, args []string) error {
	source, _ := cmd.Flags().GetString("source")
	pkgType, _ := cmd.Flags().GetString("type")
	limit, _ := cmd.Flags().GetInt("limit")
	concurrency, _ := cmd.Flags().GetInt("concurrency")

	if concurrency <= 0 {
		concurrency = application.Config.Discovery.Concurrency
	}

	switch source {
	case "config":
		return discoverFromConfig(cmd.Context(), pkgType, limit, concurrency)
	case "svn":
		return discoverFromSVN(cmd.Context(), pkgType, limit)
	default:
		return fmt.Errorf("unknown source %q (use config or svn)", source)
	}
}

func discoverFromConfig(ctx context.Context, pkgType string, limit, concurrency int) error {
	seeds, err := packages.LoadSeeds(application.Config.Discovery.SeedsFile)
	if err != nil {
		return err
	}

	client := wporg.NewClient(application.Config.Discovery, application.Logger)

	type job struct {
		slug    string
		pkgType string
	}

	var jobs []job
	if pkgType == "all" || pkgType == "plugin" {
		for _, slug := range seeds.PopularPlugins {
			jobs = append(jobs, job{slug: slug, pkgType: "plugin"})
		}
	}
	if pkgType == "all" || pkgType == "theme" {
		for _, slug := range seeds.PopularThemes {
			jobs = append(jobs, job{slug: slug, pkgType: "theme"})
		}
	}

	if limit > 0 && limit < len(jobs) {
		jobs = jobs[:limit]
	}

	application.Logger.Info("discovering packages from config", "count", len(jobs), "concurrency", concurrency)

	var succeeded, failed atomic.Int64
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for _, j := range jobs {
		j := j
		g.Go(func() error {
			// Fetch minimal info from API to get last_updated date
			lastCommitted, fetchErr := client.FetchLastUpdated(gCtx, j.pkgType, j.slug)
			if fetchErr != nil {
				application.Logger.Warn("failed to fetch package info", "type", j.pkgType, "slug", j.slug, "error", fetchErr)
				failed.Add(1)
				return nil
			}

			if err := packages.UpsertShellPackage(gCtx, application.DB, j.pkgType, j.slug, lastCommitted); err != nil {
				application.Logger.Warn("failed to store shell package", "type", j.pkgType, "slug", j.slug, "error", err)
				failed.Add(1)
				return nil
			}

			succeeded.Add(1)
			application.Logger.Debug("discovered package", "type", j.pkgType, "slug", j.slug)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	application.Logger.Info("discovery complete", "succeeded", succeeded.Load(), "failed", failed.Load())
	if err := packages.RefreshSiteStats(ctx, application.DB); err != nil {
		return fmt.Errorf("refreshing package stats: %w", err)
	}
	if failed.Load() > 0 {
		return fmt.Errorf("discovery completed with %d failures", failed.Load())
	}
	return nil
}

func discoverFromSVN(ctx context.Context, pkgType string, limit int) error {
	const svnBatchSize = 500

	client := wporg.NewClient(application.Config.Discovery, application.Logger)

	type svnSource struct {
		url     string
		pkgType string
		metaKey string // site_meta key for storing last-seen SVN revision
	}

	var sources []svnSource
	if pkgType == "all" || pkgType == "plugin" {
		sources = append(sources, svnSource{
			url:     "https://plugins.svn.wordpress.org/",
			pkgType: "plugin",
			metaKey: "svn_revision_plugin",
		})
	}
	if pkgType == "all" || pkgType == "theme" {
		sources = append(sources, svnSource{
			url:     "https://themes.svn.wordpress.org/",
			pkgType: "theme",
			metaKey: "svn_revision_theme",
		})
	}

	var totalCount, totalFailed int
	for _, src := range sources {
		application.Logger.Info("discovering from SVN", "type", src.pkgType, "url", src.url)

		var count, failed int
		batch := make([]packages.ShellEntry, 0, svnBatchSize)

		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := packages.BatchUpsertShellPackages(ctx, application.DB, batch); err != nil {
				application.Logger.Warn("batch upsert failed, falling back to individual", "error", err)
				for _, e := range batch {
					if err := packages.UpsertShellPackage(ctx, application.DB, e.Type, e.Name, e.LastCommitted); err != nil {
						application.Logger.Warn("failed to upsert shell package", "slug", e.Name, "error", err)
						failed++
						count--
					}
				}
			}
			batch = batch[:0]
		}

		result, err := client.ParseSVNListing(ctx, src.url, func(entry wporg.SVNEntry) error {
			if limit > 0 && totalCount >= limit {
				return errLimitReached
			}

			batch = append(batch, packages.ShellEntry{
				Type:          src.pkgType,
				Name:          entry.Slug,
				LastCommitted: entry.LastCommitted,
			})
			count++
			totalCount++

			if len(batch) >= svnBatchSize {
				flush()
			}
			return nil
		})

		flush()

		if err != nil && err != errLimitReached {
			return fmt.Errorf("SVN discovery for %s: %w", src.pkgType, err)
		}

		// Use SVN revision log to find which packages changed since last run.
		// Skip when --limit is set (partial/test runs shouldn't mutate global
		// change state or trigger large update workloads).
		if limit == 0 && result != nil && result.Revision > 0 {
			if markErr := markChangedFromSVNLog(ctx, client, src, result.Revision); markErr != nil {
				application.Logger.Warn("SVN changelog fetch failed, skipping change detection",
					"type", src.pkgType, "error", markErr)
			}
		}

		totalFailed += failed
		application.Logger.Info("SVN discovery done", "type", src.pkgType, "succeeded", count, "failed", failed)

		if limit > 0 && totalCount >= limit {
			break
		}
	}

	if err := packages.RefreshSiteStats(ctx, application.DB); err != nil {
		return fmt.Errorf("refreshing package stats: %w", err)
	}

	if totalFailed > 0 {
		return fmt.Errorf("SVN discovery completed with %d failures", totalFailed)
	}
	return nil
}

// markChangedFromSVNLog fetches the SVN log between the last-seen revision and
// the current revision, extracts which slugs changed, and marks them in the DB
// so they'll be picked up by the update step.
func markChangedFromSVNLog(ctx context.Context, client *wporg.Client, src struct {
	url     string
	pkgType string
	metaKey string
}, currentRev int64) error {
	lastRevStr, err := packages.GetMeta(ctx, application.DB, src.metaKey)
	if err != nil {
		return fmt.Errorf("reading last revision: %w", err)
	}

	var lastRev int64
	if lastRevStr != "" {
		var parseErr error
		lastRev, parseErr = strconv.ParseInt(lastRevStr, 10, 64)
		if parseErr != nil {
			return fmt.Errorf("malformed stored revision %q for %s: %w", lastRevStr, src.metaKey, parseErr)
		}
	}

	// First run: no baseline to diff against, so just record HEAD as the
	// starting point for the next run.
	if lastRev == 0 {
		application.Logger.Info("no previous SVN revision stored, skipping changelog (first run)",
			"type", src.pkgType, "current_rev", currentRev)
		if err := packages.SetMeta(ctx, application.DB, src.metaKey, strconv.FormatInt(currentRev, 10)); err != nil {
			return fmt.Errorf("storing revision: %w", err)
		}
		return nil
	}

	// Scan forward from the watermark in bounded chunks. The watermark advances
	// only to the highest revision the changelog REPORT actually returned —
	// never to currentRev, which is read from a different endpoint (the HTML
	// listing) that can be ahead of the REPORT replica. Advancing past
	// revisions the REPORT never returned would skip those commits permanently
	// and strand the affected packages at stale versions. Chunking also keeps
	// each response well under the server's log-item limit so a large catch-up
	// after downtime can't silently truncate.
	cursor := lastRev
	for cursor < currentRev {
		chunkEnd := cursor + svnLogChunkSize
		if chunkEnd > currentRev {
			chunkEnd = currentRev
		}

		application.Logger.Info("fetching SVN changelog",
			"type", src.pkgType, "from_rev", cursor+1, "to_rev", chunkEnd)

		slugRevisions, maxRev, err := client.FetchSVNChangedSlugs(ctx, src.url, cursor+1, chunkEnd)
		if err != nil {
			// Persist whatever we fully scanned before bailing so the next run
			// resumes from there instead of redoing work.
			if cursor > lastRev {
				if setErr := packages.SetMeta(ctx, application.DB, src.metaKey, strconv.FormatInt(cursor, 10)); setErr != nil {
					return fmt.Errorf("storing revision: %w", setErr)
				}
			}
			return err
		}

		if len(slugRevisions) > 0 {
			affected, err := packages.MarkPackagesChanged(ctx, application.DB, src.pkgType, slugRevisions)
			if err != nil {
				return fmt.Errorf("marking changed packages: %w", err)
			}
			application.Logger.Info("marked changed packages from SVN log",
				"type", src.pkgType, "from_rev", cursor+1, "to_rev", chunkEnd,
				"slugs_in_log", len(slugRevisions), "packages_marked", affected)
		}

		// Every revision in the repo is a commit, so the REPORT yields a
		// log-item per revision in range; maxRev is therefore the highest
		// revision the REPORT replica actually holds within this chunk.
		if maxRev <= cursor {
			// REPORT endpoint is behind our cursor (replica lag) — nothing new
			// available. Stop and retry from the same point next run.
			break
		}
		cursor = maxRev
		if maxRev < chunkEnd {
			// Replica lagged within this chunk; rescan the remainder next run
			// rather than skipping it.
			break
		}
	}

	if err := packages.SetMeta(ctx, application.DB, src.metaKey, strconv.FormatInt(cursor, 10)); err != nil {
		return fmt.Errorf("storing revision: %w", err)
	}

	return nil
}

var errLimitReached = fmt.Errorf("limit reached")

var backfillRevisionsCmd = &cobra.Command{
	Use:   "backfill-revisions",
	Short: "Backfill trunk_revision for plugins missing it",
	Long:  "Scans SVN changelog backwards to populate trunk_revision for active plugins that don't have one yet.",
	RunE:  runBackfillRevisions,
}

func runBackfillRevisions(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	client := wporg.NewClient(application.Config.Discovery, application.Logger)
	baseURL := "https://plugins.svn.wordpress.org/"

	// Get current SVN revision
	chunkSize, _ := cmd.Flags().GetInt64("chunk-size")

	// Count how many plugins need backfill
	var remaining int
	if err := application.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM packages WHERE is_active = 1 AND type = 'plugin' AND trunk_revision IS NULL`,
	).Scan(&remaining); err != nil {
		return fmt.Errorf("counting packages: %w", err)
	}
	application.Logger.Info("plugins needing trunk_revision backfill", "count", remaining)

	if remaining == 0 {
		application.Logger.Info("nothing to backfill")
		return nil
	}

	// Get current revision from SVN listing
	var currentRev int64
	result, err := client.ParseSVNListing(ctx, baseURL, func(entry wporg.SVNEntry) error {
		return nil
	})
	if err != nil {
		return fmt.Errorf("fetching current SVN revision: %w", err)
	}
	if result != nil {
		currentRev = result.Revision
	}
	if currentRev == 0 {
		return fmt.Errorf("could not determine current SVN revision")
	}

	application.Logger.Info("starting backfill", "current_rev", currentRev, "chunk_size", chunkSize)

	// Scan backwards in chunks
	var totalUpdated int64
	fromRev := currentRev
	for fromRev > 0 && remaining > 0 {
		toRev := fromRev - chunkSize
		if toRev < 1 {
			toRev = 1
		}

		application.Logger.Info("fetching SVN changelog chunk",
			"from_rev", toRev, "to_rev", fromRev, "remaining", remaining)

		var slugRevisions map[string]int64
		var fetchErr error
		for attempt := 1; attempt <= 3; attempt++ {
			slugRevisions, _, fetchErr = client.FetchSVNChangedSlugs(ctx, baseURL, toRev, fromRev)
			if fetchErr == nil {
				break
			}
			application.Logger.Warn("SVN log chunk fetch failed, retrying",
				"from_rev", toRev, "to_rev", fromRev, "attempt", attempt, "error", fetchErr)
			time.Sleep(time.Duration(attempt) * 5 * time.Second)
		}
		if fetchErr != nil {
			application.Logger.Warn("skipping chunk after retries",
				"from_rev", toRev, "to_rev", fromRev, "error", fetchErr)
			fromRev = toRev - 1
			continue
		}

		if len(slugRevisions) > 0 {
			updated, err := packages.BackfillTrunkRevisions(ctx, application.DB, slugRevisions)
			if err != nil {
				return fmt.Errorf("backfilling trunk revisions: %w", err)
			}
			totalUpdated += updated
			remaining -= int(updated)
			application.Logger.Info("backfill chunk complete",
				"slugs_in_chunk", len(slugRevisions), "updated", updated,
				"total_updated", totalUpdated, "remaining", remaining)
		}

		fromRev = toRev - 1
	}

	application.Logger.Info("backfill complete", "total_updated", totalUpdated)
	return nil
}

func init() {
	appCommand(discoverCmd)
	discoverCmd.Flags().String("source", "config", "discovery source (config or svn)")
	discoverCmd.Flags().String("type", "all", "package type (plugin, theme, or all)")
	discoverCmd.Flags().Int("limit", 0, "maximum packages to discover (0 = unlimited)")
	discoverCmd.Flags().Int("concurrency", 0, "concurrent API requests (0 = use config default)")
	rootCmd.AddCommand(discoverCmd)

	appCommand(backfillRevisionsCmd)
	backfillRevisionsCmd.Flags().Int64("chunk-size", 10000, "number of SVN revisions per chunk")
	rootCmd.AddCommand(backfillRevisionsCmd)
}
