package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/roots/wp-packages/internal/deploy"
	"github.com/spf13/cobra"
)

// pipelineLockFile holds the lock file reference for the lifetime of the process,
// preventing the GC finalizer from closing the fd and releasing the lock.
var pipelineLockFile *os.File

// acquirePipelineLock ensures only one pipeline runs at a time.
// The lock file is placed next to the database file.
func acquirePipelineLock() error {
	lockPath := filepath.Join(filepath.Dir(application.Config.DB.Path), "pipeline.lock")
	return acquireLock(lockPath)
}

// ensurePipelineLock takes the pipeline lock for a command run on its own, so
// it can't interleave with a scheduled pipeline (e.g. a manual update writing
// back published times that discover just reset). When the command runs as a
// pipeline step, this process already holds the lock.
func ensurePipelineLock() error {
	if pipelineLockFile != nil {
		return nil
	}
	return acquirePipelineLock()
}

// acquireLock acquires an exclusive file lock at lockPath.
func acquireLock(lockPath string) error {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("pipeline lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return fmt.Errorf("pipeline already running (could not acquire %s)", lockPath)
	}
	pipelineLockFile = f
	return nil
}

var pipelineCmd = &cobra.Command{
	Use:   "pipeline",
	Short: "Run discover → update → build → deploy",
	RunE:  runPipeline,
}

// pipelineBuildID is set by runPipeline so that runBuild can UPDATE the
// existing "running" row instead of INSERTing a new one.
var pipelineBuildID string

func runPipeline(cmd *cobra.Command, args []string) error {
	// Acquire a system-wide file lock so only one pipeline runs at a time.
	if err := acquirePipelineLock(); err != nil {
		return err
	}

	skipDiscover, _ := cmd.Flags().GetBool("skip-discover")
	skipDeploy, _ := cmd.Flags().GetBool("skip-deploy")
	discoverSource, _ := cmd.Flags().GetString("discover-source")
	buildIDFlag, _ := cmd.Flags().GetString("build-id")

	ctx := cmd.Context()
	started := time.Now().UTC()

	// Mark any stale "running" builds (dead PID) as cancelled.
	markStaleBuildsCancelled(ctx, application.DB)

	// When triggered from the admin UI, a "running" row already exists —
	// claim it by updating the PID to our own and verify it exists. When
	// invoked from the CLI, insert a new row.
	var buildID string
	if buildIDFlag != "" {
		buildID = buildIDFlag
		res, err := application.DB.ExecContext(ctx, `
			UPDATE builds SET pid = ? WHERE id = ? AND status = 'running'`,
			os.Getpid(), buildID)
		if err != nil {
			return fmt.Errorf("claiming build %s: %w", buildID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("build %s not found or not in running state", buildID)
		}
	} else {
		buildID = started.Format("20060102-150405")
		_, err := application.DB.ExecContext(ctx, `
			INSERT INTO builds (id, started_at, status, pid,
				packages_total, packages_changed, packages_skipped,
				artifact_count, root_hash, manifest_json)
			VALUES (?, ?, 'running', ?, 0, 0, 0, 0, '', '{}')`,
			buildID,
			started.Format(time.RFC3339),
			os.Getpid(),
		)
		if err != nil {
			return fmt.Errorf("recording running build: %w", err)
		}
	}
	pipelineBuildID = buildID
	defer func() { pipelineBuildID = "" }()

	if err := executePipelineSteps(cmd, ctx, skipDiscover, skipDeploy, discoverSource); err != nil {
		recordFailedBuild(cmd, started, err)
		return err
	}

	now := time.Now().UTC()
	d := pipelineStepDurations
	_, dbErr := application.DB.ExecContext(ctx, `
		UPDATE builds SET status = 'completed', finished_at = ?, duration_seconds = ?,
			discover_seconds = ?, update_seconds = ?, build_seconds = ?, deploy_seconds = ?,
			r2_upload_seconds = ?
		WHERE id = ?`,
		now.Format(time.RFC3339),
		int(now.Sub(started).Seconds()),
		d.Discover, d.Update, d.Build, d.Deploy, d.R2Upload,
		buildID,
	)
	if dbErr != nil {
		application.Logger.Warn("failed to record completed build", "error", dbErr)
	}

	application.Logger.Info("pipeline: complete")
	return nil
}

// stepDurations holds per-step timing recorded during pipeline execution.
type stepDurations struct {
	Discover *int
	Update   *int
	Build    *int
	Deploy   *int
	R2Upload *int
}

// pipelineStepDurations is set by executePipelineSteps so that recordFailedBuild
// and runBuild can persist whatever step timings were captured before failure.
var pipelineStepDurations stepDurations

func executePipelineSteps(cmd *cobra.Command, ctx context.Context, skipDiscover, skipDeploy bool, discoverSource string) error {
	pipelineStepDurations = stepDurations{}

	if !skipDiscover {
		application.Logger.Info("pipeline: running discover")
		discoverCmd.SetContext(ctx)
		_ = discoverCmd.Flags().Set("source", discoverSource)
		stepStart := time.Now()
		if err := runDiscover(discoverCmd, nil); err != nil {
			return fmt.Errorf("discover: %w", err)
		}
		pipelineStepDurations.Discover = new(int(time.Since(stepStart).Seconds()))
	}

	application.Logger.Info("pipeline: running update")
	updateCmd.SetContext(ctx)
	stepStart := time.Now()
	if err := runUpdate(updateCmd, nil); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	pipelineStepDurations.Update = new(int(time.Since(stepStart).Seconds()))

	application.Logger.Info("pipeline: running build")
	buildCmd.SetContext(ctx)
	stepStart = time.Now()
	if err := runBuild(buildCmd, nil); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	pipelineStepDurations.Build = new(int(time.Since(stepStart).Seconds()))

	if !skipDeploy {
		application.Logger.Info("pipeline: running deploy")
		deployCmd.SetContext(ctx)
		stepStart = time.Now()
		deployErr := runDeploy(deployCmd, nil)
		pipelineStepDurations.Deploy = new(int(time.Since(stepStart).Seconds()))
		pipelineStepDurations.R2Upload = deployR2SyncSeconds
		if deployErr != nil {
			return fmt.Errorf("deploy: %w", deployErr)
		}

		// Clean up old builds after a successful deploy.
		repoDir := filepath.Join("storage", "repository")
		if removed, err := deploy.Cleanup(repoDir, 5, application.Logger); err != nil {
			application.Logger.Warn("pipeline: local cleanup failed", "error", err)
		} else if removed > 0 {
			application.Logger.Info("pipeline: local cleanup done", "removed", removed)
		}

		// Legacy releases/ and p/ objects on R2 are orphaned and should be
		// deleted manually via AWS CLI when ready.
	}

	return nil
}

func recordFailedBuild(cmd *cobra.Command, started time.Time, pipelineErr error) {
	now := time.Now().UTC()
	d := pipelineStepDurations
	_, dbErr := application.DB.ExecContext(cmd.Context(), `
		UPDATE builds SET status = 'failed', finished_at = ?, duration_seconds = ?,
			error_message = ?, discover_seconds = ?, update_seconds = ?,
			build_seconds = ?, deploy_seconds = ?, r2_upload_seconds = ?
		WHERE id = ?`,
		now.Format(time.RFC3339),
		int(now.Sub(started).Seconds()),
		pipelineErr.Error(),
		d.Discover, d.Update, d.Build, d.Deploy, d.R2Upload,
		pipelineBuildID,
	)
	if dbErr != nil {
		application.Logger.Warn("failed to record failed build in database", "error", dbErr)
	}
}

// markStaleBuildsCancelled finds builds with status "running" whose PID is no
// longer alive and marks them as "cancelled".
func markStaleBuildsCancelled(ctx context.Context, db *sql.DB) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, pid FROM builds WHERE status = 'running' AND pid IS NOT NULL`)
	if err != nil {
		return
	}

	// Collect stale IDs first — writing while iterating deadlocks SQLite.
	var staleIDs []string
	for rows.Next() {
		var id string
		var pid int
		if err := rows.Scan(&id, &pid); err != nil {
			continue
		}
		// Signal 0 checks if the process exists without sending a real signal.
		// ESRCH = no such process (safe to cancel). EPERM = process exists but
		// we lack permission (do not cancel). Any other error is unexpected.
		if err := syscall.Kill(pid, 0); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				staleIDs = append(staleIDs, id)
			} else {
				application.Logger.Warn("stale build check: unexpected kill(0) error", "build_id", id, "pid", pid, "error", err)
			}
		}
	}
	_ = rows.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range staleIDs {
		_, _ = db.ExecContext(ctx, `UPDATE builds SET status = 'cancelled', finished_at = ? WHERE id = ?`, now, id)
	}
}

func init() {
	appCommand(pipelineCmd)
	pipelineCmd.Flags().String("discover-source", "config", "discovery source (config or svn)")
	pipelineCmd.Flags().Bool("skip-discover", false, "skip the discover step")
	pipelineCmd.Flags().Bool("skip-deploy", false, "skip the deploy step")
	pipelineCmd.Flags().String("build-id", "", "pre-allocated build ID (set by admin UI trigger)")
	rootCmd.AddCommand(pipelineCmd)
}
