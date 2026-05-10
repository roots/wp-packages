package reports

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ClosureEventThreshold is the minimum number of plugin closures from a single
// vendor in a 24h window before a mass-closure event is recorded.
const ClosureEventThreshold = 2

var (
	htmlTagRE     = regexp.MustCompile(`<[^>]*>`)
	nonAlphaNumRE = regexp.MustCompile(`[^a-z0-9]+`)
)

type closure struct {
	Slug   string
	Author string
	Time   time.Time
}

// TrackMassClosures scans for new mass-closure events in the last 24h and
// records or updates them in the closure_events table.
func TrackMassClosures(ctx context.Context, db *sql.DB) error {
	now := time.Now().UTC()
	windowStart := now.Add(-24 * time.Hour)

	closures, err := loadRecentClosures(ctx, db, windowStart)
	if err != nil {
		return err
	}

	groups := groupByVendor(closures)

	for vendorName, items := range groups {
		if len(items) < ClosureEventThreshold {
			continue
		}

		vendorSlug := slugify(vendorName)
		if vendorSlug == "" {
			continue
		}

		// Find an existing event for this vendor whose detection is still
		// within the active 24h window. Same outbreak → update; otherwise
		// the window has reset and we create a new event.
		var eventID int64
		var existingSlugsJSON string
		err := db.QueryRowContext(ctx, `
			SELECT id, plugin_slugs FROM closure_events
			WHERE vendor_slug = ? AND detected_at >= ?
			ORDER BY detected_at DESC LIMIT 1`,
			vendorSlug, windowStart.Format(time.RFC3339)).Scan(&eventID, &existingSlugsJSON)

		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("checking existing event for %s: %w", vendorSlug, err)
		}

		slugs := make(map[string]struct{})
		for _, it := range items {
			slugs[it.Slug] = struct{}{}
		}

		if err == sql.ErrNoRows {
			var sortedSlugs []string
			for s := range slugs {
				sortedSlugs = append(sortedSlugs, s)
			}
			sort.Strings(sortedSlugs)
			slugsJSON, _ := json.Marshal(sortedSlugs)

			_, err = db.ExecContext(ctx, `
				INSERT INTO closure_events (
					vendor_name, vendor_slug, detected_at, plugin_slugs, plugin_count
				) VALUES (?, ?, ?, ?, ?)`,
				vendorName, vendorSlug, now.Format(time.RFC3339),
				string(slugsJSON), len(sortedSlugs))
			if err != nil {
				return fmt.Errorf("creating event for %s: %w", vendorSlug, err)
			}
		} else {
			// Update existing event
			var existingSlugs []string
			if err := json.Unmarshal([]byte(existingSlugsJSON), &existingSlugs); err != nil {
				return fmt.Errorf("unmarshaling existing slugs for %s: %w", vendorSlug, err)
			}
			for _, s := range existingSlugs {
				slugs[s] = struct{}{}
			}

			var sortedSlugs []string
			for s := range slugs {
				sortedSlugs = append(sortedSlugs, s)
			}
			sort.Strings(sortedSlugs)
			slugsJSON, _ := json.Marshal(sortedSlugs)

			_, err = db.ExecContext(ctx, `
				UPDATE closure_events SET plugin_slugs = ?, plugin_count = ?
				WHERE id = ?`,
				string(slugsJSON), len(sortedSlugs), eventID)
			if err != nil {
				return fmt.Errorf("updating event %d: %w", eventID, err)
			}
		}
	}

	return nil
}

func loadRecentClosures(ctx context.Context, db *sql.DB, since time.Time) ([]closure, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT scc.package_name, COALESCE(p.author, ''), scc.created_at
		FROM status_check_changes scc
		LEFT JOIN packages p ON p.type = scc.package_type AND p.name = scc.package_name
		WHERE scc.created_at >= ?
		  AND scc.package_type = 'plugin'
		  AND scc.action IN ('deactivated','tombstoned')
		ORDER BY scc.created_at ASC`, since.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("querying closures: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []closure
	for rows.Next() {
		var c closure
		var createdAtStr string
		if err := rows.Scan(&c.Slug, &c.Author, &createdAtStr); err != nil {
			return nil, err
		}
		c.Author = strings.TrimSpace(html.UnescapeString(htmlTagRE.ReplaceAllString(c.Author, "")))
		if t, err := time.Parse(time.RFC3339, createdAtStr); err == nil {
			c.Time = t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func groupByVendor(closures []closure) map[string][]closure {
	groups := map[string][]closure{}
	displayName := map[string]string{}
	for _, c := range closures {
		key := strings.ToLower(c.Author)
		if key == "" {
			continue
		}
		groups[key] = append(groups[key], c)
		if _, ok := displayName[key]; !ok {
			displayName[key] = c.Author
		}
	}

	out := make(map[string][]closure)
	for key, items := range groups {
		out[displayName[key]] = items
	}
	return out
}

// reservedVendorSlugs collide with sibling routes under /closures/.
var reservedVendorSlugs = map[string]struct{}{
	"feed": {},
}

func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlphaNumRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if _, reserved := reservedVendorSlugs[s]; reserved {
		s += "-vendor"
	}
	return s
}
