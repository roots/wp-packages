package wporg

import (
	"context"
	"log/slog"
	"strings"
	"testing"
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
