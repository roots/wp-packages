package cmd

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roots/wp-packages/internal/config"
	"github.com/roots/wp-packages/internal/wporg"
)

func TestMergeTagChanges(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	since := now.AddDate(0, 0, -30)
	older := since.Add(-time.Hour)
	mid := since.Add(24 * time.Hour)
	late := since.Add(48 * time.Hour)

	dst := wporg.TagChangeSet{
		Tags: map[string]map[string]time.Time{"akismet": {"1.0": late}},
		All:  map[string]time.Time{"evil": late},
	}
	mergeTagChanges(dst, wporg.TagChangeSet{
		Tags: map[string]map[string]time.Time{
			"akismet": {"1.0": mid, "2.0": older, "3.0": {}},
			"jetpack": {"1.0": mid},
		},
		All: map[string]time.Time{"evil": mid, "old": older, "copied": mid},
	}, since, now)

	wantTags := map[string]map[string]time.Time{
		"akismet": {"1.0": late, "3.0": now},
		"jetpack": {"1.0": mid},
	}
	if !reflect.DeepEqual(dst.Tags, wantTags) {
		t.Errorf("Tags = %v, want %v", dst.Tags, wantTags)
	}
	wantAll := map[string]time.Time{"evil": late, "copied": mid}
	if !reflect.DeepEqual(dst.All, wantAll) {
		t.Errorf("All = %v, want %v", dst.All, wantAll)
	}
}

// fakeSVNLog serves DAV log-reports for revisions 1..head, one commit per
// revision dated base + rev hours. paths maps revision → added path. lag
// drops that many of the newest revisions from every response.
func fakeSVNLog(t *testing.T, head int64, base time.Time, paths map[int64]string, lag int64, requests *atomic.Int64) *httptest.Server {
	t.Helper()
	revRe := regexp.MustCompile(`<S:(start|end)-revision>(\d+)<`)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		var start, end int64
		for _, m := range revRe.FindAllStringSubmatch(string(body), -1) {
			n, _ := strconv.ParseInt(m[2], 10, 64)
			if m[1] == "start" {
				start = n
			} else {
				end = n
			}
		}
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="utf-8"?><S:log-report xmlns:S="svn:" xmlns:D="DAV:">`)
		for rev := start; rev >= end; rev-- {
			if rev > head-lag {
				continue
			}
			path := paths[rev]
			if path == "" {
				path = "/noise/trunk/file.php"
			}
			fmt.Fprintf(&b, `<S:log-item><D:version-name>%d</D:version-name><S:date>%s</S:date><S:added-path>%s</S:added-path></S:log-item>`,
				rev, base.Add(time.Duration(rev)*time.Hour).Format(time.RFC3339Nano), path)
		}
		b.WriteString(`</S:log-report>`)
		_, _ = io.WriteString(w, b.String())
	}))
}

func TestScanTagChangesSince(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(100 * time.Hour)
	since := base.Add(15 * time.Hour) // revision 15's date

	var requests atomic.Int64
	srv := fakeSVNLog(t, 30, base, map[int64]string{
		25: "/recent/tags/1.0",
		18: "/copied/tags",
		12: "/before-window/tags/1.0", // in the last chunk, but older than since
		5:  "/never-reached/tags/1.0",
	}, 0, &requests)
	defer srv.Close()

	client := wporg.NewClient(config.DiscoveryConfig{}, slog.Default())
	got, err := scanTagChangesSince(t.Context(), client, slog.Default(), srv.URL, "plugin", 30, 10, since, now)
	if err != nil {
		t.Fatal(err)
	}

	// Chunks r21-30 and r11-20; r11 predates since, so the scan stops there.
	if n := requests.Load(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
	wantTags := map[string]map[string]time.Time{"recent": {"1.0": base.Add(25 * time.Hour)}}
	if !reflect.DeepEqual(got.Tags, wantTags) {
		t.Errorf("Tags = %v, want %v", got.Tags, wantTags)
	}
	wantAll := map[string]time.Time{"copied": base.Add(18 * time.Hour)}
	if !reflect.DeepEqual(got.All, wantAll) {
		t.Errorf("All = %v, want %v", got.All, wantAll)
	}
}

func TestScanTagChangesSinceFailsOnIncompleteRange(t *testing.T) {
	prev := svnRetryDelay
	svnRetryDelay = time.Millisecond
	t.Cleanup(func() { svnRetryDelay = prev })

	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var requests atomic.Int64
	// The REPORT replica lags 3 revisions behind the head we ask for.
	srv := fakeSVNLog(t, 30, base, nil, 3, &requests)
	defer srv.Close()

	client := wporg.NewClient(config.DiscoveryConfig{}, slog.Default())
	_, err := scanTagChangesSince(t.Context(), client, slog.Default(), srv.URL, "plugin", 30, 10, base, base)
	if err == nil || !strings.Contains(err.Error(), "incomplete SVN log") {
		t.Fatalf("err = %v, want incomplete SVN log error", err)
	}
	if n := requests.Load(); n != 3 {
		t.Errorf("requests = %d, want 3 attempts", n)
	}
}

func TestCheckSVNLogCoverage(t *testing.T) {
	full := &wporg.SVNLog{MinRev: 11, MaxRev: 13, Commits: make([]wporg.SVNCommit, 3)}
	if err := checkSVNLogCoverage(full, 11, 13); err != nil {
		t.Errorf("complete range rejected: %v", err)
	}

	for name, l := range map[string]*wporg.SVNLog{
		"lagging top":       {MinRev: 11, MaxRev: 12, Commits: make([]wporg.SVNCommit, 2)},
		"truncated bottom":  {MinRev: 12, MaxRev: 13, Commits: make([]wporg.SVNCommit, 2)},
		"gap in the middle": {MinRev: 11, MaxRev: 13, Commits: make([]wporg.SVNCommit, 2)},
		"empty":             {},
	} {
		if err := checkSVNLogCoverage(l, 11, 13); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
