package http

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/roots/wp-packages/internal/app"
	"github.com/roots/wp-packages/internal/config"
	"github.com/roots/wp-packages/internal/deploy"
	"github.com/roots/wp-packages/internal/og"
	"github.com/roots/wp-packages/internal/packages"
	"github.com/roots/wp-packages/internal/telemetry"
	"github.com/roots/wp-packages/internal/version"
)

const perPage = 12

// captureError reports a non-panic error to Sentry with the request's hub.
// It silently ignores context cancellation errors (timeouts, client disconnects)
// since these are expected during normal operation.
func captureError(r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	if hub := sentry.GetHubFromContext(r.Context()); hub != nil {
		hub.CaptureException(err)
	} else {
		sentry.CaptureException(err)
	}
}

// setETag computes an ETag from the given seed strings, sets it on the
// response, and returns true if the client already has this version (304).
func setETag(w http.ResponseWriter, r *http.Request, parts ...string) bool {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
	}
	etag := `"` + hex.EncodeToString(h.Sum(nil))[:16] + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

type packageRow struct {
	ID                      int64
	Type                    string
	Name                    string
	DisplayName             string
	Description             string
	Author                  string
	Homepage                string
	CurrentVersion          string
	WporgVersion            string
	Downloads               int64
	ActiveInstalls          int64
	IsActive                bool
	LastSyncedAt            string
	LastCommitted           string
	WpPackagesInstallsTotal int64
}

type versionRow struct {
	Version  string
	IsLatest bool
}

func handleIndex(a *app.App, tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filters := publicFilters{
			Search: r.URL.Query().Get("search"),
			Type:   r.URL.Query().Get("type"),
			Sort:   r.URL.Query().Get("sort"),
		}
		if filters.Sort == "" {
			filters.Sort = "composer_installs"
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}

		packages, total, err := queryPackages(r.Context(), a.DB, filters, page, perPage)
		if err != nil {
			a.Logger.Error("querying packages", "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		totalPages := (total + perPage - 1) / perPage

		stats := queryIndexStats(r.Context(), a.DB)
		stats.RootsDownloads = a.Packagist.Total()

		searchURL := a.Config.AppURL + "/?search={search_term_string}"
		jsonLDData := map[string]any{
			"@context": "https://schema.org",
			"@type":    "WebSite",
			"name":     "WP Packages",
			"url":      a.Config.AppURL + "/",
			"potentialAction": map[string]any{
				"@type":       "SearchAction",
				"target":      searchURL,
				"query-input": "required name=search_term_string",
			},
		}

		w.Header().Set("Cache-Control", "public, max-age=60, s-maxage=300, stale-while-revalidate=3600")
		if setETag(w, r, "index", stats.UpdatedAt, filters.Search, filters.Type, filters.Sort, strconv.Itoa(page)) {
			return
		}

		render(w, r, tmpl.index, "layout", map[string]any{
			"Packages":   packages,
			"Filters":    filters,
			"Page":       page,
			"Total":      total,
			"TotalPages": totalPages,
			"Stats":      stats,
			"AppURL":     a.Config.AppURL,
			"CDNURL":     a.Config.R2.CDNPublicURL,
			"OGImage":    ogImageURL(a.Config, "social/default.png"),
			"JSONLD":     jsonLDData,
			"BlogPosts":  a.Blog.Posts(),
		})
	}
}

func handleIndexPartial(a *app.App, tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filters := publicFilters{
			Search: r.URL.Query().Get("search"),
			Type:   r.URL.Query().Get("type"),
			Sort:   r.URL.Query().Get("sort"),
		}
		if filters.Sort == "" {
			filters.Sort = "composer_installs"
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}

		packages, total, err := queryPackages(r.Context(), a.DB, filters, page, perPage)
		if err != nil {
			a.Logger.Error("querying packages", "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		totalPages := (total + perPage - 1) / perPage

		w.Header().Set("X-Robots-Tag", "noindex")
		render(w, r, tmpl.indexPartial, "package-results", map[string]any{
			"Packages":   packages,
			"Filters":    filters,
			"Page":       page,
			"Total":      total,
			"TotalPages": totalPages,
		})
	}
}

func handleDocs(a *app.App, tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600, stale-while-revalidate=86400")
		render(w, r, tmpl.docs, "layout", map[string]any{
			"AppURL":  a.Config.AppURL,
			"CDNURL":  a.Config.R2.CDNPublicURL,
			"OGImage": ogImageURL(a.Config, "social/default.png"),
		})
	}
}

func handleCompare(a *app.App, tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600, stale-while-revalidate=86400")
		render(w, r, tmpl.compare, "layout", map[string]any{
			"AppURL":  a.Config.AppURL,
			"CDNURL":  a.Config.R2.CDNPublicURL,
			"OGImage": ogImageURL(a.Config, "social/default.png"),
		})
	}
}

const untaggedPerPage = 20

func handleUntagged(a *app.App, tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		filter := r.URL.Query().Get("filter")
		search := strings.TrimSpace(r.URL.Query().Get("search"))
		author := strings.TrimSpace(r.URL.Query().Get("author"))
		sort := r.URL.Query().Get("sort")

		packages, total, err := queryUntaggedPackages(r.Context(), a.DB, filter, search, author, sort, page, untaggedPerPage)
		if err != nil {
			a.Logger.Error("querying untagged packages", "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		totalPages := (total + untaggedPerPage - 1) / untaggedPerPage

		var totalPlugins int64
		_ = a.DB.QueryRowContext(r.Context(), "SELECT active_plugins FROM package_stats WHERE id = 1").Scan(&totalPlugins)

		w.Header().Set("Cache-Control", "public, max-age=3600, stale-while-revalidate=86400")
		render(w, r, tmpl.untagged, "layout", map[string]any{
			"Packages":     packages,
			"Filter":       filter,
			"Search":       search,
			"Author":       author,
			"Sort":         sort,
			"Page":         page,
			"Total":        int64(total),
			"TotalPlugins": totalPlugins,
			"TotalPages":   totalPages,
			"AppURL":       a.Config.AppURL,
			"CDNURL":       a.Config.R2.CDNPublicURL,
			"OGImage":      ogImageURL(a.Config, "social/default.png"),
		})
	}
}

func handleUntaggedPartial(a *app.App, tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		filter := r.URL.Query().Get("filter")
		search := strings.TrimSpace(r.URL.Query().Get("search"))
		author := strings.TrimSpace(r.URL.Query().Get("author"))
		sort := r.URL.Query().Get("sort")

		packages, total, err := queryUntaggedPackages(r.Context(), a.DB, filter, search, author, sort, page, untaggedPerPage)
		if err != nil {
			a.Logger.Error("querying untagged packages", "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		totalPages := (total + untaggedPerPage - 1) / untaggedPerPage

		w.Header().Set("X-Robots-Tag", "noindex")
		render(w, r, tmpl.untaggedPartial, "untagged-results", map[string]any{
			"Packages":   packages,
			"Filter":     filter,
			"Search":     search,
			"Author":     author,
			"Sort":       sort,
			"Page":       page,
			"Total":      int64(total),
			"TotalPages": totalPages,
		})
	}
}

func handleUntaggedAuthors(a *app.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if len(q) < 2 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}

		rows, err := a.DB.QueryContext(r.Context(),
			`SELECT DISTINCT author FROM packages
			WHERE is_active = 1 AND type = 'plugin'
			AND wporg_version IS NOT NULL AND wporg_version != ''
			AND NOT EXISTS (SELECT 1 FROM json_each(versions_json) WHERE key = wporg_version)
			AND author != '' AND author LIKE ?
			ORDER BY author COLLATE NOCASE
			LIMIT 10`,
			q+"%",
		)
		if err != nil {
			a.Logger.Error("querying untagged authors", "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		defer func() { _ = rows.Close() }()

		var authors []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				continue
			}
			authors = append(authors, name)
		}
		if authors == nil {
			authors = []string{}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_ = json.NewEncoder(w).Encode(authors)
	}
}

func handleWordpressCore(a *app.App, tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600, stale-while-revalidate=86400")
		render(w, r, tmpl.wordpressCore, "layout", map[string]any{
			"AppURL":         a.Config.AppURL,
			"CDNURL":         a.Config.R2.CDNPublicURL,
			"OGImage":        ogImageURL(a.Config, "social/default.png"),
			"RootsDownloads": a.Packagist.Total(),
		})
	}
}

func handleDetail(a *app.App, tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pkgType := r.PathValue("type")
		name := r.PathValue("name")

		// Strip wp- prefix from type
		pkgType = strings.TrimPrefix(pkgType, "wp-")

		pkg, err := queryPackageDetail(r.Context(), a.DB, pkgType, name)
		if err != nil {
			gone := packageExistsInactive(r.Context(), a.DB, pkgType, name)
			if gone {
				http.Redirect(w, r, "https://wp-packages.org/", http.StatusFound)
			} else {
				w.WriteHeader(http.StatusNotFound)
				render(w, r, tmpl.notFound, "layout", map[string]any{"Gone": false, "CDNURL": a.Config.R2.CDNPublicURL})
			}
			return
		}

		versions := parseVersions(pkg)

		monthlyInstalls, _ := telemetry.GetMonthlyInstalls(r.Context(), a.DB, pkg.ID)

		untagged := pkg.Type == "plugin" && pkg.WporgVersion != "" && !versionIsTagged(versions, pkg.WporgVersion)
		trunkOnly := untagged && !hasTaggedVersion(versions)

		var ogImage string
		if pkg.OGImageGeneratedAt != nil {
			ogImage = ogImageURL(a.Config, "social/"+pkg.Type+"/"+pkg.Name+".png")
		}

		displayName := pkg.Name
		if pkg.DisplayName != "" {
			displayName = pkg.DisplayName
		}
		pkgURL := a.Config.AppURL + "/packages/wp-" + pkg.Type + "/" + pkg.Name

		softwareApp := map[string]any{
			"@context":            "https://schema.org",
			"@type":               "SoftwareApplication",
			"name":                displayName,
			"applicationCategory": "WordPress " + pkg.Type,
			"operatingSystem":     "WordPress",
			"url":                 pkgURL,
		}
		if pkg.CurrentVersion != "" {
			softwareApp["softwareVersion"] = pkg.CurrentVersion
		}
		if pkg.Author != "" {
			softwareApp["author"] = map[string]any{
				"@type": "Person",
				"name":  pkg.Author,
			}
		}
		if pkg.ActiveInstalls > 0 {
			softwareApp["interactionStatistic"] = map[string]any{
				"@type":                "InteractionCounter",
				"interactionType":      "https://schema.org/InstallAction",
				"userInteractionCount": pkg.ActiveInstalls,
			}
		}
		if pkg.Description != "" {
			softwareApp["description"] = pkg.Description
		}
		if pkg.UpdatedAt != "" {
			softwareApp["dateModified"] = pkg.UpdatedAt
		}

		breadcrumbs := map[string]any{
			"@context": "https://schema.org",
			"@type":    "BreadcrumbList",
			"itemListElement": []map[string]any{
				{
					"@type":    "ListItem",
					"position": 1,
					"name":     "Packages",
					"item":     a.Config.AppURL + "/",
				},
				{
					"@type":    "ListItem",
					"position": 2,
					"name":     displayName,
					"item":     pkgURL,
				},
			},
		}

		w.Header().Set("Cache-Control", "public, max-age=60, s-maxage=3600, stale-while-revalidate=86400")
		if setETag(w, r, "detail", pkg.Type, pkg.Name, pkg.UpdatedAt) {
			return
		}

		render(w, r, tmpl.detail, "layout", map[string]any{
			"Package":         pkg,
			"Versions":        versions,
			"MonthlyInstalls": monthlyInstalls,
			"Untagged":        untagged,
			"TrunkOnly":       trunkOnly,
			"AppURL":          a.Config.AppURL,
			"CDNURL":          a.Config.R2.CDNPublicURL,
			"OGImage":         ogImage,
			"JSONLD":          []any{softwareApp, breadcrumbs},
		})
	}
}

var logFiles = map[string]string{
	"wppackages":   filepath.Join("storage", "logs", "wppackages.log"),
	"pipeline":     filepath.Join("storage", "logs", "pipeline.log"),
	"check-status": filepath.Join("storage", "logs", "check-status.log"),
}

func handleAdminLogs(tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		render(w, r, tmpl.adminLogs, "admin_layout", nil)
	}
}

func handleAdminLogStream(a *app.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		file := r.URL.Query().Get("file")
		logPath, ok := logFiles[file]
		if !ok {
			http.Error(w, "unknown log file", http.StatusBadRequest)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		ctx := r.Context()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		// Wait for the file to exist
		var f *os.File
		for f == nil {
			if opened, err := os.Open(logPath); err == nil {
				f = opened
			} else {
				_, _ = fmt.Fprintf(w, "data: Waiting for %s ...\n\n", filepath.Base(logPath))
				flusher.Flush()
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
		}
		defer func() { _ = f.Close() }()

		// Send initial batch: last 200 lines
		lines := tailFile(logPath, 200)
		for _, line := range lines {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", line)
		}
		flusher.Flush()

		// Seek to end for tailing
		offset, _ := f.Seek(0, 2)

		buf := make([]byte, 64*1024)
		var partial string
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				info, err := os.Stat(logPath)
				if err != nil {
					continue
				}
				if info.Size() < offset {
					// File was truncated/rotated, reopen
					_ = f.Close()
					f, err = os.Open(logPath)
					if err != nil {
						continue
					}
					offset = 0
				}
				if info.Size() == offset {
					continue
				}
				_, _ = f.Seek(offset, 0)
				n, err := f.Read(buf)
				if n > 0 {
					offset += int64(n)
					chunk := partial + string(buf[:n])
					partial = ""
					newLines := strings.Split(chunk, "\n")
					if !strings.HasSuffix(chunk, "\n") {
						partial = newLines[len(newLines)-1]
						newLines = newLines[:len(newLines)-1]
					}
					for _, line := range newLines {
						if line != "" {
							if _, werr := fmt.Fprintf(w, "data: %s\n\n", line); werr != nil {
								return
							}
						}
					}
					flusher.Flush()
				}
				if err != nil {
					continue
				}
			}
		}
	}
}

func tailFile(path string, n int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// ogImageURL returns the full OG image URL for a given key.
// In production, it uses the CDN. In local dev, it uses /og/ routes.
func ogImageURL(cfg *config.Config, key string) string {
	if cfg.R2.CDNPublicURL != "" {
		return cfg.R2.CDNPublicURL + "/" + key
	}
	if cfg.AppURL != "" {
		return cfg.AppURL + "/og/" + key
	}
	return "/og/" + key
}

// handleOGImage serves OG images from local disk (dev mode).
func handleOGImage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/og/")
		clean := filepath.Clean(key)
		if strings.Contains(clean, "..") {
			http.NotFound(w, r)
			return
		}
		path := filepath.Join("storage", "og", clean)
		data, err := os.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(data)
	}
}

// ensureLocalFallbackOG generates the fallback OG image to disk and uploads to R2 if configured.
func ensureLocalFallbackOG(cfg *config.Config) {
	path := "storage/og/social/default.png"

	data, err := og.GenerateFallbackImage()
	if err != nil {
		return
	}

	// Always write locally
	_ = os.MkdirAll("storage/og/social", 0o755)
	_ = os.WriteFile(path, data, 0o644)

	// Upload to R2 CDN if configured
	uploader := og.NewUploader(cfg.R2)
	if uploader.IsR2() {
		_ = uploader.Upload(context.Background(), "social/default.png", data)
	}
}

// Query helpers

type indexStats struct {
	PluginInstalls int64
	ThemeInstalls  int64
	RootsDownloads int64
	UpdatedAt      string
}

func queryIndexStats(ctx context.Context, db *sql.DB) indexStats {
	var s indexStats
	_ = db.QueryRowContext(ctx, "SELECT plugin_installs, theme_installs, COALESCE(updated_at,'') FROM package_stats WHERE id = 1").Scan(&s.PluginInstalls, &s.ThemeInstalls, &s.UpdatedAt)
	return s
}

// collapseSlug strips hyphens, underscores, and spaces to produce a
// compact form suitable for LIKE-matching against similarly collapsed names.
func collapseSlug(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}

// ftsQuery converts a user search string into an FTS5 query.
// Each token becomes a prefix search, joined with AND.
// e.g. "woo commerce" -> "woo* AND commerce*"
func ftsQuery(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		// Escape double quotes to prevent FTS5 syntax injection
		w = strings.ReplaceAll(w, `"`, `""`)
		words[i] = `"` + w + `"` + "*"
	}
	return strings.Join(words, " AND ")
}

func queryPackages(ctx context.Context, db *sql.DB, f publicFilters, page, limit int) ([]packageRow, int, error) {
	where := "is_active = 1"
	args := []any{}

	if q := ftsQuery(f.Search); q != "" {
		where += " AND (id IN (SELECT rowid FROM packages_fts WHERE packages_fts MATCH ?) OR REPLACE(name, '-', '') LIKE ?)"
		args = append(args, q, "%"+collapseSlug(f.Search)+"%")
	}
	if f.Type != "" {
		where += " AND type = ?"
		args = append(args, f.Type)
	}

	orderBy := "wp_packages_installs_total DESC"
	switch f.Sort {
	case "active_installs":
		orderBy = "active_installs DESC"
	case "updated":
		orderBy = "last_committed DESC NULLS LAST"
	case "name":
		orderBy = "name ASC"
	}

	var total int
	if f.Search == "" && f.Type == "" {
		_ = db.QueryRowContext(ctx, "SELECT active_plugins + active_themes FROM package_stats WHERE id = 1").Scan(&total)
	} else {
		countQ := "SELECT COUNT(*) FROM packages WHERE " + where
		if err := db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}

	offset := (page - 1) * limit
	q := fmt.Sprintf(`SELECT type, name, COALESCE(display_name,''), COALESCE(description,''),
		COALESCE(current_version,''), downloads, active_installs, wp_packages_installs_total
		FROM packages WHERE %s ORDER BY %s LIMIT ? OFFSET ?`, where, orderBy)

	rows, err := db.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var pkgs []packageRow
	for rows.Next() {
		var p packageRow
		if err := rows.Scan(&p.Type, &p.Name, &p.DisplayName, &p.Description, &p.CurrentVersion, &p.Downloads, &p.ActiveInstalls, &p.WpPackagesInstallsTotal); err != nil {
			return nil, 0, fmt.Errorf("scanning package row: %w", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, total, rows.Err()
}

type packageDetail struct {
	packageRow
	VersionsJSON       string
	WporgVersion       string
	UpdatedAt          string
	OGImageGeneratedAt *string
}

func packageExistsInactive(ctx context.Context, db *sql.DB, pkgType, name string) bool {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM packages WHERE type = ? AND name = ? AND is_active = 0`, pkgType, name).Scan(&n)
	return err == nil
}

func queryPackageDetail(ctx context.Context, db *sql.DB, pkgType, name string) (*packageDetail, error) {
	var p packageDetail
	err := db.QueryRowContext(ctx, `SELECT id, type, name, COALESCE(display_name,''), COALESCE(description,''),
		COALESCE(author,''), COALESCE(homepage,''), COALESCE(current_version,''),
		downloads, active_installs, wp_packages_installs_total, versions_json,
		COALESCE(wporg_version,''), COALESCE(updated_at,''), og_image_generated_at
		FROM packages WHERE type = ? AND name = ? AND is_active = 1`, pkgType, name,
	).Scan(&p.ID, &p.Type, &p.Name, &p.DisplayName, &p.Description, &p.Author, &p.Homepage,
		&p.CurrentVersion, &p.Downloads, &p.ActiveInstalls, &p.WpPackagesInstallsTotal,
		&p.VersionsJSON, &p.WporgVersion, &p.UpdatedAt, &p.OGImageGeneratedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func parseVersions(pkg *packageDetail) []versionRow {
	var versions map[string]string
	_ = json.Unmarshal([]byte(pkg.VersionsJSON), &versions)

	var rows []versionRow
	for v := range versions {
		rows = append(rows, versionRow{
			Version:  v,
			IsLatest: v == pkg.CurrentVersion,
		})
	}

	// Sort by semver descending, with latest always first
	slices.SortFunc(rows, func(a, b versionRow) int {
		if a.IsLatest != b.IsLatest {
			if a.IsLatest {
				return -1
			}
			return 1
		}
		return version.Compare(b.Version, a.Version)
	})
	return rows
}

func versionIsTagged(versions []versionRow, cv string) bool {
	for _, v := range versions {
		if v.Version == cv {
			return true
		}
	}
	return false
}

func hasTaggedVersion(versions []versionRow) bool {
	for _, v := range versions {
		if v.Version != "dev-trunk" {
			return true
		}
	}
	return false
}

func queryDashboardStats(ctx context.Context, db *sql.DB) map[string]any {
	stats := map[string]any{
		"Stats": struct {
			TotalPackages  int64
			ActivePlugins  int64
			ActiveThemes   int64
			TotalInstalls  int64
			PluginInstalls int64
			ThemeInstalls  int64
			Installs30d    int64
			CurrentBuild   string
			StatsUpdatedAt string
		}{},
	}

	var s struct {
		TotalPackages  int64
		ActivePlugins  int64
		ActiveThemes   int64
		TotalInstalls  int64
		PluginInstalls int64
		ThemeInstalls  int64
		Installs30d    int64
		CurrentBuild   string
		StatsUpdatedAt string
	}

	_ = db.QueryRowContext(ctx, `SELECT active_plugins, active_themes, active_plugins + active_themes,
		plugin_installs + theme_installs, plugin_installs, theme_installs, installs_30d, COALESCE(updated_at,'') FROM package_stats WHERE id = 1`).Scan(
		&s.ActivePlugins, &s.ActiveThemes, &s.TotalPackages, &s.TotalInstalls, &s.PluginInstalls, &s.ThemeInstalls, &s.Installs30d, &s.StatsUpdatedAt)

	stats["Stats"] = s
	return stats
}

func queryUntaggedPackages(ctx context.Context, db *sql.DB, filter, search, author, sort string, page, limit int) ([]packageRow, int, error) {
	where := `is_active = 1 AND type = 'plugin' AND wporg_version IS NOT NULL AND wporg_version != '' AND NOT EXISTS (SELECT 1 FROM json_each(versions_json) WHERE key = wporg_version)`

	var args []any

	switch filter {
	case "trunk-only":
		where += ` AND (SELECT COUNT(*) FROM json_each(versions_json) WHERE key != 'dev-trunk') = 0`
	case "latest-not-tagged":
		where += ` AND (SELECT COUNT(*) FROM json_each(versions_json) WHERE key != 'dev-trunk') > 0`
	}

	if search != "" {
		where += ` AND (name LIKE ? OR display_name LIKE ?)`
		pat := "%" + search + "%"
		args = append(args, pat, pat)
	}

	if author != "" {
		where += ` AND author = ? COLLATE NOCASE`
		args = append(args, author)
	}

	var total int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM packages WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	orderBy := "active_installs DESC"
	switch sort {
	case "updated":
		orderBy = "last_committed DESC NULLS LAST"
	case "least_updated":
		orderBy = "last_committed ASC NULLS LAST"
	}

	offset := (page - 1) * limit
	q := fmt.Sprintf(`SELECT type, name, COALESCE(display_name,''), COALESCE(description,''),
		COALESCE(current_version,''), COALESCE(wporg_version,''), downloads, active_installs, wp_packages_installs_total,
		COALESCE(last_committed,'')
		FROM packages WHERE %s ORDER BY %s LIMIT ? OFFSET ?`, where, orderBy)

	rows, err := db.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var pkgs []packageRow
	for rows.Next() {
		var p packageRow
		if err := rows.Scan(&p.Type, &p.Name, &p.DisplayName, &p.Description, &p.CurrentVersion, &p.WporgVersion, &p.Downloads, &p.ActiveInstalls, &p.WpPackagesInstallsTotal, &p.LastCommitted); err != nil {
			return nil, 0, fmt.Errorf("scanning untagged package row: %w", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, total, rows.Err()
}

type buildRow struct {
	ID              string
	StartedAt       string
	PackagesTotal   int
	PackagesChanged int
	ArtifactCount   int
	Status          string
	IsCurrent       bool
	R2SyncedAt      string
	ErrorMessage    string
	DurationSeconds *int
	DiscoverSeconds *int
	UpdateSeconds   *int
	BuildSeconds    *int
	DeploySeconds   *int
	R2UploadSeconds *int
}

func queryBuilds(ctx context.Context, db *sql.DB) ([]buildRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, started_at, packages_total, packages_changed,
		artifact_count, status, COALESCE(r2_synced_at, ''), COALESCE(error_message, ''),
		duration_seconds, discover_seconds, update_seconds, build_seconds, deploy_seconds, r2_upload_seconds
		FROM builds ORDER BY started_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var builds []buildRow
	for rows.Next() {
		var b buildRow
		_ = rows.Scan(&b.ID, &b.StartedAt, &b.PackagesTotal, &b.PackagesChanged,
			&b.ArtifactCount, &b.Status, &b.R2SyncedAt, &b.ErrorMessage,
			&b.DurationSeconds, &b.DiscoverSeconds, &b.UpdateSeconds, &b.BuildSeconds, &b.DeploySeconds, &b.R2UploadSeconds)
		builds = append(builds, b)
	}
	return builds, rows.Err()
}

type buildChange struct {
	PackageName string
	Action      string
}

type statusPageBuild struct {
	buildRow
	Changes []buildChange
}

type statusPageCheck struct {
	packages.StatusCheck
	Changes []packages.StatusCheckChange
}

func handleStatus(a *app.App, tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()

		dashStats := queryDashboardStats(ctx, a.DB)
		currentID, _ := deploy.CurrentBuildID("storage/repository")
		s := dashStats["Stats"].(struct {
			TotalPackages  int64
			ActivePlugins  int64
			ActiveThemes   int64
			TotalInstalls  int64
			PluginInstalls int64
			ThemeInstalls  int64
			Installs30d    int64
			CurrentBuild   string
			StatsUpdatedAt string
		})
		s.CurrentBuild = currentID
		dashStats["Stats"] = s

		builds, err := queryBuilds(ctx, a.DB)
		if err != nil {
			a.Logger.Error("querying builds for status", "error", err)
			captureError(r, err)
		}

		var statusBuilds []statusPageBuild
		for i := range builds {
			if builds[i].ID == currentID {
				builds[i].IsCurrent = true
			}
			sb := statusPageBuild{buildRow: builds[i]}
			if builds[i].PackagesChanged > 0 {
				changeRows, err := a.DB.QueryContext(ctx, `
					SELECT package_name, action FROM metadata_changes
					WHERE build_id = ? AND timestamp > ?
					ORDER BY id`, builds[i].ID, cutoff)
				if err == nil {
					for changeRows.Next() {
						var c buildChange
						_ = changeRows.Scan(&c.PackageName, &c.Action)
						sb.Changes = append(sb.Changes, c)
					}
					_ = changeRows.Close()
				}
			}
			statusBuilds = append(statusBuilds, sb)
		}

		checks, err := packages.GetStatusChecks(ctx, a.DB, packages.StatusCheckDisplayLimit)
		if err != nil {
			a.Logger.Error("querying status checks for status page", "error", err)
			captureError(r, err)
		}

		var statusChecks []statusPageCheck
		for _, c := range checks {
			sc := statusPageCheck{StatusCheck: c}
			if c.Deactivated > 0 || c.Reactivated > 0 {
				sc.Changes, _ = packages.GetStatusCheckChanges(ctx, a.DB, c.ID)
			}
			statusChecks = append(statusChecks, sc)
		}

		// 24h activity stats for the summary card
		var packagesUpdated24h int64
		_ = a.DB.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT package_name) FROM metadata_changes WHERE timestamp > ?`,
			cutoff).Scan(&packagesUpdated24h)

		var deactivated24h, reactivated24h int64
		_ = a.DB.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(deactivated),0), COALESCE(SUM(reactivated),0)
			 FROM status_checks WHERE started_at > ?`,
			time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339)).Scan(&deactivated24h, &reactivated24h)

		data := map[string]any{
			"Title":              "Status",
			"Stats":              dashStats["Stats"],
			"Builds":             statusBuilds,
			"StatusChecks":       statusChecks,
			"PackagesUpdated24h": packagesUpdated24h,
			"Deactivated24h":     deactivated24h,
			"Reactivated24h":     reactivated24h,
			"AppURL":             a.Config.AppURL,
			"CDNURL":             a.Config.R2.CDNPublicURL,
			"OGImage":            ogImageURL(a.Config, "social/default.png"),
		}
		if len(statusBuilds) > 0 {
			data["LastBuildStartedAt"] = statusBuilds[0].StartedAt
		}
		render(w, r, tmpl.status, "layout", data)
	}
}
