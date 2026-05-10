package packages

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/roots/wp-packages/internal/composer"
	"github.com/roots/wp-packages/internal/version"
)

// ComposerMeta builds a composer.PackageMeta from a Package's nullable DB fields.
func (p *Package) ComposerMeta() composer.PackageMeta {
	meta := composer.PackageMeta{TrunkRevision: p.TrunkRevision}
	if p.Description != nil {
		meta.Description = *p.Description
	}
	if p.Homepage != nil {
		meta.Homepage = *p.Homepage
	}
	if p.Author != nil {
		meta.Author = *p.Author
	}
	if p.LastCommitted != nil {
		meta.LastUpdated = p.LastCommitted.Format(time.RFC3339)
	}
	return meta
}

type Package struct {
	ID                      int64
	Type                    string
	Name                    string
	DisplayName             *string
	Description             *string
	Author                  *string
	Homepage                *string
	SlugURL                 *string
	VersionsJSON            string
	Downloads               int64
	ActiveInstalls          int64
	CurrentVersion          *string
	WporgVersion            *string
	Rating                  *float64
	NumRatings              int
	IsActive                bool
	PermanentlyClosed       bool
	LastCommitted           *time.Time
	LastSyncedAt            *time.Time
	LastSyncRunID           *int64
	TrunkRevision           *int64
	ContentHash             *string
	DeployedHash            *string
	ContentChangedAt        *time.Time
	WpPackagesInstallsTotal int
	WpPackagesInstalls30d   int
	LastInstalledAt         *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time

	// RawVersions holds the pre-normalization version map from the API.
	// Not persisted directly — normalized into VersionsJSON before storage.
	RawVersions map[string]string `json:"-"`
}

// NormalizeAndStoreVersions normalizes raw versions and serializes to VersionsJSON.
// It also sets CurrentVersion to the highest available version.
// Returns the number of valid versions.
func (p *Package) NormalizeAndStoreVersions() (int, error) {
	if p.RawVersions == nil {
		p.VersionsJSON = "{}"
		return 0, nil
	}

	normalized := version.NormalizeVersions(p.RawVersions)

	data, err := json.Marshal(normalized)
	if err != nil {
		return 0, fmt.Errorf("marshaling versions: %w", err)
	}
	p.VersionsJSON = string(data)

	if latest := version.Latest(normalized); latest != "" {
		p.CurrentVersion = &latest
	}

	return len(normalized), nil
}

func timeStr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

type ClosureEvent struct {
	ID          int64     `json:"id"`
	VendorName  string    `json:"vendor_name"`
	VendorSlug  string    `json:"vendor_slug"`
	DetectedAt  time.Time `json:"detected_at"`
	PluginSlugs []string  `json:"plugin_slugs"`
	PluginCount int       `json:"plugin_count"`
}

type ClosurePluginStatus struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name,omitempty"`
	IsActive    bool   `json:"is_active"`
	IsClosed    bool   `json:"is_closed"`
}

func GetClosureEvents(ctx context.Context, db *sql.DB, page, perPage int) ([]ClosureEvent, int, error) {
	var total int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM closure_events").Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, vendor_name, vendor_slug, detected_at, plugin_slugs, plugin_count
		FROM closure_events
		ORDER BY detected_at DESC, id DESC
		LIMIT ? OFFSET ?`, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var events []ClosureEvent
	for rows.Next() {
		e, err := scanClosureEvent(rows)
		if err != nil {
			return nil, 0, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return events, total, nil
}

func GetVendorClosureEvents(ctx context.Context, db *sql.DB, vendorSlug string) ([]ClosureEvent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, vendor_name, vendor_slug, detected_at, plugin_slugs, plugin_count
		FROM closure_events
		WHERE vendor_slug = ?
		ORDER BY detected_at DESC, id DESC`, vendorSlug)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []ClosureEvent
	for rows.Next() {
		e, err := scanClosureEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func scanClosureEvent(rows *sql.Rows) (ClosureEvent, error) {
	var e ClosureEvent
	var detectedAtStr, slugsJSON string
	if err := rows.Scan(&e.ID, &e.VendorName, &e.VendorSlug, &detectedAtStr, &slugsJSON, &e.PluginCount); err != nil {
		return e, err
	}
	t, err := time.Parse(time.RFC3339, detectedAtStr)
	if err != nil {
		return e, fmt.Errorf("closure_events.detected_at (id=%d): %w", e.ID, err)
	}
	e.DetectedAt = t
	if err := json.Unmarshal([]byte(slugsJSON), &e.PluginSlugs); err != nil {
		return e, fmt.Errorf("closure_events.plugin_slugs (id=%d): %w", e.ID, err)
	}
	return e, nil
}

// GetClosurePluginStatuses returns a pointer map so missing keys are nil
// (templates can use {{if $s}} to detect "not in DB"). A value map would
// return the zero ClosurePluginStatus for missing keys, which Go templates
// treat as truthy — silently mislabeling unknown slugs as "Closed".
func GetClosurePluginStatuses(ctx context.Context, db *sql.DB, slugs []string) (map[string]*ClosurePluginStatus, error) {
	if len(slugs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(slugs))
	args := make([]any, len(slugs))
	for i, s := range slugs {
		placeholders[i] = "?"
		args[i] = s
	}

	query := fmt.Sprintf(`
		SELECT name, COALESCE(display_name, ''), is_active, permanently_closed
		FROM packages
		WHERE type = 'plugin' AND name IN (%s)`, strings.Join(placeholders, ","))

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	statuses := make(map[string]*ClosurePluginStatus)
	for rows.Next() {
		var name, displayName string
		var isActive, permanentlyClosed bool
		if err := rows.Scan(&name, &displayName, &isActive, &permanentlyClosed); err != nil {
			return nil, err
		}
		statuses[name] = &ClosurePluginStatus{
			Slug:        name,
			DisplayName: displayName,
			IsActive:    isActive,
			IsClosed:    permanentlyClosed,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return statuses, nil
}

// UpsertPackage inserts or updates a package record by (type, name).
func UpsertPackage(ctx context.Context, db *sql.DB, pkg *Package) error {
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := db.ExecContext(ctx, `
		INSERT INTO packages (
			type, name, display_name, description, author, homepage, slug_url,
			versions_json, downloads, active_installs,
			current_version, wporg_version, rating, num_ratings, is_active,
			last_committed, last_synced_at, last_sync_run_id,
			content_hash, content_changed_at,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(type, name) DO UPDATE SET
			display_name = excluded.display_name,
			description = excluded.description,
			author = excluded.author,
			homepage = excluded.homepage,
			slug_url = excluded.slug_url,
			versions_json = excluded.versions_json,
			downloads = excluded.downloads,
			active_installs = excluded.active_installs,
			current_version = excluded.current_version,
			wporg_version = excluded.wporg_version,
			rating = excluded.rating,
			num_ratings = excluded.num_ratings,
			is_active = excluded.is_active,
			last_committed = CASE
				WHEN excluded.last_committed > COALESCE(packages.last_committed, '')
				THEN excluded.last_committed
				ELSE packages.last_committed
			END,
			last_synced_at = COALESCE(excluded.last_synced_at, packages.last_synced_at),
			last_sync_run_id = COALESCE(excluded.last_sync_run_id, packages.last_sync_run_id),
			content_hash = COALESCE(excluded.content_hash, packages.content_hash),
			content_changed_at = COALESCE(excluded.content_changed_at, packages.content_changed_at),
			updated_at = excluded.updated_at`,
		pkg.Type, pkg.Name, pkg.DisplayName, pkg.Description, pkg.Author,
		pkg.Homepage, pkg.SlugURL, pkg.VersionsJSON,
		pkg.Downloads, pkg.ActiveInstalls, pkg.CurrentVersion, pkg.WporgVersion, pkg.Rating,
		pkg.NumRatings, boolToInt(pkg.IsActive),
		timeStr(pkg.LastCommitted), timeStr(pkg.LastSyncedAt), pkg.LastSyncRunID,
		pkg.ContentHash, timeStr(pkg.ContentChangedAt),
		now, now,
	)
	if err != nil {
		return fmt.Errorf("upserting package %s/%s: %w", pkg.Type, pkg.Name, err)
	}
	return nil
}

// UpsertShellPackage creates a minimal package record (for SVN discovery) or
// updates last_committed if the new date is more recent.
func UpsertShellPackage(ctx context.Context, db *sql.DB, pkgType, name string, lastCommitted *time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := db.ExecContext(ctx, `
		INSERT INTO packages (type, name, last_committed, is_active, versions_json, created_at, updated_at)
		VALUES (?, ?, ?, 1, '{}', ?, ?)
		ON CONFLICT(type, name) DO UPDATE SET
			last_committed = CASE
				WHEN excluded.last_committed > COALESCE(packages.last_committed, '')
				THEN excluded.last_committed
				ELSE packages.last_committed
			END,
			updated_at = excluded.updated_at
		WHERE packages.permanently_closed = 0`,
		pkgType, name, timeStr(lastCommitted), now, now,
	)
	if err != nil {
		return fmt.Errorf("upserting shell package %s/%s: %w", pkgType, name, err)
	}
	return nil
}

// ShellEntry holds minimal package data for batch SVN discovery upserts.
type ShellEntry struct {
	Type          string
	Name          string
	LastCommitted *time.Time
}

// BatchUpsertShellPackages inserts or updates shell package records in a single transaction.
func BatchUpsertShellPackages(ctx context.Context, db *sql.DB, entries []ShellEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO packages (type, name, last_committed, is_active, versions_json, created_at, updated_at)
		VALUES (?, ?, ?, 1, '{}', ?, ?)
		ON CONFLICT(type, name) DO UPDATE SET
			last_committed = CASE
				WHEN excluded.last_committed > COALESCE(packages.last_committed, '')
				THEN excluded.last_committed
				ELSE packages.last_committed
			END,
			updated_at = excluded.updated_at
		WHERE packages.permanently_closed = 0`)
	if err != nil {
		return fmt.Errorf("preparing statement: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, e := range entries {
		if _, err := stmt.ExecContext(ctx, e.Type, e.Name, timeStr(e.LastCommitted), now, now); err != nil {
			return fmt.Errorf("upserting shell package %s/%s: %w", e.Type, e.Name, err)
		}
	}
	return tx.Commit()
}

// BatchUpsertPackages inserts or updates full package records in a single transaction.
func BatchUpsertPackages(ctx context.Context, db *sql.DB, pkgs []*Package) error {
	if len(pkgs) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO packages (
			type, name, display_name, description, author, homepage, slug_url,
			versions_json, downloads, active_installs,
			current_version, wporg_version, rating, num_ratings, is_active,
			last_committed, last_synced_at, last_sync_run_id,
			content_hash, content_changed_at,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(type, name) DO UPDATE SET
			display_name = excluded.display_name,
			description = excluded.description,
			author = excluded.author,
			homepage = excluded.homepage,
			slug_url = excluded.slug_url,
			versions_json = excluded.versions_json,
			downloads = excluded.downloads,
			active_installs = excluded.active_installs,
			current_version = excluded.current_version,
			wporg_version = excluded.wporg_version,
			rating = excluded.rating,
			num_ratings = excluded.num_ratings,
			is_active = excluded.is_active,
			last_committed = CASE
				WHEN excluded.last_committed > COALESCE(packages.last_committed, '')
				THEN excluded.last_committed
				ELSE packages.last_committed
			END,
			last_synced_at = COALESCE(excluded.last_synced_at, packages.last_synced_at),
			last_sync_run_id = COALESCE(excluded.last_sync_run_id, packages.last_sync_run_id),
			content_hash = COALESCE(excluded.content_hash, packages.content_hash),
			content_changed_at = COALESCE(excluded.content_changed_at, packages.content_changed_at),
			updated_at = excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("preparing statement: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, pkg := range pkgs {
		if _, err := stmt.ExecContext(ctx,
			pkg.Type, pkg.Name, pkg.DisplayName, pkg.Description, pkg.Author,
			pkg.Homepage, pkg.SlugURL, pkg.VersionsJSON,
			pkg.Downloads, pkg.ActiveInstalls, pkg.CurrentVersion, pkg.WporgVersion, pkg.Rating,
			pkg.NumRatings, boolToInt(pkg.IsActive),
			timeStr(pkg.LastCommitted), timeStr(pkg.LastSyncedAt), pkg.LastSyncRunID,
			pkg.ContentHash, timeStr(pkg.ContentChangedAt),
			now, now,
		); err != nil {
			return fmt.Errorf("upserting package %s/%s: %w", pkg.Type, pkg.Name, err)
		}
	}
	return tx.Commit()
}

type UpdateQueryOpts struct {
	Type            string
	Name            string
	Names           []string // filter to these slugs only
	Force           bool
	IncludeInactive bool
	Limit           int
}

// GetPackagesNeedingUpdate returns packages that should be updated.
func GetPackagesNeedingUpdate(ctx context.Context, db *sql.DB, opts UpdateQueryOpts) ([]*Package, error) {
	query := `SELECT id, type, name, last_committed, last_synced_at, is_active, versions_json, content_hash, trunk_revision FROM packages WHERE permanently_closed = 0`
	var args []any

	if opts.Name != "" {
		query += ` AND name = ?`
		args = append(args, opts.Name)
	}

	if len(opts.Names) > 0 {
		placeholders := make([]string, len(opts.Names))
		for i, n := range opts.Names {
			placeholders[i] = "?"
			args = append(args, n)
		}
		query += ` AND name IN (` + strings.Join(placeholders, ",") + `)`
	}

	if opts.Type != "" && opts.Type != "all" {
		query += ` AND type = ?`
		args = append(args, opts.Type)
	}

	if !opts.Force && opts.Name == "" {
		if opts.IncludeInactive {
			query += ` AND (last_synced_at IS NULL OR last_committed > last_synced_at OR (is_active = 0 AND (last_synced_at IS NULL OR last_synced_at < datetime('now', '-30 days'))))`
		} else {
			query += ` AND is_active = 1 AND (last_synced_at IS NULL OR last_committed > last_synced_at)`
		}
	} else if !opts.IncludeInactive && opts.Name == "" {
		query += ` AND is_active = 1`
	}

	query += ` ORDER BY last_synced_at ASC NULLS FIRST`

	if opts.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, opts.Limit)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying packages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var pkgs []*Package
	for rows.Next() {
		var p Package
		var isActive int
		var lastCommitted, lastSyncedAt *string
		if err := rows.Scan(&p.ID, &p.Type, &p.Name, &lastCommitted, &lastSyncedAt, &isActive, &p.VersionsJSON, &p.ContentHash, &p.TrunkRevision); err != nil {
			return nil, fmt.Errorf("scanning package row: %w", err)
		}
		p.IsActive = isActive == 1
		if lastCommitted != nil {
			if t, err := time.Parse(time.RFC3339, *lastCommitted); err == nil {
				p.LastCommitted = &t
			}
		}
		if lastSyncedAt != nil {
			if t, err := time.Parse(time.RFC3339, *lastSyncedAt); err == nil {
				p.LastSyncedAt = &t
			}
		}
		pkgs = append(pkgs, &p)
	}
	return pkgs, rows.Err()
}

// GetDirtyPackages returns active packages whose content_hash differs from deployed_hash.
func GetDirtyPackages(ctx context.Context, db *sql.DB) ([]*Package, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, type, name, versions_json, content_hash,
			description, homepage, author, last_committed, trunk_revision
		FROM packages
		WHERE is_active = 1
			AND content_hash IS NOT NULL
			AND (deployed_hash IS NULL OR content_hash != deployed_hash)`)
	if err != nil {
		return nil, fmt.Errorf("querying dirty packages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var pkgs []*Package
	for rows.Next() {
		var p Package
		var lastCommitted *string
		if err := rows.Scan(&p.ID, &p.Type, &p.Name, &p.VersionsJSON, &p.ContentHash,
			&p.Description, &p.Homepage, &p.Author, &lastCommitted, &p.TrunkRevision); err != nil {
			return nil, fmt.Errorf("scanning dirty package: %w", err)
		}
		if lastCommitted != nil {
			if t, err := time.Parse(time.RFC3339, *lastCommitted); err == nil {
				p.LastCommitted = &t
			}
		}
		pkgs = append(pkgs, &p)
	}
	return pkgs, rows.Err()
}

// GetDeactivatedDeployedPackages returns inactive packages that still have a deployed_hash
// (i.e. their p2 files are still on R2 and need to be deleted).
func GetDeactivatedDeployedPackages(ctx context.Context, db *sql.DB) ([]*Package, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, type, name FROM packages
		WHERE is_active = 0 AND deployed_hash IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("querying deactivated deployed packages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var pkgs []*Package
	for rows.Next() {
		var p Package
		if err := rows.Scan(&p.ID, &p.Type, &p.Name); err != nil {
			return nil, fmt.Errorf("scanning deactivated package: %w", err)
		}
		pkgs = append(pkgs, &p)
	}
	return pkgs, rows.Err()
}

// DeactivatePackage sets is_active = 0 for a package.
func DeactivatePackage(ctx context.Context, db *sql.DB, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.ExecContext(ctx,
		`UPDATE packages SET is_active = 0, updated_at = ?, content_changed_at = ? WHERE id = ?`,
		now, now, id,
	)
	if err != nil {
		return fmt.Errorf("deactivating package %d: %w", id, err)
	}
	return nil
}

// MarkPermanentlyClosed tombstones a package so it's excluded from discovery
// and polling entirely. The row is kept (with is_active = 0 and
// permanently_closed = 1) so SVN discovery dedup still works and audit
// history via status_check_changes is preserved.
func MarkPermanentlyClosed(ctx context.Context, db *sql.DB, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.ExecContext(ctx,
		`UPDATE packages SET permanently_closed = 1, is_active = 0, updated_at = ?, content_changed_at = ? WHERE id = ?`,
		now, now, id,
	)
	if err != nil {
		return fmt.Errorf("marking package %d permanently closed: %w", id, err)
	}
	return nil
}

// ReactivatePackage sets is_active = 1 for a package.
func ReactivatePackage(ctx context.Context, db *sql.DB, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.ExecContext(ctx,
		`UPDATE packages SET is_active = 1, updated_at = ?, content_changed_at = ? WHERE id = ?`,
		now, now, id,
	)
	if err != nil {
		return fmt.Errorf("reactivating package %d: %w", id, err)
	}
	return nil
}

// GetAllPackages returns all packages, optionally filtered by type.
func GetAllPackages(ctx context.Context, db *sql.DB, pkgType string) ([]*Package, error) {
	query := `SELECT id, type, name, is_active FROM packages WHERE permanently_closed = 0`
	var args []any

	if pkgType != "" && pkgType != "all" {
		query += ` AND type = ?`
		args = append(args, pkgType)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying packages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var pkgs []*Package
	for rows.Next() {
		var p Package
		if err := rows.Scan(&p.ID, &p.Type, &p.Name, &p.IsActive); err != nil {
			return nil, fmt.Errorf("scanning package: %w", err)
		}
		pkgs = append(pkgs, &p)
	}
	return pkgs, rows.Err()
}

// StartStatusCheck inserts a new status_checks row and returns its ID.
func StartStatusCheck(ctx context.Context, db *sql.DB, started time.Time) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO status_checks (started_at, status) VALUES (?, 'running')`,
		started.Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("inserting status check: %w", err)
	}
	return res.LastInsertId()
}

// FinishStatusCheck updates a status_checks row with the final results.
func FinishStatusCheck(ctx context.Context, db *sql.DB, id int64, started time.Time,
	checked, deactivated, reactivated, failed int64, runErr error) error {
	now := time.Now().UTC()
	status := "completed"
	var errMsg *string
	if runErr != nil {
		status = "failed"
		s := runErr.Error()
		errMsg = &s
	} else if failed > 0 {
		status = "completed_with_errors"
	}
	_, err := db.ExecContext(ctx, `
		UPDATE status_checks SET
			finished_at = ?, status = ?, checked = ?, deactivated = ?,
			reactivated = ?, failed = ?, duration_seconds = ?, error_message = ?
		WHERE id = ?`,
		now.Format(time.RFC3339), status, checked, deactivated,
		reactivated, failed, int(now.Sub(started).Seconds()), errMsg, id)
	if err != nil {
		return fmt.Errorf("finishing status check %d: %w", id, err)
	}
	return nil
}

// RecordStatusCheckChange inserts a per-package deactivation or reactivation event.
func RecordStatusCheckChange(ctx context.Context, db *sql.DB, statusCheckID int64, pkgType, pkgName, action string) {
	_, _ = db.ExecContext(ctx,
		`INSERT INTO status_check_changes (status_check_id, package_type, package_name, action, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		statusCheckID, pkgType, pkgName, action, time.Now().UTC().Format(time.RFC3339))
}

// StatusCheckChange represents a per-package event from a status check run.
type StatusCheckChange struct {
	PackageType string
	PackageName string
	Action      string
}

// GetStatusCheckChanges returns the per-package changes for a given status check.
func GetStatusCheckChanges(ctx context.Context, db *sql.DB, statusCheckID int64) ([]StatusCheckChange, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT package_type, package_name, action
		 FROM status_check_changes WHERE status_check_id = ?
		 ORDER BY id`, statusCheckID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var changes []StatusCheckChange
	for rows.Next() {
		var c StatusCheckChange
		if err := rows.Scan(&c.PackageType, &c.PackageName, &c.Action); err != nil {
			return nil, err
		}
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

// StatusCheckDisplayLimit is the number of recent status check runs shown on
// the status page, and the retention window for per-package change rows.
const StatusCheckDisplayLimit = 50

// StatusCheck represents a row from the status_checks table.
type StatusCheck struct {
	ID              int64
	StartedAt       string
	FinishedAt      string
	Status          string
	Checked         int64
	Deactivated     int64
	Reactivated     int64
	Failed          int64
	DurationSeconds *int
	ErrorMessage    string
}

// GetStatusChecks returns the most recent status check runs.
func GetStatusChecks(ctx context.Context, db *sql.DB, limit int) ([]StatusCheck, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, started_at, COALESCE(finished_at, ''), status,
			checked, deactivated, reactivated, failed,
			duration_seconds, COALESCE(error_message, '')
		FROM status_checks ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("querying status checks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var checks []StatusCheck
	for rows.Next() {
		var c StatusCheck
		if err := rows.Scan(&c.ID, &c.StartedAt, &c.FinishedAt, &c.Status,
			&c.Checked, &c.Deactivated, &c.Reactivated, &c.Failed,
			&c.DurationSeconds, &c.ErrorMessage); err != nil {
			return nil, fmt.Errorf("scanning status check: %w", err)
		}
		checks = append(checks, c)
	}
	return checks, rows.Err()
}

// RefreshSiteStats recomputes the package_stats row from the packages table.
func RefreshSiteStats(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		INSERT OR REPLACE INTO package_stats (id, active_plugins, active_themes, plugin_installs, theme_installs, installs_30d, updated_at)
		SELECT 1,
			COALESCE(SUM(CASE WHEN type = 'plugin' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN type = 'theme' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN type = 'plugin' THEN wp_packages_installs_total ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN type = 'theme' THEN wp_packages_installs_total ELSE 0 END), 0),
			COALESCE(SUM(wp_packages_installs_30d), 0),
			datetime('now')
		FROM packages
		WHERE is_active = 1`)
	if err != nil {
		return fmt.Errorf("refreshing package stats: %w", err)
	}
	return nil
}

// MarkPackagesChanged sets last_committed = now and trunk_revision for the given
// slugs of a specific type, so they'll be picked up by GetPackagesNeedingUpdate.
// slugRevisions maps each slug to its highest SVN revision from the changelog.
func MarkPackagesChanged(ctx context.Context, db *sql.DB, pkgType string, slugRevisions map[string]int64) (int64, error) {
	if len(slugRevisions) == 0 {
		return 0, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		UPDATE packages
		SET last_committed = ?, updated_at = ?, trunk_revision = ?
		WHERE type = ? AND name = ? AND is_active = 1`)
	if err != nil {
		return 0, fmt.Errorf("preparing statement: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	var affected int64
	for slug, rev := range slugRevisions {
		res, err := stmt.ExecContext(ctx, now, now, rev, pkgType, slug)
		if err != nil {
			return affected, fmt.Errorf("marking package %s/%s changed: %w", pkgType, slug, err)
		}
		n, _ := res.RowsAffected()
		affected += n
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing: %w", err)
	}
	return affected, nil
}

// BackfillTrunkRevisions sets trunk_revision for active plugins that don't have one yet.
// Only updates rows where trunk_revision IS NULL to avoid overwriting newer data.
func BackfillTrunkRevisions(ctx context.Context, db *sql.DB, slugRevisions map[string]int64) (int64, error) {
	if len(slugRevisions) == 0 {
		return 0, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		UPDATE packages
		SET trunk_revision = ?
		WHERE type = 'plugin' AND name = ? AND is_active = 1 AND trunk_revision IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("preparing statement: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	var affected int64
	for slug, rev := range slugRevisions {
		res, err := stmt.ExecContext(ctx, rev, slug)
		if err != nil {
			return affected, fmt.Errorf("backfilling trunk_revision for %s: %w", slug, err)
		}
		n, _ := res.RowsAffected()
		affected += n
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing: %w", err)
	}
	return affected, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
