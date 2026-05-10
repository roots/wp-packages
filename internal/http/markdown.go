package http

import (
	"embed"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/roots/wp-packages/internal/app"
	"github.com/roots/wp-packages/internal/deploy"
	"github.com/roots/wp-packages/internal/packages"
)

//go:embed templates/markdown/*.md
var markdownFS embed.FS

// staticMarkdown caches hand-authored Markdown bodies for pages whose HTML
// is too presentational to mechanically translate. Loaded once at startup.
// Leading HTML comments (used for in-source DUAL-SOURCE pointers) are
// stripped so they don't reach clients in the response body.
var staticMarkdown = func() map[string]string {
	files := map[string]string{
		"home":           "templates/markdown/home.md",
		"compare":        "templates/markdown/compare.md",
		"docs":           "templates/markdown/docs.md",
		"wordpress_core": "templates/markdown/wordpress_core.md",
	}
	out := make(map[string]string, len(files))
	for key, path := range files {
		data, err := markdownFS.ReadFile(path)
		if err != nil {
			panic(fmt.Sprintf("loading embedded markdown %s: %v", path, err))
		}
		out[key] = stripLeadingHTMLComment(string(data))
	}
	return out
}()

// stripLeadingHTMLComment removes a single `<!-- ... -->` block at the
// very top of s, plus any trailing whitespace. Only the first leading
// comment is stripped; comments anywhere else in the body are preserved
// in case they're load-bearing content.
func stripLeadingHTMLComment(s string) string {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(trimmed, "<!--") {
		return s
	}
	end := strings.Index(trimmed, "-->")
	if end < 0 {
		return s
	}
	return strings.TrimLeft(trimmed[end+3:], " \t\r\n")
}

func writeMarkdown(w http.ResponseWriter, body, cacheControl string) {
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(strings.TrimRight(body, "\n") + "\n"))
}

// siteURL returns appURL+path when appURL is set (production), or path
// as-is when appURL is empty (dev/staging). Hand-authored Markdown in
// templates/markdown/*.md keeps absolute prod URLs since those files
// aren't templated; only generated Markdown participates in this
// substitution.
func siteURL(appURL, path string) string {
	if appURL == "" {
		return path
	}
	return appURL + path
}

// totalPages returns the page count for `total` items at `perPage` per
// page. Always returns at least 1 so a single-page result still reads as
// "page 1 of 1" rather than "page 1 of 0".
func totalPages(total, perPage int) int {
	if total <= 0 || perPage <= 0 {
		return 1
	}
	return (total + perPage - 1) / perPage
}

// paginationURL returns a `.md` URL for the given page, preserving every
// existing query parameter (filter, search, sort, etc.) and overwriting
// `page`. baseURL is the `.md` form like `/index.md` or `/untagged.md`.
// appURL, if non-empty, prefixes the result for absolute URLs.
func paginationURL(page int, baseURL, appURL, rawQuery string) string {
	q, _ := url.ParseQuery(rawQuery)
	q.Set("page", strconv.Itoa(page))
	out := baseURL + "?" + q.Encode()
	if appURL != "" {
		out = appURL + out
	}
	return out
}

// setPaginationLinkHeader writes prev/next entries to the Link header,
// appending to anything already there (the negotiation middleware may
// have set its own).
func setPaginationLinkHeader(w http.ResponseWriter, page, total, perPage int, baseURL, appURL, rawQuery string) {
	tp := totalPages(total, perPage)
	var entries []string
	if page > 1 {
		entries = append(entries, fmt.Sprintf("<%s>; rel=\"prev\"", paginationURL(page-1, baseURL, appURL, rawQuery)))
	}
	if page < tp {
		entries = append(entries, fmt.Sprintf("<%s>; rel=\"next\"", paginationURL(page+1, baseURL, appURL, rawQuery)))
	}
	if len(entries) == 0 {
		return
	}
	value := strings.Join(entries, ", ")
	if existing := w.Header().Get("Link"); existing != "" {
		value = existing + ", " + value
	}
	w.Header().Set("Link", value)
}

// appendPaginationFooter writes a "Page X of Y" line with prev/next
// markdown links to b. No-op for single-page results.
func appendPaginationFooter(b *strings.Builder, page, total, perPage int, baseURL, appURL, rawQuery string) {
	tp := totalPages(total, perPage)
	if tp <= 1 {
		return
	}
	parts := make([]string, 0, 3)
	if page > 1 {
		parts = append(parts, fmt.Sprintf("[← Previous page](%s)", paginationURL(page-1, baseURL, appURL, rawQuery)))
	}
	parts = append(parts, fmt.Sprintf("Page %d of %d", page, tp))
	if page < tp {
		parts = append(parts, fmt.Sprintf("[Next page →](%s)", paginationURL(page+1, baseURL, appURL, rawQuery)))
	}
	b.WriteString("\n---\n\n")
	b.WriteString(strings.Join(parts, " · "))
	b.WriteString("\n")
}

// newMarkdownMux returns a parallel ServeMux that produces Markdown
// representations for every negotiable page. The negotiation middleware
// dispatches to it when the request prefers `text/markdown` (via Accept
// header or `.md` URL suffix). Patterns mirror the HTML mux so a single
// path lookup tells us whether a Markdown representation exists.
func newMarkdownMux(a *app.App) *http.ServeMux {
	appURL := a.Config.AppURL
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndexMD(a, appURL))
	mux.HandleFunc("GET /packages/{type}/{name}", handleDetailMD(a, appURL))
	mux.HandleFunc("GET /wp-packages-vs-wpackagist", handleCompareMD())
	mux.HandleFunc("GET /docs", handleDocsMD())
	mux.HandleFunc("GET /wordpress-core", handleWordPressCoreMD())
	mux.HandleFunc("GET /status", handleStatusMD(a))
	mux.HandleFunc("GET /untagged", handleUntaggedMD(a, appURL))
	mux.HandleFunc("GET /closures", handleClosuresMD(a, appURL))
	mux.HandleFunc("GET /closures/{vendor_slug}", handleVendorClosuresMD(a, appURL))

	// Mirror the legacy redirects from router.go so MD-only clients
	// receive a 301 to the canonical Markdown URL instead of a 406.
	// Same destination as the HTML redirect, just `.md` form so the
	// client doesn't need to re-send Accept.
	mux.HandleFunc("GET /wp-composer-vs-wpackagist", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/wp-packages-vs-wpackagist.md", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /roots-wordpress", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/wordpress-core.md", http.StatusMovedPermanently)
	})

	// Catch pattern drift: every key in mdContentParams must resolve
	// to a registered pattern. If someone renames `GET /{$}` to `GET
	// /` (or splits a pattern), tracking-param stripping silently
	// regresses to "ignore all queries" — pagination URLs lose their
	// content query and search results behave like the static
	// homepage. Panic on startup is loud enough to catch in CI or on
	// the first deploy.
	for key := range mdContentParams {
		method, patternPath, ok := strings.Cut(key, " ")
		if !ok {
			panic(fmt.Sprintf("mdContentParams key %q is not in 'METHOD /path' form", key))
		}
		// Convert pattern path to a probe URL. `{$}` is the
		// end-of-path marker matching the empty trailing segment.
		probePath := strings.TrimSuffix(patternPath, "{$}")
		if probePath == "" {
			probePath = "/"
		}
		probe := &http.Request{Method: method, URL: &url.URL{Path: probePath}}
		_, pattern := mux.Handler(probe)
		if pattern != key {
			panic(fmt.Sprintf("mdContentParams key %q does not match any registered MD route (got %q)", key, pattern))
		}
	}

	return mux
}

// hasMarkdownRoute reports whether the Markdown mux has a handler that
// matches r. ServeMux.Handler returns an empty pattern when nothing
// matches, which we use as the signal.
func hasMarkdownRoute(mux *http.ServeMux, r *http.Request) bool {
	_, pattern := mux.Handler(r)
	return pattern != ""
}

func handleIndexMD(a *app.App, appURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// Only honor known content-affecting params. Bare `/` and
		// `/?utm_source=x` both render the static homepage; only
		// search/type/sort/page trigger the dynamic listing.
		hasContentQuery := q.Get("search") != "" || q.Get("type") != "" ||
			q.Get("sort") != "" || q.Get("page") != ""
		if !hasContentQuery {
			writeMarkdown(w, staticMarkdown["home"], "public, max-age=300, s-maxage=3600, stale-while-revalidate=86400")
			return
		}

		filters := publicFilters{
			Search: q.Get("search"),
			Type:   q.Get("type"),
			Sort:   q.Get("sort"),
		}
		if filters.Sort == "" {
			filters.Sort = "composer_installs"
		}
		page, _ := strconv.Atoi(q.Get("page"))
		if page < 1 {
			page = 1
		}

		pkgs, total, err := queryPackages(r.Context(), a.DB, filters, page, perPage)
		if err != nil {
			a.Logger.Error("querying packages for markdown search", "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		// Strip non-content-affecting params (utm_*, gclid, …) so
		// pagination URLs stay canonical.
		cleanQuery := extractContentQuery("GET /{$}", r.URL.RawQuery)
		setPaginationLinkHeader(w, page, total, perPage, "/index.md", appURL, cleanQuery)
		body := renderSearchResultsMarkdown(pkgs, page, total, filters, appURL, cleanQuery)
		writeMarkdown(w, body, "public, max-age=60, s-maxage=300, stale-while-revalidate=3600")
	}
}

func renderSearchResultsMarkdown(pkgs []packageRow, page, total int, f publicFilters, appURL, rawQuery string) string {
	var b strings.Builder
	switch {
	case f.Search != "":
		fmt.Fprintf(&b, "# Search: %q\n\n", f.Search)
	case f.Type == "plugin":
		b.WriteString("# Plugins\n\n")
	case f.Type == "theme":
		b.WriteString("# Themes\n\n")
	default:
		b.WriteString("# Packages\n\n")
	}
	homeURL := "/index.md"
	if appURL != "" {
		homeURL = appURL + homeURL
	}
	fmt.Fprintf(&b, "[← WP Packages home](%s)\n\n", homeURL)

	if total == 0 {
		b.WriteString("_No packages match._\n")
		return b.String()
	}

	tp := totalPages(total, perPage)
	fmt.Fprintf(&b, "**%s** matching package(s). Page %d of %d.\n\n",
		formatNumber(int64(total)), page, tp)

	for _, p := range pkgs {
		display := p.Name
		if p.DisplayName != "" {
			display = p.DisplayName
		}
		pkgURL := siteURL(appURL, fmt.Sprintf("/packages/wp-%s/%s", p.Type, p.Name))
		fmt.Fprintf(&b, "- [%s](%s) — `composer require wp-%s/%s`",
			display, pkgURL, p.Type, p.Name)
		if p.CurrentVersion != "" {
			fmt.Fprintf(&b, " · v%s", p.CurrentVersion)
		}
		if p.ActiveInstalls > 0 {
			fmt.Fprintf(&b, " · %s active installs", formatNumber(p.ActiveInstalls))
		}
		b.WriteString("\n")
	}

	appendPaginationFooter(&b, page, total, perPage, "/index.md", appURL, rawQuery)
	return b.String()
}

func handleDetailMD(a *app.App, appURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pkgType := strings.TrimPrefix(r.PathValue("type"), "wp-")
		name := r.PathValue("name")

		pkg, err := queryPackageDetail(r.Context(), a.DB, pkgType, name)
		if err != nil {
			// Mirror handleDetail: inactive packages get a redirect, not
			// a 404, so MD-only clients land on the same destination as
			// browsers do.
			if packageExistsInactive(r.Context(), a.DB, pkgType, name) {
				http.Redirect(w, r, siteURL(appURL, "/"), http.StatusFound)
				return
			}
			http.Error(w, "Not found\n", http.StatusNotFound)
			return
		}

		versions := parseVersions(pkg)
		untagged := pkg.Type == "plugin" && pkg.WporgVersion != "" && !versionIsTagged(versions, pkg.WporgVersion)
		trunkOnly := untagged && !hasTaggedVersion(versions)

		body := renderDetailMarkdown(pkg, versions, untagged, trunkOnly, appURL)
		writeMarkdown(w, body, "public, max-age=60, s-maxage=3600, stale-while-revalidate=86400")
	}
}

func renderDetailMarkdown(pkg *packageDetail, versions []versionRow, untagged, trunkOnly bool, appURL string) string {
	var b strings.Builder
	display := pkg.Name
	if pkg.DisplayName != "" {
		display = pkg.DisplayName
	}
	fmt.Fprintf(&b, "# %s\n\n", display)
	fmt.Fprintf(&b, "`wp-%s/%s` — WordPress %s\n\n", pkg.Type, pkg.Name, pkg.Type)

	untaggedURL := siteURL(appURL, "/untagged")
	if untagged {
		if trunkOnly {
			fmt.Fprintf(&b, "> **No tagged releases in SVN.** This plugin releases exclusively via SVN trunk. Install with `dev-trunk` — Composer will pin to a specific SVN revision in your lock file. See [all untagged plugins](%s).\n\n", untaggedURL)
		} else {
			fmt.Fprintf(&b, "> **Latest version not tagged in SVN.** WordPress.org reports version %s but no matching tagged release exists, so the latest version isn't available as a tagged Composer release. Install with `dev-trunk` to track trunk, or pin to an older tagged version. See [all untagged plugins](%s).\n\n", pkg.WporgVersion, untaggedURL)
		}
	}

	b.WriteString("## Install\n\n")
	suffix := ""
	if trunkOnly {
		suffix = ":dev-trunk"
	}
	fmt.Fprintf(&b, "```sh\ncomposer require wp-%s/%s%s\n```\n\n", pkg.Type, pkg.Name, suffix)

	b.WriteString("## Details\n\n")
	if pkg.CurrentVersion != "" {
		fmt.Fprintf(&b, "- **Latest version:** %s\n", pkg.CurrentVersion)
	}
	fmt.Fprintf(&b, "- **Active installs:** %s\n", formatNumber(pkg.ActiveInstalls))
	fmt.Fprintf(&b, "- **Composer installs:** %s\n", formatNumber(pkg.WpPackagesInstallsTotal))
	if pkg.Author != "" {
		fmt.Fprintf(&b, "- **Author:** %s\n", pkg.Author)
	}
	wporgSection := "plugins"
	if pkg.Type == "theme" {
		wporgSection = "themes"
	}
	fmt.Fprintf(&b, "- **WordPress.org:** https://wordpress.org/%s/%s/\n", wporgSection, pkg.Name)
	if pkg.Homepage != "" {
		fmt.Fprintf(&b, "- **Homepage:** %s\n", pkg.Homepage)
	}
	b.WriteString("\n")

	if pkg.Description != "" {
		b.WriteString("## Description\n\n")
		b.WriteString(strings.TrimSpace(pkg.Description))
		b.WriteString("\n\n")
	}

	if len(versions) > 0 {
		fmt.Fprintf(&b, "## Versions (%d)\n\n", len(versions))
		limit := 20
		if len(versions) < limit {
			limit = len(versions)
		}
		for i := 0; i < limit; i++ {
			v := versions[i]
			marker := ""
			if v.IsLatest {
				marker = " (latest)"
			}
			fmt.Fprintf(&b, "- `%s`%s — `composer require wp-%s/%s:%s`\n",
				v.Version, marker, pkg.Type, pkg.Name, v.Version)
		}
		if len(versions) > limit {
			pkgURL := siteURL(appURL, fmt.Sprintf("/packages/wp-%s/%s", pkg.Type, pkg.Name))
			fmt.Fprintf(&b, "\n_%d more versions on the [HTML page](%s)._\n",
				len(versions)-limit, pkgURL)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func handleCompareMD() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeMarkdown(w, staticMarkdown["compare"], "public, max-age=3600, stale-while-revalidate=86400")
	}
}

func handleDocsMD() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeMarkdown(w, staticMarkdown["docs"], "public, max-age=3600, stale-while-revalidate=86400")
	}
}

func handleWordPressCoreMD() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeMarkdown(w, staticMarkdown["wordpress_core"], "public, max-age=3600, stale-while-revalidate=86400")
	}
}

func handleStatusMD(a *app.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		dashStats := queryDashboardStats(ctx, a.DB)
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
		s.CurrentBuild, _ = deploy.CurrentBuildID("storage/repository")

		builds, err := queryBuilds(ctx, a.DB)
		if err != nil {
			a.Logger.Error("querying builds for markdown status", "error", err)
			captureError(r, err)
		}
		for i := range builds {
			if builds[i].ID == s.CurrentBuild {
				builds[i].IsCurrent = true
			}
		}

		checks, err := packages.GetStatusChecks(ctx, a.DB, packages.StatusCheckDisplayLimit)
		if err != nil {
			a.Logger.Error("querying status checks for markdown status", "error", err)
			captureError(r, err)
		}

		cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()
		var packagesUpdated24h int64
		_ = a.DB.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT package_name) FROM metadata_changes WHERE timestamp > ?`,
			cutoff).Scan(&packagesUpdated24h)
		var deactivated24h, reactivated24h int64
		_ = a.DB.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(deactivated),0), COALESCE(SUM(reactivated),0)
			 FROM status_checks WHERE started_at > ?`,
			time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339)).
			Scan(&deactivated24h, &reactivated24h)

		body := renderStatusMarkdown(s, builds, checks,
			packagesUpdated24h, deactivated24h, reactivated24h)
		writeMarkdown(w, body, "public, max-age=60, s-maxage=300, stale-while-revalidate=3600")
	}
}

func renderStatusMarkdown(s struct {
	TotalPackages  int64
	ActivePlugins  int64
	ActiveThemes   int64
	TotalInstalls  int64
	PluginInstalls int64
	ThemeInstalls  int64
	Installs30d    int64
	CurrentBuild   string
	StatsUpdatedAt string
}, builds []buildRow, checks []packages.StatusCheck,
	packagesUpdated24h, deactivated24h, reactivated24h int64,
) string {
	var b strings.Builder
	b.WriteString("# Status\n\n")
	b.WriteString("WP Packages build and status check activity.\n\n")

	b.WriteString("## Overview\n\n")
	fmt.Fprintf(&b, "- **Packages:** %s (%s plugins · %s themes)\n",
		formatNumberComma(s.TotalPackages), formatNumberComma(s.ActivePlugins), formatNumberComma(s.ActiveThemes))
	fmt.Fprintf(&b, "- **Composer installs:** %s (%s in last 30d)\n",
		formatNumberComma(s.TotalInstalls), formatNumberComma(s.Installs30d))
	fmt.Fprintf(&b, "- **Activity (24h):** %s packages updated, %d deactivated, %d reactivated\n",
		formatNumberComma(packagesUpdated24h), deactivated24h, reactivated24h)
	if s.CurrentBuild != "" {
		fmt.Fprintf(&b, "- **Current build:** `%s`\n", s.CurrentBuild)
	}
	b.WriteString("\n")

	b.WriteString("## Recent builds\n\n")
	if len(builds) == 0 {
		b.WriteString("_No builds found._\n\n")
	} else {
		b.WriteString("Builds run every 5 minutes.\n\n")
		b.WriteString("| Build ID | Started | Packages | Changed | Status | Duration |\n")
		b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
		limit := 20
		if len(builds) < limit {
			limit = len(builds)
		}
		for i := 0; i < limit; i++ {
			bd := builds[i]
			marker := ""
			if bd.IsCurrent {
				marker = " (current)"
			}
			dur := ""
			if bd.DurationSeconds != nil {
				dur = fmt.Sprintf("%ds", *bd.DurationSeconds)
			}
			fmt.Fprintf(&b, "| `%s`%s | %s | %d | %d | %s | %s |\n",
				bd.ID, marker, bd.StartedAt, bd.PackagesTotal, bd.PackagesChanged, bd.Status, dur)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Recent status checks\n\n")
	if len(checks) == 0 {
		b.WriteString("_No status checks found._\n")
	} else {
		b.WriteString("| Started | Status | Checked | Deactivated | Reactivated | Failed |\n")
		b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
		limit := 20
		if len(checks) < limit {
			limit = len(checks)
		}
		for i := 0; i < limit; i++ {
			c := checks[i]
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d |\n",
				c.StartedAt, c.Status, c.Checked, c.Deactivated, c.Reactivated, c.Failed)
		}
	}
	return b.String()
}

func handleUntaggedMD(a *app.App, appURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		filter := r.URL.Query().Get("filter")
		search := strings.TrimSpace(r.URL.Query().Get("search"))
		author := strings.TrimSpace(r.URL.Query().Get("author"))
		sort := r.URL.Query().Get("sort")

		pkgs, total, err := queryUntaggedPackages(r.Context(), a.DB, filter, search, author, sort, page, untaggedPerPage)
		if err != nil {
			a.Logger.Error("querying untagged packages for markdown", "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		var totalPlugins int64
		_ = a.DB.QueryRowContext(r.Context(),
			"SELECT active_plugins FROM package_stats WHERE id = 1").Scan(&totalPlugins)

		cleanQuery := extractContentQuery("GET /untagged", r.URL.RawQuery)
		setPaginationLinkHeader(w, page, total, untaggedPerPage, "/untagged.md", appURL, cleanQuery)
		body := renderUntaggedMarkdown(pkgs, int64(total), totalPlugins, page, appURL, cleanQuery)
		writeMarkdown(w, body, "public, max-age=3600, stale-while-revalidate=86400")
	}
}

func renderUntaggedMarkdown(pkgs []packageRow, total, totalPlugins int64, page int, appURL, rawQuery string) string {
	var b strings.Builder
	b.WriteString("# Untagged Plugins\n\n")
	b.WriteString("These WordPress plugins have a latest version on WordPress.org that isn't tagged in SVN. Some release exclusively via trunk, while others have older tags but not for the current version. They can be installed via `dev-trunk` but can't be pinned to specific tagged versions.\n\n")
	pct := "0"
	if totalPlugins > 0 {
		pct = fmt.Sprintf("%.1f", float64(total)*100/float64(totalPlugins))
	}
	fmt.Fprintf(&b, "**%s plugins affected** (%s%% of all plugins).\n\n",
		formatNumberComma(total), pct)
	b.WriteString("WordPress [officially recommends](https://developer.wordpress.org/plugins/wordpress-org/how-to-use-subversion/#tags) tagging every release in SVN and is [working on disabling trunk as a release mechanism](https://meta.trac.wordpress.org/ticket/6380). Until then, the best path is to ask plugin authors to tag their releases. See [issue #15](https://github.com/roots/wp-packages/issues/15) for more details.\n\n")

	b.WriteString("## Install via Composer\n\n")
	b.WriteString("Install any plugin from trunk using `dev-trunk`. Composer pins to a specific SVN revision in your lock file:\n\n")
	b.WriteString("```sh\ncomposer require wp-plugin/example-plugin:dev-trunk\n```\n\n")

	if len(pkgs) == 0 {
		b.WriteString("## Plugins\n\n_No plugins match._\n")
		return b.String()
	}

	tp := totalPages(int(total), untaggedPerPage)
	fmt.Fprintf(&b, "## Plugins (page %d of %d)\n\n", page, tp)
	for _, p := range pkgs {
		display := p.Name
		if p.DisplayName != "" {
			display = p.DisplayName
		}
		extra := ""
		if p.CurrentVersion != "" && p.WporgVersion != "" && p.CurrentVersion != p.WporgVersion {
			extra = fmt.Sprintf(" · tagged latest `%s`, wp.org reports `%s`", p.CurrentVersion, p.WporgVersion)
		} else if p.WporgVersion != "" {
			extra = fmt.Sprintf(" · wp.org reports `%s`", p.WporgVersion)
		}
		pkgURL := siteURL(appURL, fmt.Sprintf("/packages/wp-plugin/%s", p.Name))
		fmt.Fprintf(&b, "- [%s](%s)%s — %s active installs\n",
			display, pkgURL, extra, formatNumber(p.ActiveInstalls))
	}

	appendPaginationFooter(&b, page, int(total), untaggedPerPage, "/untagged.md", appURL, rawQuery)
	return b.String()
}

func handleClosuresMD(a *app.App, appURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		events, total, err := packages.GetClosureEvents(r.Context(), a.DB, page, closuresPerPage)
		if err != nil {
			a.Logger.Error("querying closure events for markdown", "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		cleanQuery := extractContentQuery("GET /closures", r.URL.RawQuery)
		setPaginationLinkHeader(w, page, total, closuresPerPage, "/closures.md", appURL, cleanQuery)
		body := renderClosuresMarkdown(events, total, page, appURL, cleanQuery)
		writeMarkdown(w, body, "public, max-age=300, s-maxage=3600, stale-while-revalidate=86400")
	}
}

func renderClosuresMarkdown(events []packages.ClosureEvent, total, page int, appURL, rawQuery string) string {
	var b strings.Builder
	b.WriteString("# WordPress.org Mass Closures\n\n")
	b.WriteString("History of WordPress.org plugin vendors with multiple closures within a 24-hour rolling window.\n\n")
	if len(events) == 0 {
		b.WriteString("_No mass-closure events recorded._\n")
		return b.String()
	}
	tp := totalPages(total, closuresPerPage)
	fmt.Fprintf(&b, "## Events (page %d of %d)\n\n", page, tp)
	b.WriteString("| Vendor | Plugins closed | Detected |\n")
	b.WriteString("| --- | ---: | --- |\n")
	for _, e := range events {
		vendorURL := siteURL(appURL, "/closures/"+e.VendorSlug)
		fmt.Fprintf(&b, "| [%s](%s) | %d | %s |\n",
			e.VendorName, vendorURL, e.PluginCount, e.DetectedAt.Format("2006-01-02"))
	}
	appendPaginationFooter(&b, page, total, closuresPerPage, "/closures.md", appURL, rawQuery)
	return b.String()
}

func handleVendorClosuresMD(a *app.App, appURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vendorSlug := r.PathValue("vendor_slug")
		events, err := packages.GetVendorClosureEvents(r.Context(), a.DB, vendorSlug)
		if err != nil {
			a.Logger.Error("querying vendor closure events for markdown", "vendor", vendorSlug, "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if len(events) == 0 {
			http.NotFound(w, r)
			return
		}

		var allSlugs []string
		for _, e := range events {
			allSlugs = append(allSlugs, e.PluginSlugs...)
		}
		statuses, err := packages.GetClosurePluginStatuses(r.Context(), a.DB, allSlugs)
		if err != nil {
			a.Logger.Error("querying closure plugin statuses for markdown", "error", err)
		}

		body := renderVendorClosuresMarkdown(events, statuses, appURL)
		writeMarkdown(w, body, "public, max-age=300, s-maxage=3600, stale-while-revalidate=86400")
	}
}

func renderVendorClosuresMarkdown(events []packages.ClosureEvent, statuses map[string]*packages.ClosurePluginStatus, appURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", events[0].VendorName)
	b.WriteString("Vendor mass-closure outbreaks detected on WordPress.org.\n\n")
	for _, e := range events {
		fmt.Fprintf(&b, "## %d plugins closed — %s\n\n",
			e.PluginCount, e.DetectedAt.Format("January 2, 2006"))
		b.WriteString("| Plugin | Plugin Slug | Current Status |\n")
		b.WriteString("| --- | --- | --- |\n")
		for _, slug := range e.PluginSlugs {
			status := "Unknown"
			name := slug
			if s, ok := statuses[slug]; ok {
				if s.DisplayName != "" {
					name = s.DisplayName
				}
				switch {
				case s.IsActive:
					status = "Active"
				case s.IsClosed:
					status = "Tombstoned"
				default:
					status = "Closed"
				}
			}
			fmt.Fprintf(&b, "| [%s](https://wordpress.org/plugins/%s/) | `%s` | %s |\n", name, slug, slug, status)
		}
		b.WriteString("\n")
	}
	listURL := siteURL(appURL, "/closures")
	fmt.Fprintf(&b, "[← All mass-closure events](%s)\n", listURL)
	return b.String()
}
