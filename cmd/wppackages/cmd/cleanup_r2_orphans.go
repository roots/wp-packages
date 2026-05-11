package cmd

import (
	"github.com/spf13/cobra"

	"github.com/roots/wp-packages/internal/deploy"
)

var cleanupR2OrphansCmd = &cobra.Command{
	Use:   "cleanup-r2-orphans",
	Short: "Bulk-delete R2 files for deactivated packages (one-off historical sweep)",
	Long: `Deletes R2 files for all packages where is_active=0 AND deployed_hash IS NOT NULL,
using the S3 DeleteObjects batch API (up to 1000 keys per request) parallelized
across cfg.R2.Concurrency workers.

Intended for one-off cleanup of historical orphans. The regular per-deploy path
in SyncToR2 uses a serial loop, which is fine for the small number of
deactivations per 5-min pipeline cycle but far too slow for an initial backfill.

Safe to interrupt and re-run — successfully-cleaned packages have their
deployed_hash cleared, so a subsequent run only picks up what's left.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		result, err := deploy.BulkDeleteDeactivated(
			cmd.Context(), application.DB, application.Config.R2, application.Logger,
		)
		if err != nil {
			return err
		}
		application.Logger.Info("cleanup-r2-orphans complete",
			"packages", result.Packages,
			"keys_deleted", result.KeysDeleted,
			"keys_failed", result.KeysFailed)
		return nil
	},
}

func init() {
	appCommand(cleanupR2OrphansCmd)
	rootCmd.AddCommand(cleanupR2OrphansCmd)
}
