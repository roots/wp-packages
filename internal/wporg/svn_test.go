package wporg

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseSVNHTML(t *testing.T) {
	html := `<html><head><title> - Revision 123: /</title></head>
<body>
<h2> - Revision 123: /</h2>
<ul>
<li><a href="akismet/">akismet/</a></li>
<li><a href="jetpack/">jetpack/</a></li>
</ul>
</body></html>`

	var entries []SVNEntry
	result, err := parseSVNHTML(context.Background(), strings.NewReader(html), func(e SVNEntry) error {
		entries = append(entries, e)
		return nil
	}, slog.Default())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	if entries[0].Slug != "akismet" {
		t.Errorf("first entry slug = %q, want akismet", entries[0].Slug)
	}
	if entries[1].Slug != "jetpack" {
		t.Errorf("second entry slug = %q, want jetpack", entries[1].Slug)
	}

	if result.Revision != 123 {
		t.Errorf("revision = %d, want 123", result.Revision)
	}
}

func TestParseSVNHTML_SkipsNonEntries(t *testing.T) {
	html := `<html><body><ul>
<li><a href="../">..</a></li>
<li><a href="plugin-a/">plugin-a/</a></li>
</ul></body></html>`

	var entries []SVNEntry
	_, err := parseSVNHTML(context.Background(), strings.NewReader(html), func(e SVNEntry) error {
		entries = append(entries, e)
		return nil
	}, slog.Default())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

func TestParseSVNHTML_ContextCancelled(t *testing.T) {
	html := `<html><body><ul>
<li><a href="a/">a/</a></li>
<li><a href="b/">b/</a></li>
</ul></body></html>`

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := parseSVNHTML(ctx, strings.NewReader(html), func(e SVNEntry) error {
		return nil
	}, slog.Default())

	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestParseSVNRevision(t *testing.T) {
	tests := []struct {
		line string
		want int64
	}{
		{`<title> - Revision 3483213: /</title>`, 3483213},
		{`<h2> - Revision 3483213: /</h2>`, 3483213},
		{`<title>Revision 999: /</title>`, 999},
		{`<li><a href="akismet/">akismet/</a></li>`, 0},
		{`no revision here`, 0},
	}

	for _, tt := range tests {
		got := parseSVNRevision(tt.line)
		if got != tt.want {
			t.Errorf("parseSVNRevision(%q) = %d, want %d", tt.line, got, tt.want)
		}
	}
}

func TestParseSVNLogSlugs(t *testing.T) {
	xml := `<?xml version="1.0" encoding="utf-8"?>
<S:log-report xmlns:S="svn:" xmlns:D="DAV:">
<S:log-item>
<D:version-name>100</D:version-name>
<S:date>2026-03-15T10:00:00.000000Z</S:date>
<S:modified-path node-kind="file">/akismet/trunk/akismet.php</S:modified-path>
<S:added-path node-kind="dir">/akismet/tags/5.0</S:added-path>
</S:log-item>
<S:log-item>
<D:version-name>101</D:version-name>
<S:date>2026-03-15T11:00:00.000000Z</S:date>
<S:modified-path node-kind="file">/jetpack/trunk/jetpack.php</S:modified-path>
<S:modified-path node-kind="file">/akismet/trunk/readme.txt</S:modified-path>
</S:log-item>
</S:log-report>`

	slugRevisions, maxRev, err := parseSVNLogSlugs([]byte(xml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if maxRev != 101 {
		t.Errorf("maxRev = %d, want 101 (highest revision in the response)", maxRev)
	}

	if len(slugRevisions) != 2 {
		t.Fatalf("expected 2 unique slugs, got %d: %v", len(slugRevisions), slugRevisions)
	}
	if rev, ok := slugRevisions["akismet"]; !ok {
		t.Error("expected akismet in slugs")
	} else if rev != 101 {
		t.Errorf("akismet revision = %d, want 101 (highest revision that touched it)", rev)
	}
	if rev, ok := slugRevisions["jetpack"]; !ok {
		t.Error("expected jetpack in slugs")
	} else if rev != 101 {
		t.Errorf("jetpack revision = %d, want 101", rev)
	}
}

// TestParseSVNLogSlugsMaxRevReflectsResponse guards the watermark-advance fix:
// maxRev must reflect the highest revision actually present in the REPORT
// response, not any externally-requested bound. When the REPORT replica lags,
// the response stops short of the requested range, and the caller must advance
// the watermark only this far so the gap is rescanned rather than skipped.
func TestParseSVNLogSlugsMaxRevReflectsResponse(t *testing.T) {
	// Requested up to a high revision, but the replica only returned up to 205.
	xml := `<?xml version="1.0" encoding="utf-8"?>
<S:log-report xmlns:S="svn:" xmlns:D="DAV:">
<S:log-item>
<D:version-name>204</D:version-name>
<S:date>2026-05-29T10:00:00.000000Z</S:date>
<S:modified-path node-kind="file">/colissimo-shipping-methods-for-woocommerce/trunk/readme.txt</S:modified-path>
</S:log-item>
<S:log-item>
<D:version-name>205</D:version-name>
<S:date>2026-05-29T10:05:00.000000Z</S:date>
<S:added-path node-kind="dir">/colissimo-shipping-methods-for-woocommerce/tags/2.10.0</S:added-path>
</S:log-item>
</S:log-report>`

	slugRevisions, maxRev, err := parseSVNLogSlugs([]byte(xml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if maxRev != 205 {
		t.Errorf("maxRev = %d, want 205 (highest revision in response)", maxRev)
	}
	if rev := slugRevisions["colissimo-shipping-methods-for-woocommerce"]; rev != 205 {
		t.Errorf("slug revision = %d, want 205", rev)
	}
}

// TestParseSVNLogSlugsEmpty ensures an empty (but valid) response yields maxRev 0
// so the caller leaves its watermark untouched instead of advancing blindly.
func TestParseSVNLogSlugsEmpty(t *testing.T) {
	xml := `<?xml version="1.0" encoding="utf-8"?>
<S:log-report xmlns:S="svn:" xmlns:D="DAV:">
</S:log-report>`

	slugRevisions, maxRev, err := parseSVNLogSlugs([]byte(xml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if maxRev != 0 {
		t.Errorf("maxRev = %d, want 0 for empty response", maxRev)
	}
	if len(slugRevisions) != 0 {
		t.Errorf("expected no slugs, got %d", len(slugRevisions))
	}
}

func TestSlugFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/akismet/trunk/akismet.php", "akismet"},
		{"/jetpack/tags/1.0/jetpack.php", "jetpack"},
		{"/my-plugin/trunk/readme.txt", "my-plugin"},
		{"akismet/trunk/file.php", "akismet"},
		{"/", ""},
		{"", ""},
		{"..", ""},
	}

	for _, tt := range tests {
		got := slugFromPath(tt.path)
		if got != tt.want {
			t.Errorf("slugFromPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// parseSVNLogSlugs adapts parseSVNLog for the slug/watermark assertions above.
func parseSVNLogSlugs(data []byte) (map[string]int64, int64, error) {
	l, err := parseSVNLog(data)
	if err != nil {
		return nil, 0, err
	}
	return l.SlugRevisions, l.MaxRev, nil
}

func TestParseSVNLogReplacedPaths(t *testing.T) {
	xml := `<?xml version="1.0" encoding="utf-8"?>
<S:log-report xmlns:S="svn:" xmlns:D="DAV:">
<S:log-item>
<D:version-name>300</D:version-name>
<S:date>2026-10-01T09:30:00.123456Z</S:date>
<S:replaced-path node-kind="dir" copyfrom-path="/akismet/trunk" copyfrom-rev="299">/akismet/tags/5.0</S:replaced-path>
</S:log-item>
</S:log-report>`

	l, err := parseSVNLog([]byte(xml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.MinRev != 300 || l.MaxRev != 300 {
		t.Errorf("MinRev/MaxRev = %d/%d, want 300/300", l.MinRev, l.MaxRev)
	}
	if l.SlugRevisions["akismet"] != 300 {
		t.Errorf("akismet revision = %d, want 300 (replaced paths count as changes)", l.SlugRevisions["akismet"])
	}
	want := time.Date(2026, 10, 1, 9, 30, 0, 123456000, time.UTC)
	if !l.MinDate.Equal(want) {
		t.Errorf("MinDate = %v, want %v", l.MinDate, want)
	}
	if len(l.Commits) != 1 || !l.Commits[0].Date.Equal(want) {
		t.Errorf("commit date not parsed: %+v", l.Commits)
	}
}

func TestTagChangesPlugin(t *testing.T) {
	d1 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	l := &SVNLog{Commits: []SVNCommit{
		// Tag created by copy, plus an unrelated trunk change.
		{Revision: 1, Date: d1, Added: []string{"/akismet/tags/5.0"}, Modified: []string{"/akismet/trunk/akismet.php"}},
		// Readme-only edit of an existing tag ("Tested up to" bump) is ignored.
		{Revision: 2, Date: d2, Modified: []string{"/akismet/tags/4.0/readme.txt", "/akismet/trunk/readme.txt"},
			PlainFilePaths: map[string]bool{"/akismet/tags/4.0/readme.txt": true, "/akismet/trunk/readme.txt": true}},
		// Code edit inside an existing tag is a change, even alongside a readme edit.
		{Revision: 3, Date: d2, Modified: []string{"/jetpack/tags/1.0/readme.txt", "/jetpack/tags/1.0/jetpack.php"}},
		// Deleting a whole tag or a file inside one is a change; deleting a
		// whole tag can switch which raw tag a version resolves to.
		{Revision: 4, Date: d2, Deleted: []string{"/hello-dolly/tags/1.6", "/hello-dolly/tags/1.7/extra.php"}},
		// Property changes on a tag dir or a dir inside one (svn:externals
		// changes source checkouts) count.
		{Revision: 8, Date: d1, Modified: []string{"/props/tags/2.0", "/props/tags/3.0/includes"}},
		// A readme-only deletion is still ignored.
		{Revision: 9, Date: d2, Deleted: []string{"/quiet/tags/1.0/readme.txt"},
			PlainFilePaths: map[string]bool{"/quiet/tags/1.0/readme.txt": true}},
		// A directory at a readme path isn't a readme edit: SVN logs only the
		// copied directory, not the code inside it.
		{Revision: 10, Date: d1, Replaced: []string{"/sneaky/tags/1.0/readme.md"}},
		{Revision: 11, Date: d1, Added: []string{"/sneaky/tags/2.0/readme.txt"}},
		// Replacing a readme counts even when the new node is a file: the old
		// node may have been a directory of code, which the log doesn't show.
		{Revision: 12, Date: d2, Replaced: []string{"/sneaky/tags/3.0/readme.md"},
			PlainFilePaths: map[string]bool{"/sneaky/tags/3.0/readme.md": true}},
		// Retag via replace.
		{Revision: 5, Date: d2, Replaced: []string{"/akismet/tags/3.0"}},
		// Paths outside tags.
		{Revision: 6, Date: d2, Modified: []string{"/akismet/assets/banner.png", "/akismet/trunk/akismet.php"}},
		// wp.org lists tags/.1 as version 0.1.
		{Revision: 7, Date: d2, Modified: []string{"/old-plugin/tags/.1/old-plugin.php"}},
	}}

	got := l.TagChanges("plugin")
	if len(got.All) != 0 {
		t.Errorf("All = %v, want none", got.All)
	}
	want := map[string]map[string]time.Time{
		"akismet":     {"5.0": d1, "3.0": d2},
		"jetpack":     {"1.0": d2},
		"hello-dolly": {"1.6": d2, "1.7": d2},
		"props":       {"2.0": d1, "3.0": d1},
		"sneaky":      {"1.0": d1, "2.0": d1, "3.0": d2},
		"old-plugin":  {"0.1": d2},
	}
	if !reflect.DeepEqual(got.Tags, want) {
		t.Errorf("TagChanges(plugin) = %v, want %v", got.Tags, want)
	}
}

func TestParseSVNLogNodeKinds(t *testing.T) {
	xml := `<?xml version="1.0" encoding="utf-8"?>
<S:log-report xmlns:S="svn:" xmlns:D="DAV:">
<S:log-item>
<D:version-name>400</D:version-name>
<S:date>2026-10-01T09:30:00.000000Z</S:date>
<S:modified-path node-kind="file" text-mods="true" prop-mods="false">/akismet/tags/5.0/readme.txt</S:modified-path>
</S:log-item>
<S:log-item>
<D:version-name>401</D:version-name>
<S:date>2026-10-01T09:31:00.000000Z</S:date>
<S:replaced-path node-kind="dir" copyfrom-path="/akismet/branches/x" copyfrom-rev="399">/akismet/tags/4.0/readme.md</S:replaced-path>
</S:log-item>
</S:log-report>`

	l, err := parseSVNLog([]byte(xml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !l.Commits[0].PlainFilePaths["/akismet/tags/5.0/readme.txt"] {
		t.Error("file node-kind not recorded")
	}
	if l.Commits[1].PlainFilePaths["/akismet/tags/4.0/readme.md"] {
		t.Error("dir recorded as a file")
	}
	if l.Commits[1].Replaced[0] != "/akismet/tags/4.0/readme.md" {
		t.Errorf("replaced path = %q", l.Commits[1].Replaced[0])
	}

	got := l.TagChanges("plugin").Tags["akismet"]
	if _, ok := got["5.0"]; ok {
		t.Error("readme file edit should be ignored")
	}
	if _, ok := got["4.0"]; !ok {
		t.Error("directory replaced at a readme path should count")
	}
}

func TestTagChangesReadmePropertyChanges(t *testing.T) {
	xml := `<?xml version="1.0" encoding="utf-8"?>
<S:log-report xmlns:S="svn:" xmlns:D="DAV:">
<S:log-item>
<D:version-name>500</D:version-name>
<S:date>2026-10-01T09:30:00.000000Z</S:date>
<S:modified-path node-kind="file" text-mods="false" prop-mods="true">/akismet/tags/5.0/readme.txt</S:modified-path>
<S:modified-path node-kind="file" text-mods="true">/akismet/tags/4.0/readme.txt</S:modified-path>
<S:modified-path node-kind="file" text-mods="true" prop-mods="false">/akismet/tags/3.0/readme.txt</S:modified-path>
</S:log-item>
</S:log-report>`

	l, err := parseSVNLog([]byte(xml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := l.TagChanges("plugin").Tags["akismet"]
	// svn:special on a readme makes it a symlink in source checkouts.
	if _, ok := got["5.0"]; !ok {
		t.Error("readme property change should count")
	}
	if _, ok := got["4.0"]; !ok {
		t.Error("readme edit without prop-mods should count (unknown)")
	}
	if _, ok := got["3.0"]; ok {
		t.Error("plain readme text edit should be ignored")
	}
}

func TestTagChangesKeepsLatestDate(t *testing.T) {
	d1 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	l := &SVNLog{Commits: []SVNCommit{
		{Revision: 2, Date: d2, Modified: []string{"/akismet/tags/5.0/akismet.php"}},
		{Revision: 1, Date: d1, Added: []string{"/akismet/tags/5.0"}},
	}}

	if got := l.TagChanges("plugin").Tags["akismet"]["5.0"]; !got.Equal(d2) {
		t.Errorf("tag date = %v, want latest %v", got, d2)
	}
}

func TestTagChangesTheme(t *testing.T) {
	d := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	l := &SVNLog{Commits: []SVNCommit{
		{Revision: 1, Date: d, Added: []string{"/astra/4.8.0", "/astra/4.8.0/style.css"}},
		{Revision: 2, Date: d, Modified: []string{"/astra/4.7.0/readme.txt"},
			PlainFilePaths: map[string]bool{"/astra/4.7.0/readme.txt": true}},
		{Revision: 3, Date: d, Added: []string{"/new-theme"}},
	}}

	got := l.TagChanges("theme")
	want := map[string]map[string]time.Time{"astra": {"4.8.0": d}}
	if !reflect.DeepEqual(got.Tags, want) {
		t.Errorf("TagChanges(theme) = %v, want %v", got.Tags, want)
	}
	// Adding a theme root counts as every tag changing (a no-op for a new
	// theme, which has no published times yet).
	if wantAll := map[string]time.Time{"new-theme": d}; !reflect.DeepEqual(got.All, wantAll) {
		t.Errorf("TagChanges(theme).All = %v, want %v", got.All, wantAll)
	}
}

// TestTagChangesWholeTagsDir covers copies over a package's tags directory or
// root: SVN logs only the copied directory, never the files inside it.
func TestTagChangesWholeTagsDir(t *testing.T) {
	d1 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	l := &SVNLog{Commits: []SVNCommit{
		{Revision: 1, Date: d1, Replaced: []string{"/evil/tags"}},
		{Revision: 2, Date: d2, Added: []string{"/restored/tags"}},
		{Revision: 3, Date: d1, Replaced: []string{"/swapped"}},
		{Revision: 4, Date: d2, Added: []string{"/swapped"}},
		// Property changes on the tags dir or root don't apply to tag
		// checkouts or zips.
		{Revision: 5, Date: d1, Modified: []string{"/props/tags", "/props"}},
		// Deleting the tags directory or root isn't a change.
		{Revision: 6, Date: d2, Deleted: []string{"/gone/tags", "/gone"}},
		// trunk and assets aren't ancestors of tags.
		{Revision: 7, Date: d2, Replaced: []string{"/other/trunk", "/other/assets"}},
	}}

	got := l.TagChanges("plugin")
	want := map[string]time.Time{"evil": d1, "restored": d2, "swapped": d2}
	if !reflect.DeepEqual(got.All, want) {
		t.Errorf("TagChanges(plugin).All = %v, want %v", got.All, want)
	}
	if len(got.Tags) != 0 {
		t.Errorf("Tags = %v, want none", got.Tags)
	}

	// For themes, /<slug>/tags is a version directory named "tags", not an
	// ancestor of every version.
	theme := l.TagChanges("theme")
	if _, ok := theme.All["evil"]; ok {
		t.Error("theme /evil/tags should not count as every tag changing")
	}
	if _, ok := theme.All["swapped"]; !ok {
		t.Error("theme root replace should count as every tag changing")
	}
}
