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
}

// sqlExecutor is the subset of *sql.DB / *sql.Tx that this package needs,
// so the same helpers can run inside or outside a transaction.
type sqlExecutor interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// TrackMassClosures scans for new mass-closure events in the last 24h and
// records or updates them in the closure_events table.
//
// Wrapped in a transaction so a partial run rolls back cleanly. A residual
// SELECT-then-INSERT race exists if two runs execute concurrently — in
// practice prevented by the hourly cron + busy_timeout.
func TrackMassClosures(ctx context.Context, db *sql.DB) error {
	now := time.Now().UTC()
	windowStart := now.Add(-24 * time.Hour)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	closures, err := loadRecentClosures(ctx, tx, windowStart)
	if err != nil {
		return err
	}

	groups := groupByVendor(closures)

	for vendorName, items := range groups {
		// Dedupe by slug first, then check threshold — duplicate closure
		// rows for the same plugin (possible if a re-check fires twice
		// within the window) shouldn't inflate the count.
		slugs := make(map[string]struct{})
		for _, it := range items {
			slugs[it.Slug] = struct{}{}
		}
		if len(slugs) < ClosureEventThreshold {
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
		err := tx.QueryRowContext(ctx, `
			SELECT id, plugin_slugs FROM closure_events
			WHERE vendor_slug = ? AND detected_at >= ?
			ORDER BY detected_at DESC LIMIT 1`,
			vendorSlug, windowStart.Format(time.RFC3339)).Scan(&eventID, &existingSlugsJSON)

		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("checking existing event for %s: %w", vendorSlug, err)
		}

		if err == sql.ErrNoRows {
			// No event in the active 24h window. Before creating a new one,
			// subtract slugs already recorded in this vendor's most recent
			// event — the rolling-window candidate set can include
			// status_check_changes rows from a prior outbreak whose
			// `detected_at` has aged out of cooldown but whose source rows
			// are still within the 24h window. Without this, slow-rolling
			// vendors would get adjacent events that share slugs.
			var priorSlugsJSON string
			pErr := tx.QueryRowContext(ctx, `
				SELECT plugin_slugs FROM closure_events
				WHERE vendor_slug = ?
				ORDER BY detected_at DESC LIMIT 1`, vendorSlug).Scan(&priorSlugsJSON)
			if pErr != nil && pErr != sql.ErrNoRows {
				return fmt.Errorf("checking prior event for %s: %w", vendorSlug, pErr)
			}
			if pErr == nil {
				var priorSlugs []string
				if err := json.Unmarshal([]byte(priorSlugsJSON), &priorSlugs); err != nil {
					return fmt.Errorf("unmarshaling prior slugs for %s: %w", vendorSlug, err)
				}
				for _, s := range priorSlugs {
					delete(slugs, s)
				}
				if len(slugs) < ClosureEventThreshold {
					continue
				}
			}

			var sortedSlugs []string
			for s := range slugs {
				sortedSlugs = append(sortedSlugs, s)
			}
			sort.Strings(sortedSlugs)
			slugsJSON, _ := json.Marshal(sortedSlugs)

			_, err = tx.ExecContext(ctx, `
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

			_, err = tx.ExecContext(ctx, `
				UPDATE closure_events SET plugin_slugs = ?, plugin_count = ?
				WHERE id = ?`,
				string(slugsJSON), len(sortedSlugs), eventID)
			if err != nil {
				return fmt.Errorf("updating event %d: %w", eventID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func loadRecentClosures(ctx context.Context, db sqlExecutor, since time.Time) ([]closure, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT scc.package_name, COALESCE(p.author, '')
		FROM status_check_changes scc
		LEFT JOIN packages p ON p.type = scc.package_type AND p.name = scc.package_name
		WHERE scc.created_at >= ?
		  AND scc.package_type = 'plugin'
		  AND scc.action IN ('deactivated','tombstoned')`, since.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("querying closures: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []closure
	for rows.Next() {
		var c closure
		if err := rows.Scan(&c.Slug, &c.Author); err != nil {
			return nil, err
		}
		c.Author = strings.TrimSpace(html.UnescapeString(htmlTagRE.ReplaceAllString(c.Author, "")))
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
