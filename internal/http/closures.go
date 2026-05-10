package http

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/roots/wp-packages/internal/app"
	"github.com/roots/wp-packages/internal/packages"
)

const closuresPerPage = 50

func handleClosures(a *app.App, tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}

		events, total, err := packages.GetClosureEvents(r.Context(), a.DB, page, closuresPerPage)
		if err != nil {
			a.Logger.Error("querying closure events", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		render(w, r, tmpl.closures, "layout", map[string]any{
			"AppURL":     a.Config.AppURL,
			"CDNURL":     a.Config.R2.CDNPublicURL,
			"Events":     events,
			"Total":      total,
			"Page":       page,
			"PerPage":    closuresPerPage,
			"TotalPages": totalPages(total, closuresPerPage),
		})
	}
}

func handleVendorClosures(a *app.App, tmpl *templateSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vendorSlug := r.PathValue("vendor_slug")
		events, err := packages.GetVendorClosureEvents(r.Context(), a.DB, vendorSlug)
		if err != nil {
			a.Logger.Error("querying vendor closure events", "vendor", vendorSlug, "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if len(events) == 0 {
			w.WriteHeader(http.StatusNotFound)
			render(w, r, tmpl.notFound, "layout", map[string]any{"Gone": false, "CDNURL": a.Config.R2.CDNPublicURL})
			return
		}

		statuses, err := packages.GetClosurePluginStatuses(r.Context(), a.DB, uniqueSlugs(events))
		if err != nil {
			a.Logger.Error("querying closure plugin statuses", "error", err)
		}

		render(w, r, tmpl.vendorClosures, "layout", map[string]any{
			"AppURL":     a.Config.AppURL,
			"CDNURL":     a.Config.R2.CDNPublicURL,
			"VendorName": events[0].VendorName,
			"VendorSlug": events[0].VendorSlug,
			"Events":     events,
			"Statuses":   statuses,
		})
	}
}

func handleAPIClosures(a *app.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}

		events, total, err := packages.GetClosureEvents(r.Context(), a.DB, page, closuresPerPage)
		if err != nil {
			a.Logger.Error("api: querying closure events", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events":            formatEvents(events),
			"page":              page,
			"per_page":          closuresPerPage,
			"total":             total,
			"total_pages":       totalPages(total, closuresPerPage),
			"documentation_url": a.Config.AppURL + "/docs#api-closures",
		})
	}
}

func handleAPIVendorClosures(a *app.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vendorSlug := r.PathValue("vendor_slug")
		events, err := packages.GetVendorClosureEvents(r.Context(), a.DB, vendorSlug)
		if err != nil {
			a.Logger.Error("api: querying vendor closure events", "vendor", vendorSlug, "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if len(events) == 0 {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":             "vendor not found",
				"documentation_url": a.Config.AppURL + "/docs#api-vendor-closures",
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"events":            formatEvents(events),
			"documentation_url": a.Config.AppURL + "/docs#api-vendor-closures",
		})
	}
}

func formatEvents(events []packages.ClosureEvent) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"id":                    e.ID,
			"vendor_name":           e.VendorName,
			"vendor_slug":           e.VendorSlug,
			"detected_at":           e.DetectedAt.Format(time.RFC3339),
			"detected_at_formatted": e.DetectedAt.Format("January 2, 2006"),
			"plugin_slugs":          e.PluginSlugs,
			"plugin_count":          e.PluginCount,
		})
	}
	return out
}

func uniqueSlugs(events []packages.ClosureEvent) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, e := range events {
		for _, s := range e.PluginSlugs {
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

type rssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	AtomNS  string     `xml:"xmlns:atom,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	Language      string    `xml:"language"`
	LastBuildDate string    `xml:"lastBuildDate"`
	Items         []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
}

// rssEventDescription returns a scannable summary listing the affected plugin
// slugs (truncated for readability in feed readers).
func rssEventDescription(e packages.ClosureEvent) string {
	const maxSlugs = 10
	slugs := e.PluginSlugs
	suffix := ""
	if len(slugs) > maxSlugs {
		suffix = ", and " + strconv.Itoa(len(slugs)-maxSlugs) + " more"
		slugs = slugs[:maxSlugs]
	}
	return strconv.Itoa(e.PluginCount) + " plugins from " + e.VendorName +
		" closed on WordPress.org: " + strings.Join(slugs, ", ") + suffix
}

func handleClosuresFeed(a *app.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		events, _, err := packages.GetClosureEvents(r.Context(), a.DB, 1, closuresPerPage)
		if err != nil {
			a.Logger.Error("querying closure events for feed", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		feed := rssFeed{
			Version: "2.0",
			AtomNS:  "http://www.w3.org/2005/Atom",
			Channel: rssChannel{
				Title:         "WordPress.org Mass Closures — WP Packages",
				Link:          a.Config.AppURL + "/closures",
				Description:   "Recent mass-closure events on WordPress.org",
				Language:      "en-us",
				LastBuildDate: time.Now().Format(time.RFC1123Z),
			},
		}
		for _, e := range events {
			vendorURL := a.Config.AppURL + "/closures/" + e.VendorSlug
			feed.Channel.Items = append(feed.Channel.Items, rssItem{
				Title:       e.VendorName + ": " + strconv.Itoa(e.PluginCount) + " plugins closed",
				Link:        vendorURL,
				GUID:        vendorURL + "#" + strconv.FormatInt(e.ID, 10),
				PubDate:     e.DetectedAt.Format(time.RFC1123Z),
				Description: rssEventDescription(e),
			})
		}

		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		_, _ = w.Write([]byte(xml.Header))
		_ = xml.NewEncoder(w).Encode(feed)
	}
}
