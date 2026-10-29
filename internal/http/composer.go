package http

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/roots/wp-packages/internal/app"
	"github.com/roots/wp-packages/internal/composer"
)

// handlePackagesJSON serves the root Composer repository descriptor.
func handlePackagesJSON(a *app.App) http.HandlerFunc {
	data, err := composer.PackagesJSON(a.Config.AppURL)
	if err != nil {
		a.Logger.Error("building packages.json", "error", err)
		return func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

// handleP2Package serves Composer p2 metadata for a single package.
// Handles both /p2/{vendor}/{file} where file is "akismet.json" or "akismet~dev.json".
func handleP2Package(a *app.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vendor := r.PathValue("vendor")
		file := r.PathValue("file")

		pkgType := composer.PackageType(vendor)
		if pkgType == "" {
			http.NotFound(w, r)
			return
		}

		if !strings.HasSuffix(file, ".json") {
			http.NotFound(w, r)
			return
		}

		// file = "akismet.json" or "akismet~dev.json"
		// name = "akismet" or "akismet~dev" (passed to SerializePackage as version filter)
		// slug = "akismet" (DB lookup key)
		name := strings.TrimSuffix(file, ".json")
		slug := strings.TrimSuffix(name, "~dev")

		if slug == "" {
			http.NotFound(w, r)
			return
		}

		var versionsJSON string
		var description, homepage, author, lastCommitted *string
		var trunkRevision *int64
		var publishedTimesJSON *string

		err := a.DB.QueryRowContext(r.Context(), `
			SELECT versions_json, description, homepage, author, last_committed, trunk_revision,
				published_times_json
			FROM packages WHERE type = ? AND name = ? AND is_active = 1`,
			pkgType, slug,
		).Scan(&versionsJSON, &description, &homepage, &author, &lastCommitted, &trunkRevision,
			&publishedTimesJSON)

		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			a.Logger.Error("querying package for composer", "type", pkgType, "name", slug, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		meta := composer.PackageMeta{
			TrunkRevision:  trunkRevision,
			PublishedTimes: composer.ParsePublishedTimes(publishedTimesJSON),
		}
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

		data, err := composer.SerializePackage(pkgType, name, versionsJSON, meta)
		if err != nil {
			a.Logger.Error("serializing package", "type", pkgType, "name", slug, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if data == nil {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}
