package og

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const uploadConcurrency = 20

// PackageOGRow holds the data needed for OG image generation decisions.
type PackageOGRow struct {
	ID                      int64
	Type                    string
	Name                    string
	DisplayName             string
	Description             string
	CurrentVersion          string
	ActiveInstalls          int64
	WpPackagesInstallsTotal int64
	OGImageGeneratedAt      *string
	OGImageInstalls         int64
	OGImageWpInstalls       int64
}

// FormatInstalls returns a human-readable install count with rounding
// to reduce unnecessary OG image regeneration when counts change slightly.
func FormatInstalls(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.0fk", float64(n)/1_000)
	}
	if n >= 100 {
		rounded := (n / 100) * 100
		return fmt.Sprintf("%d+", rounded)
	}
	if n >= 10 {
		rounded := (n / 10) * 10
		return fmt.Sprintf("%d+", rounded)
	}
	return fmt.Sprintf("%d", n)
}

// getPackagesNeedingOG returns packages that need OG image generation:
// - Never generated (og_image_generated_at IS NULL)
// - Composer install count changed since last generation
func getPackagesNeedingOG(ctx context.Context, db *sql.DB) ([]PackageOGRow, error) {
	q := `SELECT id, type, name, COALESCE(display_name, ''), COALESCE(description, ''),
		COALESCE(current_version, ''), active_installs, wp_packages_installs_total,
		og_image_generated_at, og_image_installs, og_image_wp_installs
		FROM packages
		WHERE is_active = 1
		AND (
			og_image_generated_at IS NULL
			OR wp_packages_installs_total != og_image_wp_installs
		)
		ORDER BY active_installs DESC`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying packages for OG: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var pkgs []PackageOGRow
	for rows.Next() {
		var p PackageOGRow
		if err := rows.Scan(&p.ID, &p.Type, &p.Name, &p.DisplayName, &p.Description,
			&p.CurrentVersion, &p.ActiveInstalls, &p.WpPackagesInstallsTotal,
			&p.OGImageGeneratedAt, &p.OGImageInstalls, &p.OGImageWpInstalls); err != nil {
			return nil, fmt.Errorf("scanning OG row: %w", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, rows.Err()
}

// MarkOGGenerated updates the OG tracking columns after successful generation.
func markOGGenerated(ctx context.Context, db *sql.DB, id, activeInstalls, wpInstalls int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.ExecContext(ctx, `UPDATE packages SET
		og_image_generated_at = ?,
		og_image_installs = ?,
		og_image_wp_installs = ?
		WHERE id = ?`, now, activeInstalls, wpInstalls, id)
	if err != nil {
		return fmt.Errorf("marking OG generated for package %d: %w", id, err)
	}
	return nil
}

// GenerateResult holds the outcome of a generation run.
type GenerateResult struct {
	Generated int
	Skipped   int
	Errors    int
}

// GenerateAll generates OG images for all packages that need them.
// Rendering and uploading run concurrently; DB writes stay serial.
func GenerateAll(ctx context.Context, db *sql.DB, uploader *Uploader, logger *slog.Logger) (GenerateResult, error) {
	pkgs, err := getPackagesNeedingOG(ctx, db)
	if err != nil {
		return GenerateResult{}, err
	}

	if len(pkgs) == 0 {
		logger.Info("no packages need OG image generation")
		return GenerateResult{}, nil
	}

	// Filter out packages whose formatted display values haven't changed.
	var work []PackageOGRow
	var skipped int
	for _, pkg := range pkgs {
		if pkg.OGImageGeneratedAt != nil &&
			FormatInstalls(pkg.WpPackagesInstallsTotal) == FormatInstalls(pkg.OGImageWpInstalls) {
			skipped++
			continue
		}
		work = append(work, pkg)
	}

	logger.Info("generating OG images", "packages", len(work), "skipped", skipped)

	if len(work) == 0 {
		return GenerateResult{Skipped: skipped}, nil
	}

	type dbUpdate struct {
		id, installs, wpInstalls int64
	}

	var generated, errCount atomic.Int64
	ch := make(chan PackageOGRow)
	updates := make(chan dbUpdate, uploadConcurrency*2)

	// Render + upload concurrently.
	var wg sync.WaitGroup
	for range uploadConcurrency {
		wg.Go(func() {
			for pkg := range ch {
				data := PackageData{
					DisplayName:        pkg.DisplayName,
					Name:               pkg.Name,
					Type:               pkg.Type,
					CurrentVersion:     pkg.CurrentVersion,
					Description:        pkg.Description,
					ActiveInstalls:     FormatInstalls(pkg.ActiveInstalls),
					WpPackagesInstalls: FormatInstalls(pkg.WpPackagesInstallsTotal),
				}

				pngBytes, err := GeneratePackageImage(data)
				if err != nil {
					logger.Error("generating OG image", "package", pkg.Name, "error", err)
					errCount.Add(1)
					continue
				}

				key := fmt.Sprintf("social/%s/%s.png", pkg.Type, pkg.Name)
				if err := uploader.Upload(ctx, key, pngBytes); err != nil {
					logger.Error("uploading OG image", "package", pkg.Name, "error", err)
					errCount.Add(1)
					continue
				}

				updates <- dbUpdate{pkg.ID, pkg.ActiveInstalls, pkg.WpPackagesInstallsTotal}

				if n := generated.Add(1); n%500 == 0 {
					logger.Info("OG generation progress", "generated", n, "total", len(work))
				}
			}
		})
	}

	// Serial DB writer to avoid SQLite contention.
	var dbErrors atomic.Int64
	var dbWg sync.WaitGroup
	dbWg.Go(func() {
		for u := range updates {
			if err := markOGGenerated(ctx, db, u.id, u.installs, u.wpInstalls); err != nil {
				logger.Error("marking OG generated", "id", u.id, "error", err)
				dbErrors.Add(1)
			}
		}
	})

	for _, pkg := range work {
		if ctx.Err() != nil {
			break
		}
		ch <- pkg
	}
	close(ch)
	wg.Wait()
	close(updates)
	dbWg.Wait()

	return GenerateResult{
		Generated: int(generated.Load()),
		Skipped:   skipped,
		Errors:    int(errCount.Load() + dbErrors.Load()),
	}, nil
}
