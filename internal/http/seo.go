package http

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/roots/wp-packages/internal/app"
)

// RSS feed

type feedCache struct {
	mu          sync.RWMutex
	data        []byte
	generatedAt time.Time
}

func handleFeed(a *app.App) http.HandlerFunc {
	cache := &feedCache{}

	return func(w http.ResponseWriter, r *http.Request) {
		cache.mu.RLock()
		fresh := !cache.generatedAt.IsZero() && time.Since(cache.generatedAt) < time.Hour
		cached := cache.data
		cache.mu.RUnlock()

		if !fresh {
			var err error
			cached, err = generateFeed(r.Context(), a.DB, a.Config.AppURL)
			if err != nil {
				a.Logger.Error("generating feed", "error", err)
				captureError(r, err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			cache.mu.Lock()
			cache.data = cached
			cache.generatedAt = time.Now()
			cache.mu.Unlock()
		}

		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(cached)
	}
}

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	XMLNS   string      `xml:"xmlns,attr"`
	Title   string      `xml:"title"`
	Link    []atomLink  `xml:"link"`
	Updated string      `xml:"updated"`
	ID      string      `xml:"id"`
	Entries []atomEntry `xml:"entry"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr,omitempty"`
	Type string `xml:"type,attr,omitempty"`
}

type atomEntry struct {
	Title   string     `xml:"title"`
	Link    []atomLink `xml:"link"`
	ID      string     `xml:"id"`
	Updated string     `xml:"updated"`
	Summary string     `xml:"summary"`
	Author  atomAuthor `xml:"author"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

func generateFeed(ctx context.Context, db *sql.DB, appURL string) ([]byte, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT type, name, COALESCE(display_name, name), COALESCE(current_version, ''),
			COALESCE(author, ''), updated_at
		FROM packages WHERE is_active = 1
		ORDER BY updated_at DESC LIMIT 50`)
	if err != nil {
		return nil, fmt.Errorf("querying packages for feed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []atomEntry
	var latestUpdated string

	for rows.Next() {
		var pkgType, name, displayName, version, author, updatedAt string
		if err := rows.Scan(&pkgType, &name, &displayName, &version, &author, &updatedAt); err != nil {
			return nil, fmt.Errorf("scanning feed row: %w", err)
		}

		if latestUpdated == "" {
			latestUpdated = updatedAt
		}

		pkgURL := appURL + "/packages/wp-" + pkgType + "/" + name

		summary := "Install " + displayName + " with WP Packages: composer require wp-" + pkgType + "/" + name
		if version != "" {
			summary = displayName + " " + version + " — " + summary
		}

		if author == "" {
			author = "WordPress.org"
		}

		entries = append(entries, atomEntry{
			Title:   displayName,
			Link:    []atomLink{{Href: pkgURL, Rel: "alternate"}},
			ID:      pkgURL,
			Updated: updatedAt,
			Summary: summary,
			Author:  atomAuthor{Name: author},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if latestUpdated == "" {
		latestUpdated = time.Now().UTC().Format(time.RFC3339)
	}

	feed := atomFeed{
		XMLNS:   "http://www.w3.org/2005/Atom",
		Title:   "WP Packages — Recently Updated Packages",
		ID:      appURL + "/feed.xml",
		Updated: latestUpdated,
		Link: []atomLink{
			{Href: appURL + "/", Rel: "alternate", Type: "text/html"},
			{Href: appURL + "/feed.xml", Rel: "self", Type: "application/atom+xml"},
		},
		Entries: entries,
	}

	out, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling feed: %w", err)
	}

	return append([]byte(xml.Header), out...), nil
}

func handleRobotsTxt(a *app.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		sitemapURL := "/sitemap.xml"
		if a.Config.AppURL != "" {
			sitemapURL = a.Config.AppURL + sitemapURL
		}

		_, _ = fmt.Fprintf(w, "User-agent: *\nAllow: /\nDisallow: /admin\nDisallow: /*?search=\nDisallow: /*?page=\nDisallow: /packages-partial\n\nSitemap: %s\n", sitemapURL)
	}
}

// Sitemap XML types

type sitemapIndex struct {
	XMLName  xml.Name            `xml:"sitemapindex"`
	XMLNS    string              `xml:"xmlns,attr"`
	Sitemaps []sitemapIndexEntry `xml:"sitemap"`
}

type sitemapIndexEntry struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

type sitemapURLSet struct {
	XMLName xml.Name     `xml:"urlset"`
	XMLNS   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

type sitemapURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

const sitemapPageSize = 2500

type sitemapData struct {
	mu              sync.RWMutex
	index           []byte
	pages           []byte // sitemap-pages.xml (static pages)
	packageSitemaps [][]byte
	generatedAt     time.Time
}

func (s *sitemapData) isFresh() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.generatedAt.IsZero() && time.Since(s.generatedAt) < time.Hour
}

func handleSitemapIndex(a *app.App, data *sitemapData) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := ensureSitemapData(r.Context(), a, data); err != nil {
			a.Logger.Error("generating sitemap", "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		data.mu.RLock()
		out := data.index
		data.mu.RUnlock()

		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(out)
	}
}

func handleSitemapPages(a *app.App, data *sitemapData) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := ensureSitemapData(r.Context(), a, data); err != nil {
			a.Logger.Error("generating sitemap", "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		data.mu.RLock()
		out := data.pages
		data.mu.RUnlock()

		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(out)
	}
}

func handleSitemapPackages(a *app.App, data *sitemapData) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := ensureSitemapData(r.Context(), a, data); err != nil {
			a.Logger.Error("generating sitemap", "error", err)
			captureError(r, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		slug := strings.TrimPrefix(r.URL.Path, "/sitemap-packages-")
		pageStr := strings.TrimSuffix(slug, ".xml")
		page, err := strconv.Atoi(pageStr)
		if err != nil || page < 0 {
			http.NotFound(w, r)
			return
		}

		data.mu.RLock()
		if page >= len(data.packageSitemaps) {
			data.mu.RUnlock()
			http.NotFound(w, r)
			return
		}
		out := data.packageSitemaps[page]
		data.mu.RUnlock()

		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(out)
	}
}

func ensureSitemapData(ctx context.Context, a *app.App, data *sitemapData) error {
	if data.isFresh() {
		return nil
	}

	idx, pages, pkgSitemaps, err := generateSitemaps(ctx, a.DB, a.Config.AppURL)
	if err != nil {
		return err
	}

	data.mu.Lock()
	data.index = idx
	data.pages = pages
	data.packageSitemaps = pkgSitemaps
	data.generatedAt = time.Now()
	data.mu.Unlock()
	return nil
}

func marshalXML(v any) ([]byte, error) {
	out, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}

func generateSitemaps(ctx context.Context, db *sql.DB, appURL string) (index []byte, pages []byte, pkgSitemaps [][]byte, err error) {
	// Static pages sitemap
	pagesURLSet := sitemapURLSet{
		XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9",
		URLs: []sitemapURL{
			{Loc: appURL + "/"},
			{Loc: appURL + "/docs"},
			{Loc: appURL + "/status"},
			{Loc: appURL + "/wordpress-core"},
			{Loc: appURL + "/wp-packages-vs-wpackagist"},
			{Loc: appURL + "/untagged"},
			{Loc: appURL + "/closures"},
		},
	}

	closureRows, err := db.QueryContext(ctx,
		`SELECT vendor_slug, MAX(detected_at) FROM closure_events GROUP BY vendor_slug ORDER BY vendor_slug`)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("querying closures for sitemap: %w", err)
	}
	for closureRows.Next() {
		var slug, detectedAt string
		if err := closureRows.Scan(&slug, &detectedAt); err != nil {
			_ = closureRows.Close()
			return nil, nil, nil, fmt.Errorf("scanning closures sitemap row: %w", err)
		}
		u := sitemapURL{Loc: appURL + "/closures/" + slug}
		if t, err := time.Parse(time.RFC3339, detectedAt); err == nil {
			u.LastMod = t.Format("2006-01-02")
		}
		pagesURLSet.URLs = append(pagesURLSet.URLs, u)
	}
	if err := closureRows.Err(); err != nil {
		_ = closureRows.Close()
		return nil, nil, nil, err
	}
	_ = closureRows.Close()

	pages, err = marshalXML(pagesURLSet)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshaling pages sitemap: %w", err)
	}

	// Package sitemaps
	rows, err := db.QueryContext(ctx,
		`SELECT type, name, updated_at FROM packages WHERE is_active = 1 ORDER BY type, name`)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("querying packages for sitemap: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var allURLs []sitemapURL
	for rows.Next() {
		var pkgType, name, updatedAt string
		if err := rows.Scan(&pkgType, &name, &updatedAt); err != nil {
			return nil, nil, nil, fmt.Errorf("scanning sitemap row: %w", err)
		}

		u := sitemapURL{
			Loc: appURL + "/packages/wp-" + pkgType + "/" + name,
		}
		if t, err := time.Parse(time.RFC3339, updatedAt); err == nil {
			u.LastMod = t.Format("2006-01-02")
		}
		allURLs = append(allURLs, u)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}

	for i := 0; i < len(allURLs); i += sitemapPageSize {
		end := i + sitemapPageSize
		if end > len(allURLs) {
			end = len(allURLs)
		}

		urlSet := sitemapURLSet{
			XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9",
			URLs:  allURLs[i:end],
		}
		out, err := marshalXML(urlSet)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("marshaling package sitemap: %w", err)
		}
		pkgSitemaps = append(pkgSitemaps, out)
	}

	// Sitemap index
	now := time.Now().UTC().Format("2006-01-02")
	entries := []sitemapIndexEntry{
		{Loc: appURL + "/sitemap-pages.xml", LastMod: now},
	}
	for i := range pkgSitemaps {
		entries = append(entries, sitemapIndexEntry{
			Loc:     fmt.Sprintf("%s/sitemap-packages-%d.xml", appURL, i),
			LastMod: now,
		})
	}

	idx := sitemapIndex{
		XMLNS:    "http://www.sitemaps.org/schemas/sitemap/0.9",
		Sitemaps: entries,
	}
	index, err = marshalXML(idx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshaling sitemap index: %w", err)
	}

	return index, pages, pkgSitemaps, nil
}
