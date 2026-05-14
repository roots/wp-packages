package cmd

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/roots/wp-packages/internal/packages"
	"github.com/roots/wp-packages/internal/reports"
	"github.com/roots/wp-packages/internal/wporg"
)

var checkStatusCmd = &cobra.Command{
	Use:   "check-status",
	Short: "Re-check all packages against the WordPress.org API to detect closures and re-openings",
	RunE:  runCheckStatus,
}

func runCheckStatus(cmd *cobra.Command, args []string) error {
	pkgType, _ := cmd.Flags().GetString("type")
	concurrency, _ := cmd.Flags().GetInt("concurrency")

	if concurrency <= 0 {
		concurrency = application.Config.Discovery.Concurrency
	}

	ctx := cmd.Context()
	started := time.Now().UTC()

	// Cleanup: keep change rows only for the most recent runs that the status
	// page displays; drop the rest.
	if _, err := application.DB.ExecContext(ctx,
		`DELETE FROM status_check_changes WHERE status_check_id NOT IN (
			SELECT id FROM status_checks ORDER BY started_at DESC LIMIT ?
		)`, packages.StatusCheckDisplayLimit); err != nil {
		application.Logger.Warn("failed to cleanup old status check changes", "error", err)
	}

	// Record the run as started.
	runID, err := packages.StartStatusCheck(ctx, application.DB, started)
	if err != nil {
		return fmt.Errorf("recording status check: %w", err)
	}

	var checked, deactivated, tombstoned, reactivated, failed atomic.Int64
	var runErr error
	defer func() {
		_ = packages.FinishStatusCheck(ctx, application.DB, runID, started,
			checked.Load(), deactivated.Load()+tombstoned.Load(), reactivated.Load(), failed.Load(), runErr)
	}()

	pkgs, err := packages.GetAllPackages(ctx, application.DB, pkgType)
	if err != nil {
		runErr = err
		return fmt.Errorf("querying packages: %w", err)
	}

	if len(pkgs) == 0 {
		application.Logger.Info("no packages to check")
		return nil
	}

	application.Logger.Info("checking package status", "count", len(pkgs), "concurrency", concurrency)

	client := wporg.NewClient(application.Config.Discovery, application.Logger)

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for _, p := range pkgs {
		p := p
		g.Go(func() error {
			var fetchErr error

			if p.Type == "plugin" {
				_, fetchErr = client.FetchPlugin(gCtx, p.Name)
			} else {
				_, fetchErr = client.FetchTheme(gCtx, p.Name)
			}

			if fetchErr != nil {
				if errors.Is(fetchErr, wporg.ErrClosedPermanent) {
					if err := packages.MarkPermanentlyClosed(gCtx, application.DB, p.ID); err != nil {
						application.Logger.Warn("failed to tombstone", "type", p.Type, "name", p.Name, "error", err)
						failed.Add(1)
					} else {
						tombstoned.Add(1)
						application.Logger.Info("tombstoned permanently-closed package", "type", p.Type, "name", p.Name)
						packages.RecordStatusCheckChange(gCtx, application.DB, runID, p.Type, p.Name, "tombstoned")
					}
				} else if errors.Is(fetchErr, wporg.ErrNotFound) {
					if p.IsActive {
						if err := packages.DeactivatePackage(gCtx, application.DB, p.ID); err != nil {
							application.Logger.Warn("failed to deactivate", "type", p.Type, "name", p.Name, "error", err)
							failed.Add(1)
						} else {
							deactivated.Add(1)
							application.Logger.Info("deactivated closed package", "type", p.Type, "name", p.Name)
							packages.RecordStatusCheckChange(gCtx, application.DB, runID, p.Type, p.Name, "deactivated")
						}
					}
				} else {
					application.Logger.Warn("failed to check", "type", p.Type, "name", p.Name, "error", fetchErr)
					failed.Add(1)
				}
			} else if !p.IsActive {
				if err := packages.ReactivatePackage(gCtx, application.DB, p.ID); err != nil {
					application.Logger.Warn("failed to reactivate", "type", p.Type, "name", p.Name, "error", err)
					failed.Add(1)
				} else {
					reactivated.Add(1)
					application.Logger.Info("reactivated reopened package", "type", p.Type, "name", p.Name)
					packages.RecordStatusCheckChange(gCtx, application.DB, runID, p.Type, p.Name, "reactivated")
				}
			}

			checked.Add(1)
			total := checked.Load()
			if total%1000 == 0 {
				application.Logger.Info("check-status progress",
					"checked", total,
					"total", len(pkgs),
					"deactivated", deactivated.Load(),
					"tombstoned", tombstoned.Load(),
					"reactivated", reactivated.Load(),
					"failed", failed.Load(),
				)
			}
			return nil
		})
	}

	runErr = g.Wait()

	application.Logger.Info("check-status complete",
		"checked", checked.Load(),
		"deactivated", deactivated.Load(),
		"tombstoned", tombstoned.Load(),
		"reactivated", reactivated.Load(),
		"failed", failed.Load(),
	)

	if err := reports.TrackMassClosures(ctx, application.DB); err != nil {
		application.Logger.Warn("failed to track mass closures", "error", err)
	}

	return runErr
}

func init() {
	appCommand(checkStatusCmd)
	checkStatusCmd.Flags().String("type", "all", "package type (plugin, theme, or all)")
	checkStatusCmd.Flags().Int("concurrency", 0, "concurrent API requests (0 = use config default)")
	rootCmd.AddCommand(checkStatusCmd)
}
