package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/roots/wp-packages/internal/deploy"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Upload changed packages from the database to R2",
	Long: `Queries packages where content_hash != deployed_hash, serializes their
Composer p2 JSON, uploads to R2, and stamps deployed_hash. Deletes p2 files for
packages that have been deactivated since their last upload.

This is the DB-driven replacement for the build + deploy pipeline. It does not
read or write the build directory.

Use --dry-run to report what would be uploaded and deleted without touching R2
or the database. Running it directly after a normal deploy is the intended way
to verify this path agrees with the one it replaces: a healthy result is zero
uploads.

Deletion is capped per run — see --max-deletes.`,
	RunE: runSync,
}

func runSync(cmd *cobra.Command, args []string) error {
	if !application.Config.R2.Enabled {
		return fmt.Errorf("R2 is not enabled in config")
	}

	dryRun, _ := cmd.Flags().GetBool("dry-run")
	maxDeletes, _ := cmd.Flags().GetInt("max-deletes")

	result, err := deploy.Sync(
		cmd.Context(),
		application.DB,
		application.Config.R2,
		application.Config.AppURL,
		deploy.SyncOptions{DryRun: dryRun, MaxDeletes: maxDeletes},
		application.Logger,
	)
	if err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}

	if result.DeletesSkipped > 0 {
		return fmt.Errorf("refused to delete %d deactivated packages (limit %d); "+
			"confirm the deactivations are genuine, then re-run with --max-deletes",
			result.DeletesSkipped, maxDeletes)
	}

	return nil
}

func init() {
	appCommand(syncCmd)
	f := syncCmd.Flags()
	f.Bool("dry-run", false, "report what would be uploaded and deleted without touching R2 or the DB")
	f.Int("max-deletes", deploy.DefaultMaxDeletes,
		"refuse the run if more than this many packages would be deleted from R2 (-1 for no limit)")
	rootCmd.AddCommand(syncCmd)
}
