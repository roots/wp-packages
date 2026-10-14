package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/roots/wp-packages/internal/packages"
	"github.com/roots/wp-packages/internal/wporg"
)

var backfillPublishedTimesCmd = &cobra.Command{
	Use:   "backfill-published-times",
	Short: "Backfill per-version published times for Composer's cooldown policy",
	Long: `Populates published_times_json for packages that don't have it yet.

Existing versions have no record of when WP Packages first published them, so
the SVN changelog is scanned backwards from the current SVN head for --days.
Versions whose tag (or whole tags directory) was created or changed in that
window get that commit date; all others get the start of the window. Neither
can be earlier than the version's true publication, so the backfill never
shortens a cooldown. If the changelog endpoint returns an incomplete range
(e.g. it lags behind the head), the run fails rather than guessing; rerun it.

The window should be longer than any cooldown period users are likely to set.
Runs under the pipeline lock and recomputes content hashes, so the next deploy
re-uploads metadata for every synced package.`,
	RunE: runBackfillPublishedTimes,
}

func runBackfillPublishedTimes(cmd *cobra.Command, args []string) error {
	if err := acquirePipelineLock(); err != nil {
		return err
	}

	pkgType, _ := cmd.Flags().GetString("type")
	days, _ := cmd.Flags().GetInt("days")
	chunkSize, _ := cmd.Flags().GetInt64("chunk-size")
	if days <= 0 {
		return fmt.Errorf("--days must be positive")
	}
	if chunkSize <= 0 || chunkSize > wporg.SVNLogLimit {
		return fmt.Errorf("--chunk-size must be between 1 and %d", wporg.SVNLogLimit)
	}

	ctx := cmd.Context()
	client := wporg.NewClient(application.Config.Discovery, application.Logger)

	type svnSource struct {
		url     string
		pkgType string
	}
	var sources []svnSource
	if pkgType == "all" || pkgType == "plugin" {
		sources = append(sources, svnSource{"https://plugins.svn.wordpress.org/", "plugin"})
	}
	if pkgType == "all" || pkgType == "theme" {
		sources = append(sources, svnSource{"https://themes.svn.wordpress.org/", "theme"})
	}

	now := time.Now().UTC()
	baseline := now.AddDate(0, 0, -days)

	for _, src := range sources {
		// Scan from the current head, not the discover watermark: the wp.org
		// API can already list versions whose tags are newer than the
		// watermark. Discover re-stamps those again later, which only moves
		// their time forward.
		listing, err := client.ParseSVNListing(ctx, src.url, func(wporg.SVNEntry) error { return nil })
		if err != nil {
			return fmt.Errorf("fetching %s SVN head revision: %w", src.pkgType, err)
		}
		if listing == nil || listing.Revision <= 0 {
			return fmt.Errorf("could not determine %s SVN head revision", src.pkgType)
		}
		endRev := listing.Revision

		application.Logger.Info("scanning SVN changelog for tag changes",
			"type", src.pkgType, "end_rev", endRev, "since", baseline.Format(time.RFC3339))

		changes, err := scanTagChangesSince(ctx, client, application.Logger, src.url, src.pkgType, endRev, chunkSize, baseline, now)
		if err != nil {
			return fmt.Errorf("scanning %s changelog: %w", src.pkgType, err)
		}

		n, err := packages.BackfillPublishedTimes(ctx, application.DB, src.pkgType, changes.Tags, changes.All, baseline, now)
		if err != nil {
			return fmt.Errorf("backfilling %s published times: %w", src.pkgType, err)
		}
		application.Logger.Info("backfilled published times",
			"type", src.pkgType, "packages", n,
			"slugs_with_tag_changes", len(changes.Tags), "slugs_with_all_tags_changed", len(changes.All))
	}

	return nil
}

// svnRetryDelay is the base backoff between SVN changelog fetch attempts.
var svnRetryDelay = 5 * time.Second

// scanTagChangesSince walks the SVN changelog backwards from endRev in chunks
// until it reaches commits older than since, collecting the latest create/change
// date per tag (see wporg.SVNLog.TagChanges). A chunk that can't be fetched, or
// comes back without every revision in its range, fails the scan: skipping
// revisions would stamp affected versions with the window start and shorten
// their cooldown.
func scanTagChangesSince(ctx context.Context, client *wporg.Client, logger *slog.Logger, baseURL, pkgType string, endRev, chunkSize int64, since, now time.Time) (wporg.TagChangeSet, error) {
	acc := wporg.TagChangeSet{
		Tags: make(map[string]map[string]time.Time),
		All:  make(map[string]time.Time),
	}
	for to := endRev; to > 0; {
		from := to - chunkSize + 1
		if from < 1 {
			from = 1
		}

		var svnLog *wporg.SVNLog
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			svnLog, err = client.FetchSVNLog(ctx, baseURL, from, to)
			if err == nil {
				err = checkSVNLogCoverage(svnLog, from, to)
			}
			if err == nil {
				break
			}
			logger.Warn("SVN log chunk fetch failed, retrying",
				"from_rev", from, "to_rev", to, "attempt", attempt, "error", err)
			time.Sleep(time.Duration(attempt) * svnRetryDelay)
		}
		if err != nil {
			return wporg.TagChangeSet{}, err
		}

		mergeTagChanges(acc, svnLog.TagChanges(pkgType), since, now)
		logger.Info("scanned SVN changelog chunk",
			"type", pkgType, "from_rev", from, "to_rev", to, "slugs", len(acc.Tags))

		if !svnLog.MinDate.IsZero() && svnLog.MinDate.Before(since) {
			break
		}
		to = from - 1
	}
	return acc, nil
}

// checkSVNLogCoverage verifies a changelog response holds every revision from
// from to to. Every revision is a commit to the repository root, so a complete
// response has one log item per revision; a lagging replica comes back short
// at the top, and a truncated one short at the bottom.
func checkSVNLogCoverage(l *wporg.SVNLog, from, to int64) error {
	if l.MinRev != from || l.MaxRev != to || int64(len(l.Commits)) != to-from+1 {
		return fmt.Errorf("incomplete SVN log for r%d-r%d: got %d revisions (r%d-r%d)",
			from, to, len(l.Commits), l.MinRev, l.MaxRev)
	}
	return nil
}

// mergeTagChanges folds changes into dst, keeping the latest date per tag and
// per package. Changes before since are dropped; a missing commit date counts
// as now.
func mergeTagChanges(dst, changes wporg.TagChangeSet, since, now time.Time) {
	clamp := func(t time.Time) (time.Time, bool) {
		if t.IsZero() {
			t = now
		}
		return t, !t.Before(since)
	}
	for slug, tags := range changes.Tags {
		for tag, t := range tags {
			t, ok := clamp(t)
			if !ok {
				continue
			}
			if dst.Tags[slug] == nil {
				dst.Tags[slug] = make(map[string]time.Time)
			}
			if t.After(dst.Tags[slug][tag]) {
				dst.Tags[slug][tag] = t
			}
		}
	}
	for slug, t := range changes.All {
		t, ok := clamp(t)
		if ok && t.After(dst.All[slug]) {
			dst.All[slug] = t
		}
	}
}

func init() {
	appCommand(backfillPublishedTimesCmd)
	backfillPublishedTimesCmd.Flags().String("type", "all", "package type (plugin, theme, or all)")
	backfillPublishedTimesCmd.Flags().Int("days", 30, "how far back to scan SVN for tag changes")
	backfillPublishedTimesCmd.Flags().Int64("chunk-size", svnLogChunkSize, "number of SVN revisions per chunk")
	rootCmd.AddCommand(backfillPublishedTimesCmd)
}
