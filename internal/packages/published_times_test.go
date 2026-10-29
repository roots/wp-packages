package packages

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/roots/wp-packages/internal/composer"
)

func strPtr(s string) *string { return &s }

func decodeTimes(t *testing.T, s *string) map[string]string {
	t.Helper()
	if s == nil {
		t.Fatal("published times = nil, want JSON")
	}
	var times map[string]string
	if err := json.Unmarshal([]byte(*s), &times); err != nil {
		t.Fatalf("decoding published times %q: %v", *s, err)
	}
	return times
}

func TestMergePublishedTimes(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	stamp := "2026-10-13T12:00:00Z"
	old := "2026-01-01T00:00:00Z"

	t.Run("new package stamps every tagged version", func(t *testing.T) {
		got, err := MergePublishedTimes(`{"1.0":"a","2.0":"b","dev-trunk":""}`, "{}", nil, now)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"1.0": stamp, "2.0": stamp}
		if times := decodeTimes(t, got); !reflect.DeepEqual(times, want) {
			t.Errorf("times = %v, want %v", times, want)
		}
	})

	t.Run("existing versions keep their time", func(t *testing.T) {
		prev := `{"1.0":"` + old + `"}`
		got, err := MergePublishedTimes(`{"1.0":"a","2.0":"b"}`, `{"1.0":"a"}`, &prev, now)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"1.0": old, "2.0": stamp}
		if times := decodeTimes(t, got); !reflect.DeepEqual(times, want) {
			t.Errorf("times = %v, want %v", times, want)
		}
	})

	t.Run("delisted versions keep their time", func(t *testing.T) {
		prev := `{"1.0":"` + old + `","2.0":"` + old + `"}`
		got, err := MergePublishedTimes(`{"2.0":"b"}`, `{"1.0":"a","2.0":"b"}`, &prev, now)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"1.0": old, "2.0": old}
		if times := decodeTimes(t, got); !reflect.DeepEqual(times, want) {
			t.Errorf("times = %v, want %v", times, want)
		}
	})

	t.Run("package awaiting backfill stays unset", func(t *testing.T) {
		got, err := MergePublishedTimes(`{"1.0":"a","2.0":"b"}`, `{"1.0":"a"}`, nil, now)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("times = %q, want nil until backfilled", *got)
		}
	})

	t.Run("invalid stored JSON re-stamps every version", func(t *testing.T) {
		got, err := MergePublishedTimes(`{"1.0":"a","2.0":"b"}`, `{"1.0":"a"}`, strPtr("nope"), now)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"1.0": stamp, "2.0": stamp}
		if times := decodeTimes(t, got); !reflect.DeepEqual(times, want) {
			t.Errorf("times = %v, want %v", times, want)
		}
	})
}

func insertTimesPackage(t *testing.T, database *sql.DB, pkgType, name, versionsJSON string, timesJSON, contentHash *string) {
	t.Helper()
	_, err := database.Exec(`
		INSERT INTO packages (type, name, versions_json, published_times_json, content_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		pkgType, name, versionsJSON, timesJSON, contentHash)
	if err != nil {
		t.Fatalf("inserting %s: %v", name, err)
	}
}

func readTimes(t *testing.T, database *sql.DB, name string) *string {
	t.Helper()
	var s *string
	if err := database.QueryRow(`SELECT published_times_json FROM packages WHERE name = ?`, name).Scan(&s); err != nil {
		t.Fatalf("reading published times for %s: %v", name, err)
	}
	return s
}

func TestResetPublishedTimes(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	stamp := "2026-10-13T12:00:00Z"
	old := "2026-01-01T00:00:00Z"
	future := "2026-10-14T00:00:00Z"

	insertTimesPackage(t, database, "plugin", "akismet", `{"1.0":"a","2.0":"b"}`,
		strPtr(`{"1.0":"`+old+`","2.0":"`+old+`","3.0":"`+future+`"}`), nil)
	insertTimesPackage(t, database, "plugin", "unbackfilled", `{"1.0":"a"}`, nil, nil)
	insertTimesPackage(t, database, "plugin", "corrupt", `{"1.0":"a"}`, strPtr("not json"), nil)
	// Stored before Normalize was idempotent.
	insertTimesPackage(t, database, "plugin", "spaced", `{" 2.1":"a"}`, strPtr(`{" 2.1":"`+old+`"}`), nil)
	insertTimesPackage(t, database, "theme", "akismet", `{"1.0":"a"}`, strPtr(`{"1.0":"`+old+`"}`), nil)

	commit := time.Date(2026, 10, 13, 11, 0, 0, 0, time.UTC)
	n, err := ResetPublishedTimes(ctx, database, "plugin", map[string]map[string]time.Time{
		// v-prefixed tag normalizes to 1.0; 4.0 isn't published yet; 3.0 is
		// already newer than now; trunk isn't a tagged version.
		"akismet":      {"v1.0": commit, "3.0": commit, "4.0": commit, "trunk": commit},
		"unbackfilled": {"1.0": commit},
		"missing":      {"1.0": commit},
		// An unreadable row is skipped instead of failing every reset.
		"corrupt": {"1.0": commit},
		"spaced":  {"v 2.1": commit},
	}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("reset = %d, want 2", n)
	}
	if got := *readTimes(t, database, "spaced"); got != `{" 2.1":"`+stamp+`"}` {
		t.Errorf("spaced times = %s, want legacy key reset", got)
	}

	want := map[string]string{"1.0": stamp, "2.0": old, "3.0": future}
	if times := decodeTimes(t, readTimes(t, database, "akismet")); !reflect.DeepEqual(times, want) {
		t.Errorf("akismet times = %v, want %v", times, want)
	}
	if got := readTimes(t, database, "unbackfilled"); got != nil {
		t.Errorf("unbackfilled times = %q, want NULL", *got)
	}

	var themeTimes string
	_ = database.QueryRow(`SELECT published_times_json FROM packages WHERE type = 'theme'`).Scan(&themeTimes)
	if themeTimes != `{"1.0":"`+old+`"}` {
		t.Errorf("theme with same slug was reset: %s", themeTimes)
	}
}

func TestResetPublishedTimesAllTags(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	stamp := "2026-10-13T12:00:00Z"
	old := "2026-01-01T00:00:00Z"

	// 0.9 is no longer listed but still has a time; it's reset too.
	insertTimesPackage(t, database, "plugin", "evil", `{"1.0":"a","2.0":"b"}`,
		strPtr(`{"0.9":"`+old+`","1.0":"`+old+`","2.0":"`+old+`"}`), nil)
	insertTimesPackage(t, database, "plugin", "bystander", `{"1.0":"a"}`, strPtr(`{"1.0":"`+old+`"}`), nil)

	n, err := ResetPublishedTimes(ctx, database, "plugin", nil, map[string]time.Time{"evil": now, "missing": now}, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("reset = %d, want 3", n)
	}
	want := map[string]string{"0.9": stamp, "1.0": stamp, "2.0": stamp}
	if times := decodeTimes(t, readTimes(t, database, "evil")); !reflect.DeepEqual(times, want) {
		t.Errorf("evil times = %v, want %v", times, want)
	}
	if got := *readTimes(t, database, "bystander"); got != `{"1.0":"`+old+`"}` {
		t.Errorf("bystander was reset: %s", got)
	}
}

func TestBackfillPublishedTimes(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	baseline := now.AddDate(0, 0, -30)
	baselineStamp := "2026-09-13T12:00:00Z"
	tagged := time.Date(2026, 10, 10, 8, 30, 0, 0, time.UTC)

	versions := `{"1.0":"a","2.0":"b","dev-trunk":""}`
	insertTimesPackage(t, database, "plugin", "synced", versions, nil, strPtr("oldhash"))
	insertTimesPackage(t, database, "plugin", "shell", "{}", nil, nil)
	insertTimesPackage(t, database, "plugin", "done", `{"1.0":"a"}`, strPtr(`{"1.0":"2026-10-01T00:00:00Z"}`), strPtr("keep"))
	insertTimesPackage(t, database, "theme", "synced", `{"1.0":"a"}`, nil, nil)

	n, err := BackfillPublishedTimes(ctx, database, "plugin", map[string]map[string]time.Time{
		"synced": {"v2.0": tagged},
		"done":   {"1.0": tagged},
	}, nil, baseline, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("backfilled = %d, want 2 (synced + shell)", n)
	}

	syncedTimes := readTimes(t, database, "synced")
	want := map[string]string{"1.0": baselineStamp, "2.0": "2026-10-10T08:30:00Z"}
	if times := decodeTimes(t, syncedTimes); !reflect.DeepEqual(times, want) {
		t.Errorf("synced times = %v, want %v", times, want)
	}

	var hash, changedAt *string
	_ = database.QueryRow(`SELECT content_hash, content_changed_at FROM packages WHERE type = 'plugin' AND name = 'synced'`).Scan(&hash, &changedAt)
	if wantHash := composer.HashVersions(versions, nil, syncedTimes); hash == nil || *hash != wantHash {
		t.Errorf("content_hash = %v, want recomputed %s", hash, wantHash)
	}
	if changedAt == nil || *changedAt != "2026-10-13T12:00:00Z" {
		t.Errorf("content_changed_at = %v, want now", changedAt)
	}

	if got := readTimes(t, database, "shell"); got == nil || *got != "{}" {
		t.Errorf("shell times = %v, want {}", got)
	}
	_ = database.QueryRow(`SELECT content_hash FROM packages WHERE name = 'shell'`).Scan(&hash)
	if hash != nil {
		t.Errorf("shell content_hash = %q, want NULL (never synced)", *hash)
	}

	if got := readTimes(t, database, "done"); *got != `{"1.0":"2026-10-01T00:00:00Z"}` {
		t.Errorf("already backfilled package was overwritten: %s", *got)
	}

	var themeTimes *string
	_ = database.QueryRow(`SELECT published_times_json FROM packages WHERE type = 'theme'`).Scan(&themeTimes)
	if themeTimes != nil {
		t.Errorf("theme was backfilled by plugin run: %s", *themeTimes)
	}
}

func TestBackfillPublishedTimesAllTags(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	baseline := now.AddDate(0, 0, -30)
	whole := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)

	insertTimesPackage(t, database, "plugin", "copied", `{"1.0":"a","2.0":"b"}`, nil, nil)
	insertTimesPackage(t, database, "plugin", "spaced", `{" 2.1":"a"}`, nil, nil)

	if _, err := BackfillPublishedTimes(ctx, database, "plugin",
		map[string]map[string]time.Time{"copied": {"2.0": later}, "spaced": {"v 2.1": later}},
		map[string]time.Time{"copied": whole}, baseline, now); err != nil {
		t.Fatal(err)
	}

	// Every version gets at least the tags-dir copy date; a later tag change wins.
	want := map[string]string{"1.0": "2026-10-05T00:00:00Z", "2.0": "2026-10-10T00:00:00Z"}
	if times := decodeTimes(t, readTimes(t, database, "copied")); !reflect.DeepEqual(times, want) {
		t.Errorf("copied times = %v, want %v", times, want)
	}
	// A legacy " 2.1" key still matches tag "v 2.1".
	if got := *readTimes(t, database, "spaced"); got != `{" 2.1":"2026-10-10T00:00:00Z"}` {
		t.Errorf("spaced times = %s, want tag date", got)
	}
}

func TestUpsertPackageKeepsPublishedTimesWhenUnset(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	pkg := &Package{Type: "plugin", Name: "akismet", VersionsJSON: `{"1.0":"a"}`, IsActive: true,
		PublishedTimesJSON: strPtr(`{"1.0":"2026-01-01T00:00:00Z"}`)}
	if err := UpsertPackage(ctx, database, pkg); err != nil {
		t.Fatal(err)
	}

	pkg.PublishedTimesJSON = nil
	if err := BatchUpsertPackages(ctx, database, []*Package{pkg}); err != nil {
		t.Fatal(err)
	}
	if got := readTimes(t, database, "akismet"); got == nil || *got != `{"1.0":"2026-01-01T00:00:00Z"}` {
		t.Errorf("published times = %v, want preserved", got)
	}
}
