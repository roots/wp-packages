package packages

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/roots/wp-packages/internal/composer"
	"github.com/roots/wp-packages/internal/version"
)

// Published times back Composer's published-time field, which the cooldown
// policy (Composer 2.11+) uses to withhold freshly released versions. They must
// reflect when WP Packages first published a version, never anything a plugin
// author controls, and are stored per package in published_times_json as a
// version → RFC3339 map. dev-trunk is excluded; it uses last_committed.
//
// A NULL published_times_json means the package hasn't been backfilled yet
// (see BackfillPublishedTimes) and published-time is omitted for it.

// MergePublishedTimes carries published times forward to a package's new
// version set. Versions that already have a time keep it and new versions are
// stamped with now. Times for versions no longer listed are kept, so a version
// that briefly drops out of the wp.org API doesn't re-enter cooldown when it
// returns; real changes to a tag are caught from the SVN log instead (see
// ResetPublishedTimes).
//
// old is nil for packages that haven't been backfilled. Those stay nil unless
// the package had no versions before, in which case every version is new.
func MergePublishedTimes(newVersionsJSON, oldVersionsJSON string, old *string, now time.Time) (*string, error) {
	if old == nil && oldVersionsJSON != "" && oldVersionsJSON != "{}" {
		return nil, nil
	}

	// An unreadable row is treated as empty, so every version is re-stamped
	// with now: the safe direction, and it repairs the row.
	times := map[string]string{}
	if old != nil {
		if err := json.Unmarshal([]byte(*old), &times); err != nil {
			times = map[string]string{}
		}
	}

	var versions map[string]string
	if err := json.Unmarshal([]byte(newVersionsJSON), &versions); err != nil {
		return nil, fmt.Errorf("parsing versions_json: %w", err)
	}

	stamp := now.UTC().Format(time.RFC3339)
	for ver := range versions {
		if ver == "dev-trunk" {
			continue
		}
		if _, ok := times[ver]; !ok {
			times[ver] = stamp
		}
	}

	data, err := json.Marshal(times)
	if err != nil {
		return nil, fmt.Errorf("marshaling published times: %w", err)
	}
	s := string(data)
	return &s, nil
}

// ResetPublishedTimes re-stamps already-published versions whose SVN tag was
// created or changed, so the cooldown period applies again to a modified
// release. tags maps slug → raw tag name → commit date and all maps slug →
// commit date for packages whose every tag may have changed (see
// wporg.SVNLog.TagChanges); tags are normalized to match versions_json keys.
// Versions without a published time yet are skipped — they're stamped when
// they first appear in the wp.org API. Returns the number of versions reset.
func ResetPublishedTimes(ctx context.Context, db *sql.DB, pkgType string, tags map[string]map[string]time.Time, all map[string]time.Time, now time.Time) (int64, error) {
	slugs := make(map[string]bool, len(tags)+len(all))
	for slug := range tags {
		slugs[slug] = true
	}
	for slug := range all {
		slugs[slug] = true
	}
	if len(slugs) == 0 {
		return 0, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stamp := now.UTC().Format(time.RFC3339)
	var reset int64
	for slug := range slugs {
		var id int64
		var timesJSON *string
		err := tx.QueryRowContext(ctx, `
			SELECT id, published_times_json FROM packages
			WHERE type = ? AND name = ? AND permanently_closed = 0`,
			pkgType, slug,
		).Scan(&id, &timesJSON)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("reading published times for %s/%s: %w", pkgType, slug, err)
		}
		if timesJSON == nil {
			continue
		}

		// Skip an unreadable row rather than failing the whole chunk, which
		// would stall change detection for every package. The slug is still
		// marked changed, and update re-stamps all its versions with now.
		var times map[string]string
		if err := json.Unmarshal([]byte(*timesJSON), &times); err != nil {
			continue
		}

		_, resetAll := all[slug]
		toReset := make(map[string]bool)
		for tag := range tags[slug] {
			if ver := version.Normalize(tag); ver != "" && ver != "dev-trunk" {
				toReset[ver] = true
			}
		}

		changed := false
		for key, prev := range times {
			// Match on the normalized key: rows stored before Normalize was
			// idempotent can hold keys like " 2.1".
			if !resetAll && !toReset[version.Normalize(key)] {
				continue
			}
			if prev < stamp {
				times[key] = stamp
				changed = true
				reset++
			}
		}
		if !changed {
			continue
		}

		data, err := json.Marshal(times)
		if err != nil {
			return 0, fmt.Errorf("marshaling published times: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE packages SET published_times_json = ?, updated_at = ? WHERE id = ?`,
			string(data), stamp, id,
		); err != nil {
			return 0, fmt.Errorf("resetting published times for %s/%s: %w", pkgType, slug, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing: %w", err)
	}
	return reset, nil
}

// BackfillPublishedTimes populates published_times_json for packages of
// pkgType that don't have it yet. WP Packages has no record of when it first
// published existing versions, so each version gets the latest date its SVN
// tag (or, per allTimes, the package's whole tags directory) was created or
// changed within the scanned window (as returned by wporg.SVNLog.TagChanges),
// or baseline — the start of that window — if nothing touched it. Either way the time is never earlier than the
// version's true publication, so it can't shorten a cooldown.
//
// content_hash is recomputed for synced packages so the next deploy uploads
// metadata with published-time. Returns the number of packages backfilled.
func BackfillPublishedTimes(ctx context.Context, db *sql.DB, pkgType string, tagTimes map[string]map[string]time.Time, allTimes map[string]time.Time, baseline, now time.Time) (int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, versions_json, trunk_revision, content_hash
		FROM packages
		WHERE type = ? AND published_times_json IS NULL`, pkgType)
	if err != nil {
		return 0, fmt.Errorf("querying packages: %w", err)
	}

	type backfill struct {
		id                 int64
		publishedTimesJSON string
		contentHash        *string
	}
	var updates []backfill
	for rows.Next() {
		var (
			id            int64
			name          string
			versionsJSON  string
			trunkRevision *int64
			contentHash   *string
		)
		if err := rows.Scan(&id, &name, &versionsJSON, &trunkRevision, &contentHash); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scanning package: %w", err)
		}

		var versions map[string]string
		if err := json.Unmarshal([]byte(versionsJSON), &versions); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("parsing versions_json for %s/%s: %w", pkgType, name, err)
		}

		tagged := make(map[string]time.Time)
		for tag, t := range tagTimes[name] {
			ver := version.Normalize(tag)
			if ver != "" && t.After(tagged[ver]) {
				tagged[ver] = t
			}
		}

		times := make(map[string]string, len(versions))
		for ver := range versions {
			if ver == "dev-trunk" {
				continue
			}
			t := baseline
			if allTimes[name].After(t) {
				t = allTimes[name]
			}
			// versions_json keys can predate Normalize being idempotent (" 2.1").
			if tt := tagged[version.Normalize(ver)]; tt.After(t) {
				t = tt
			}
			times[ver] = t.UTC().Format(time.RFC3339)
		}

		data, err := json.Marshal(times)
		if err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("marshaling published times: %w", err)
		}
		b := backfill{id: id, publishedTimesJSON: string(data)}
		// Shell packages that were never synced have no hash and aren't
		// deployed; leave them for the update step.
		if contentHash != nil {
			hash := composer.HashVersions(versionsJSON, trunkRevision, &b.publishedTimesJSON)
			b.contentHash = &hash
		}
		updates = append(updates, b)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		UPDATE packages SET
			published_times_json = ?,
			content_hash = COALESCE(?, content_hash),
			content_changed_at = CASE WHEN ? IS NULL THEN content_changed_at ELSE ? END,
			updated_at = ?
		WHERE id = ? AND published_times_json IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("preparing statement: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	stamp := now.UTC().Format(time.RFC3339)
	var affected int64
	for _, u := range updates {
		res, err := stmt.ExecContext(ctx, u.publishedTimesJSON, u.contentHash, u.contentHash, stamp, stamp, u.id)
		if err != nil {
			return affected, fmt.Errorf("backfilling published times for package %d: %w", u.id, err)
		}
		n, _ := res.RowsAffected()
		affected += n
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing: %w", err)
	}
	return affected, nil
}
