package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/roots/wp-packages/internal/composer"
	"github.com/roots/wp-packages/internal/packages"
)

var rehashCmd = &cobra.Command{
	Use:   "rehash",
	Short: "Recompute content_hash for all active packages from stored data",
	Long: `Recomputes content_hash for every active package by serializing the data
already in SQLite. Makes no wp.org API calls and does not touch R2.

Two reasons to run this:

  - After a change to the serialization format or the hash function, so that
    stored hashes describe the current output rather than the old one.
  - To populate content_hash for rows that have never been through an update
    (content_hash IS NULL). Those rows are invisible to the sync step's diff
    query, so they would otherwise never be uploaded again.

Packages whose recomputed hash differs from the stored one are left dirty
(content_hash != deployed_hash) and will be picked up by the next sync. Run
--dry-run first to see how many uploads that implies.`,
	RunE: runRehash,
}

func runRehash(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	pkgs, err := packages.GetAllActiveForHashing(ctx, application.DB)
	if err != nil {
		return fmt.Errorf("loading packages: %w", err)
	}

	application.Logger.Info("rehash: loaded packages", "count", len(pkgs), "dry_run", dryRun)

	var changed, unchanged, wasNull, failed int
	started := time.Now()

	for _, p := range pkgs {
		newHash, err := composer.HashContent(p.Type, p.Name, p.VersionsJSON, p.ComposerMeta())
		if err != nil {
			application.Logger.Warn("rehash: serialization failed",
				"type", p.Type, "name", p.Name, "error", err)
			failed++
			continue
		}

		switch {
		case p.ContentHash == nil:
			wasNull++
		case *p.ContentHash == newHash:
			unchanged++
			continue
		default:
			changed++
		}

		if dryRun {
			continue
		}

		// content_changed_at deliberately not advanced: the serialized output
		// may differ, but the underlying package data did not change, and the
		// changes feed reports data changes to Composer clients.
		if _, err := application.DB.ExecContext(ctx,
			`UPDATE packages SET content_hash = ? WHERE id = ?`, newHash, p.ID); err != nil {
			return fmt.Errorf("updating content_hash for %s/%s: %w", p.Type, p.Name, err)
		}
	}

	application.Logger.Info("rehash complete",
		"total", len(pkgs),
		"changed", changed,
		"newly_hashed", wasNull,
		"unchanged", unchanged,
		"failed", failed,
		"dry_run", dryRun,
		"duration", time.Since(started).String())

	if dryRun {
		application.Logger.Info("rehash: dry run, no changes written",
			"would_be_dirty", changed+wasNull)
	}

	return nil
}

func init() {
	appCommand(rehashCmd)
	rehashCmd.Flags().Bool("dry-run", false, "report what would change without writing")
	rootCmd.AddCommand(rehashCmd)
}
