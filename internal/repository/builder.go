package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/roots/wp-packages/internal/composer"
	"github.com/roots/wp-packages/internal/version"
)

// BuildOpts configures a repository build.
type BuildOpts struct {
	OutputDir        string // base output dir (e.g. storage/repository/builds)
	AppURL           string // absolute app URL for notify-batch
	Force            bool
	PackageName      string   // optional: build single package
	PackageNames     []string // optional: build only these slugs
	BuildID          string   // optional: pre-generated build ID (used by pipeline)
	PreviousBuildDir string   // optional: compare against previous build to count changes
	Logger           *slog.Logger
}

// PackageChange records a single package-level change for the metadata changes feed.
type PackageChange struct {
	Name   string // e.g. "wp-plugin/akismet"
	Action string // "update" or "delete"
}

// BuildResult holds build metadata for manifest.json and the builds table.
type BuildResult struct {
	BuildID         string
	StartedAt       time.Time
	FinishedAt      time.Time
	DurationSeconds int
	PackagesTotal   int
	PackagesChanged int
	PackagesSkipped int
	ArtifactCount   int
	RootHash        string
	SyncRunID       *int64
	BuildDir        string
	ChangedPackages []PackageChange
}

// Build generates all Composer repository artifacts (p2/ only).
func Build(ctx context.Context, db *sql.DB, opts BuildOpts) (*BuildResult, error) {
	started := time.Now().UTC()
	buildID := opts.BuildID
	if buildID == "" {
		buildID = started.Format("20060102-150405")
	}
	buildDir := filepath.Join(opts.OutputDir, buildID)

	// Guard against build ID collision
	if _, err := os.Stat(buildDir); err == nil {
		return nil, fmt.Errorf("build directory already exists: %s (another build started in the same second?)", buildID)
	}

	// Pre-create directories upfront
	for _, dir := range []string{
		filepath.Join(buildDir, "p2", "wp-plugin"),
		filepath.Join(buildDir, "p2", "wp-theme"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating dir %s: %w", dir, err)
		}
	}

	opts.Logger.Info("starting build", "build_id", buildID)

	// Snapshot sync run ID for consistency (skip with --force)
	var snapshotID *int64
	if !opts.Force {
		var sid int64
		err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(last_sync_run_id), 0) FROM packages`).Scan(&sid)
		if err != nil {
			return nil, fmt.Errorf("getting snapshot id: %w", err)
		}
		if sid > 0 {
			snapshotID = &sid
		}
	}

	// Query active packages
	query := `SELECT id, type, name, display_name, description, author, homepage,
		versions_json, current_version, last_committed, trunk_revision,
		published_times_json
		FROM packages WHERE is_active = 1`
	args := []any{}

	if snapshotID != nil {
		query += ` AND (last_sync_run_id IS NULL OR last_sync_run_id <= ?)`
		args = append(args, *snapshotID)
	}
	if opts.PackageName != "" {
		query += ` AND (type || '/' || name) = ?`
		args = append(args, opts.PackageName)
	}
	if len(opts.PackageNames) > 0 {
		placeholders := make([]string, len(opts.PackageNames))
		for i, n := range opts.PackageNames {
			placeholders[i] = "?"
			args = append(args, n)
		}
		query += ` AND name IN (` + strings.Join(placeholders, ",") + `)`
	}
	query += ` ORDER BY type, name`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying packages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var totalPkgs, changedPkgs, artifactCount int
	var changedPackages []PackageChange

	for rows.Next() {
		var (
			id                                         int64
			pkgType, name                              string
			displayName, description, author, homepage *string
			versionsJSON                               string
			currentVer                                 *string
			lastCommitted                              *string
			trunkRevision                              *int64
			publishedTimesJSON                         *string
		)
		if err := rows.Scan(&id, &pkgType, &name, &displayName, &description, &author,
			&homepage, &versionsJSON, &currentVer, &lastCommitted, &trunkRevision,
			&publishedTimesJSON); err != nil {
			return nil, fmt.Errorf("scanning package: %w", err)
		}

		// Parse versions
		var versions map[string]string
		if err := json.Unmarshal([]byte(versionsJSON), &versions); err != nil {
			opts.Logger.Warn("skipping package with invalid versions_json", "name", name, "error", err)
			continue
		}
		// Defense-in-depth: re-filter versions through normalization so stale
		// DB rows with invalid versions (e.g. "3.1.0-dev1") never reach artifacts.
		// Plugins with zero tagged versions still get a ~dev.json; themes are skipped.
		versions = version.NormalizeVersions(versions)

		composerName := ComposerName(pkgType, name)
		meta := PackageMeta{}
		if description != nil {
			meta.Description = *description
		}
		if homepage != nil {
			meta.Homepage = *homepage
		}
		if author != nil {
			meta.Author = *author
		}
		if lastCommitted != nil {
			meta.LastUpdated = *lastCommitted
		}
		meta.TrunkRevision = trunkRevision
		meta.PublishedTimes = composer.ParsePublishedTimes(publishedTimesJSON)

		// Split versions into tagged (for .json) and build all entries
		taggedVersions := make(map[string]any)
		for ver, dlURL := range versions {
			if !strings.HasPrefix(ver, "dev-") {
				taggedVersions[ver] = ComposerVersion(pkgType, name, ver, dlURL, meta)
			}
		}

		// Plugins get a ~dev.json with dev-trunk pointing to SVN trunk.
		// Themes don't have trunk in SVN so they only get tagged versions.
		var devVersions map[string]any
		if pkgType == "plugin" {
			devVersions = map[string]any{
				"dev-trunk": ComposerVersion(pkgType, name, "dev-trunk", "", meta),
			}
		}

		// Track whether this package has been marked as changed (avoid double-counting)
		pkgChanged := false

		// Write p2/<name>.json — tagged versions, or dev-trunk for trunk-only plugins
		mainVersions := taggedVersions
		if len(mainVersions) == 0 && devVersions != nil {
			mainVersions = devVersions
		}
		if len(mainVersions) == 0 {
			// Theme with no tagged versions — skip
			continue
		}
		totalPkgs++
		pkgPayload := map[string]any{
			"packages": map[string]any{
				composerName: mainVersions,
			},
		}
		_, data, err := HashJSON(pkgPayload)
		if err != nil {
			return nil, fmt.Errorf("hashing %s: %w", composerName, err)
		}

		p2Rel := filepath.Join("p2", composerName+".json")
		p2File := filepath.Join(buildDir, p2Rel)
		if err := os.WriteFile(p2File, data, 0644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", p2File, err)
		}
		artifactCount++

		if opts.PreviousBuildDir != "" {
			prevData, err := os.ReadFile(filepath.Join(opts.PreviousBuildDir, p2Rel))
			if err != nil || !bytes.Equal(prevData, data) {
				pkgChanged = true
			}
		} else {
			pkgChanged = true
		}

		// Write ~dev.json for plugins only
		if devVersions != nil {
			devPayload := map[string]any{
				"packages": map[string]any{
					composerName: devVersions,
				},
			}
			_, devData, err := HashJSON(devPayload)
			if err != nil {
				return nil, fmt.Errorf("hashing %s~dev: %w", composerName, err)
			}

			devRel := filepath.Join("p2", composerName+"~dev.json")
			devFile := filepath.Join(buildDir, devRel)
			if err := os.WriteFile(devFile, devData, 0644); err != nil {
				return nil, fmt.Errorf("writing %s: %w", devFile, err)
			}
			artifactCount++

			if !pkgChanged {
				if opts.PreviousBuildDir != "" {
					prevData, err := os.ReadFile(filepath.Join(opts.PreviousBuildDir, devRel))
					if err != nil || !bytes.Equal(prevData, devData) {
						pkgChanged = true
					}
				} else {
					pkgChanged = true
				}
			}
		}

		if pkgChanged {
			changedPkgs++
			changedPackages = append(changedPackages, PackageChange{Name: composerName, Action: "update"})
			opts.Logger.Info("package changed", "package", composerName)
		}

		if totalPkgs%500 == 0 {
			opts.Logger.Info("build progress", "packages", totalPkgs)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating packages: %w", err)
	}

	// Detect deleted packages (only for full builds, not partial).
	// Collect unique package names from the previous build, then check
	// if any are completely absent from the new build (no .json or ~dev.json).
	isPartialBuild := opts.PackageName != "" || len(opts.PackageNames) > 0
	if opts.PreviousBuildDir != "" && !isPartialBuild {
		prevPackages := make(map[string]struct{})
		prevP2 := filepath.Join(opts.PreviousBuildDir, "p2")
		if err := filepath.Walk(prevP2, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".json") {
				return nil
			}
			rel, err := filepath.Rel(prevP2, path)
			if err != nil {
				return fmt.Errorf("rel path for %s: %w", path, err)
			}
			relSlash := filepath.ToSlash(rel)
			name := strings.TrimSuffix(relSlash, "~dev.json")
			if name == relSlash {
				name = strings.TrimSuffix(relSlash, ".json")
			}
			prevPackages[name] = struct{}{}
			return nil
		}); err != nil {
			opts.Logger.Warn("delete detection walk failed", "error", err)
		}

		for name := range prevPackages {
			mainPath := filepath.Join(buildDir, "p2", name+".json")
			devPath := filepath.Join(buildDir, "p2", name+"~dev.json")
			_, mainErr := os.Stat(mainPath)
			_, devErr := os.Stat(devPath)
			if os.IsNotExist(mainErr) && os.IsNotExist(devErr) {
				changedPackages = append(changedPackages, PackageChange{Name: name, Action: "delete"})
				opts.Logger.Info("package deleted", "package", name)
			}
		}
	}

	// Build packages.json
	rootData, err := composer.PackagesJSON(opts.AppURL)
	if err != nil {
		return nil, fmt.Errorf("building packages.json: %w", err)
	}
	rootHash := fmt.Sprintf("%x", sha256.Sum256(rootData))
	if err := os.WriteFile(filepath.Join(buildDir, "packages.json"), rootData, 0644); err != nil {
		return nil, fmt.Errorf("writing packages.json: %w", err)
	}
	artifactCount++

	// Write manifest.json
	finished := time.Now().UTC()
	manifest := map[string]any{
		"build_id":         buildID,
		"started_at":       started.Format(time.RFC3339),
		"finished_at":      finished.Format(time.RFC3339),
		"duration_seconds": int(finished.Sub(started).Seconds()),
		"packages_total":   totalPkgs,
		"packages_changed": changedPkgs,
		"artifact_count":   artifactCount,
		"root_hash":        rootHash,
	}
	if snapshotID != nil {
		manifest["db_snapshot_id"] = *snapshotID
	}

	manifestData, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(buildDir, "manifest.json"), manifestData, 0644); err != nil {
		return nil, fmt.Errorf("writing manifest.json: %w", err)
	}
	artifactCount++

	// Integrity validation — verify p2/ files exist on disk
	integrityErrors := validateIntegrityInMemory(buildDir, totalPkgs)
	if len(integrityErrors) > 0 {
		for _, e := range integrityErrors {
			opts.Logger.Error("integrity error", "error", e)
		}
		return nil, fmt.Errorf("integrity validation failed with %d errors", len(integrityErrors))
	}

	result := &BuildResult{
		BuildID:         buildID,
		StartedAt:       started,
		FinishedAt:      finished,
		DurationSeconds: int(finished.Sub(started).Seconds()),
		PackagesTotal:   totalPkgs,
		PackagesChanged: changedPkgs,
		ArtifactCount:   artifactCount,
		RootHash:        rootHash,
		SyncRunID:       snapshotID,
		BuildDir:        buildDir,
		ChangedPackages: changedPackages,
	}

	opts.Logger.Info("build complete",
		"build_id", buildID,
		"packages", totalPkgs,
		"changed", changedPkgs,
		"artifacts", artifactCount,
		"duration", finished.Sub(started).String(),
	)

	return result, nil
}

// validateIntegrityInMemory checks that p2/ files and packages.json exist on disk.
func validateIntegrityInMemory(buildDir string, expectedPackages int) []string {
	var errs []string

	// Verify packages.json exists and is parseable
	data, err := os.ReadFile(filepath.Join(buildDir, "packages.json"))
	if err != nil {
		return []string{fmt.Sprintf("packages.json missing: %v", err)}
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return []string{fmt.Sprintf("packages.json invalid: %v", err)}
	}

	// Count p2/ files
	var p2Count int
	p2Dir := filepath.Join(buildDir, "p2")
	_ = filepath.Walk(p2Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		p2Count++
		return nil
	})

	// Each package produces at least one p2 file, and possibly a ~dev.json too
	if p2Count < expectedPackages {
		errs = append(errs, fmt.Sprintf("expected at least %d p2/ files, found %d", expectedPackages, p2Count))
	}

	return errs
}

// ValidateIntegrity checks that packages.json exists, is valid, and p2/ files
// match the count declared in manifest.json.
func ValidateIntegrity(buildDir string) []string {
	packagesPath := filepath.Join(buildDir, "packages.json")
	data, err := os.ReadFile(packagesPath)
	if err != nil {
		return []string{fmt.Sprintf("packages.json missing: %v", err)}
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return []string{fmt.Sprintf("packages.json invalid: %v", err)}
	}

	// Count p2/ files on disk.
	var p2Count int
	p2Dir := filepath.Join(buildDir, "p2")
	_ = filepath.Walk(p2Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		p2Count++
		return nil
	})

	// Cross-check against manifest if available.
	manifestData, err := os.ReadFile(filepath.Join(buildDir, "manifest.json"))
	if err == nil {
		var manifest map[string]any
		if json.Unmarshal(manifestData, &manifest) == nil {
			if expected, ok := manifest["packages_total"].(float64); ok && p2Count < int(expected) {
				return []string{fmt.Sprintf("manifest says %d packages but found only %d p2/ files", int(expected), p2Count)}
			}
		}
	}

	return nil
}
