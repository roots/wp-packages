package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubMDMux returns a Markdown mux that mirrors the real route patterns but
// produces predictable bodies. Tests use it to exercise negotiation
// decisions without touching the database.
func stubMDMux() *http.ServeMux {
	mdMux := http.NewServeMux()
	stub := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = w.Write([]byte(body))
		}
	}
	mdMux.HandleFunc("GET /{$}", stub("# Index MD\n"))
	mdMux.HandleFunc("GET /packages/{type}/{name}", stub("# Detail MD\n"))
	mdMux.HandleFunc("GET /docs", stub("# Docs MD\n"))
	mdMux.HandleFunc("GET /status", stub("# Status MD\n"))
	return mdMux
}

func stubHTMLHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>" + r.URL.Path + "</body></html>"))
	})
}

func runNegotiation(t *testing.T, method, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	handler := withNegotiationMux(stubHTMLHandler(), stubMDMux(), "")
	req := httptest.NewRequest(method, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestNegotiation_DefaultsToHTML(t *testing.T) {
	w := runNegotiation(t, "GET", "/", "")
	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: got %q, want text/html", ct)
	}
	if vary := w.Header().Get("Vary"); !strings.Contains(strings.ToLower(vary), "accept") {
		t.Errorf("Vary: got %q, want to contain accept", vary)
	}
}

func TestNegotiation_AcceptMarkdownDispatchesMD(t *testing.T) {
	w := runNegotiation(t, "GET", "/", "text/markdown")
	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type: got %q, want text/markdown", ct)
	}
	if !strings.Contains(w.Body.String(), "# Index MD") {
		t.Errorf("body: got %q, want index MD body", w.Body.String())
	}
	if vary := w.Header().Get("Vary"); !strings.EqualFold(vary, "Accept") {
		t.Errorf("Vary: got %q, want Accept", vary)
	}
}

func TestNegotiation_DotMdSetsNoindex(t *testing.T) {
	w := runNegotiation(t, "GET", "/docs.md", "")
	if got := w.Header().Get("X-Robots-Tag"); got != "noindex" {
		t.Errorf("X-Robots-Tag on .md URL: got %q, want noindex", got)
	}
}

func TestNegotiation_AcceptMarkdownDoesNotSetNoindex(t *testing.T) {
	// Accept-based MD responses live at the canonical URL — Vary: Accept
	// already prevents dual-indexing. No noindex.
	w := runNegotiation(t, "GET", "/docs", "text/markdown")
	if got := w.Header().Get("X-Robots-Tag"); got != "" {
		t.Errorf("X-Robots-Tag on Accept-based MD: got %q, want empty", got)
	}
}

func TestNegotiation_HTMLDoesNotSetNoindex(t *testing.T) {
	w := runNegotiation(t, "GET", "/docs", "text/html")
	if got := w.Header().Get("X-Robots-Tag"); got != "" {
		t.Errorf("X-Robots-Tag on HTML: got %q, want empty", got)
	}
}

func TestExtractContentQuery(t *testing.T) {
	cases := []struct {
		name       string
		pattern    string
		rawQuery   string
		want       string
		wantSubset []string // unordered membership check (url.Values map iteration)
	}{
		{name: "empty rawQuery returns empty", pattern: "GET /{$}", rawQuery: "", want: ""},
		{name: "unknown pattern returns empty", pattern: "GET /unknown", rawQuery: "search=foo", want: ""},
		{name: "preserves single content param", pattern: "GET /{$}", rawQuery: "search=akismet", want: "search=akismet"},
		{name: "strips utm tracking", pattern: "GET /{$}", rawQuery: "utm_source=newsletter", want: ""},
		{name: "strips gclid", pattern: "GET /{$}", rawQuery: "gclid=xyz", want: ""},
		{name: "preserves content, drops tracking",
			pattern: "GET /{$}", rawQuery: "search=akismet&utm_source=x&gclid=y",
			wantSubset: []string{"search=akismet"}},
		{name: "untagged keeps filter and search",
			pattern: "GET /untagged", rawQuery: "filter=trunk-only&search=woo&fbclid=z",
			wantSubset: []string{"filter=trunk-only", "search=woo"}},
		{name: "empty content param value is dropped",
			pattern: "GET /{$}", rawQuery: "search=&type=plugin",
			want: "type=plugin"},
		{name: "multiple content params",
			pattern: "GET /{$}", rawQuery: "search=foo&type=plugin&page=2",
			wantSubset: []string{"search=foo", "type=plugin", "page=2"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractContentQuery(c.pattern, c.rawQuery)
			if c.want != "" || c.wantSubset == nil {
				if got != c.want {
					t.Errorf("extractContentQuery(%q, %q) = %q, want %q", c.pattern, c.rawQuery, got, c.want)
				}
				return
			}
			for _, frag := range c.wantSubset {
				if !strings.Contains(got, frag) {
					t.Errorf("extractContentQuery(%q, %q) = %q, missing %q", c.pattern, c.rawQuery, got, frag)
				}
			}
			// Tracking params must always be absent
			for _, drop := range []string{"utm_source", "utm_medium", "gclid", "fbclid"} {
				if strings.Contains(got, drop) {
					t.Errorf("extractContentQuery(%q, %q) = %q, should not contain %q", c.pattern, c.rawQuery, got, drop)
				}
			}
		})
	}
}

func TestMDRouteInfo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /docs", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /untagged", func(http.ResponseWriter, *http.Request) {})

	cases := []struct {
		name             string
		path, rawQuery   string
		wantHas          bool
		wantContentQuery string
	}{
		{"matches route, no query", "/", "", true, ""},
		{"matches route, tracking only stripped", "/docs", "utm_source=x", true, ""},
		{"matches route, content query preserved", "/", "search=akismet", true, "search=akismet"},
		{"matches route, untagged filter preserved", "/untagged", "filter=trunk-only&utm=x", true, "filter=trunk-only"},
		{"unknown path returns false", "/no-such-route", "", false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", c.path+"?"+c.rawQuery, nil)
			if c.rawQuery == "" {
				req = httptest.NewRequest("GET", c.path, nil)
			}
			has, cq := mdRouteInfo(mux, req)
			if has != c.wantHas {
				t.Errorf("hasMD: got %v, want %v", has, c.wantHas)
			}
			if cq != c.wantContentQuery {
				t.Errorf("contentQuery: got %q, want %q", cq, c.wantContentQuery)
			}
		})
	}
}

func TestNegotiation_HomeIsQueryAware(t *testing.T) {
	// `/?search=akismet` — / is in mdQueryAware, so the alternate link
	// must carry the query string the home MD handler honors.
	w := runNegotiation(t, "GET", "/?search=akismet", "text/html")
	link := w.Header().Get("Link")
	if !strings.Contains(link, "</index.md?search=akismet>") {
		t.Errorf("Link: got %q, want sibling with preserved query", link)
	}
}

func TestNegotiation_QueryAwareRoutePreservesRawQuery(t *testing.T) {
	// /untagged is in mdQueryAware, so the alternate link must carry
	// the query string the MD handler honors.
	mdMux := http.NewServeMux()
	mdMux.HandleFunc("GET /untagged", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte("# Untagged MD\n"))
	})
	handler := withNegotiationMux(stubHTMLHandler(), mdMux, "")

	req := httptest.NewRequest("GET", "/untagged?filter=trunk-only&page=2", nil)
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	link := w.Header().Get("Link")
	if !strings.Contains(link, "</untagged.md?filter=trunk-only&page=2>") {
		t.Errorf("Link: got %q, want sibling with preserved query", link)
	}
}

func TestNegotiation_TrackingParamsStrippedFromSibling(t *testing.T) {
	// `/docs?utm_source=newsletter` — utm_source isn't a content-
	// affecting param for /docs (which has none), so the alternate link
	// must advertise the canonical /docs.md without the tracking query.
	w := runNegotiation(t, "GET", "/docs?utm_source=newsletter", "text/html")
	link := w.Header().Get("Link")
	if !strings.Contains(link, "</docs.md>") {
		t.Errorf("Link: got %q, want canonical </docs.md>", link)
	}
	if strings.Contains(link, "utm_source") {
		t.Errorf("Link: got %q, should not preserve tracking params", link)
	}
}

func TestNegotiation_TrackingParamsServeMD(t *testing.T) {
	// MD-only Accept on a static page with a tracking param should
	// serve the canonical Markdown — utm_source doesn't change content.
	w := runNegotiation(t, "GET", "/docs?utm_source=newsletter", "text/markdown")
	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type: got %q, want text/markdown", ct)
	}
}

func TestNegotiation_TrackingParamsOnHomeServeStatic(t *testing.T) {
	// `/?utm_source=x` — utm_source is not in mdContentParams for /,
	// so the home should advertise the canonical /index.md (no query),
	// matching the static homepage agents land on by default.
	w := runNegotiation(t, "GET", "/?utm_source=x", "text/html")
	link := w.Header().Get("Link")
	if !strings.Contains(link, "</index.md>") {
		t.Errorf("Link: got %q, want canonical </index.md>", link)
	}
	if strings.Contains(link, "utm_source") {
		t.Errorf("Link: got %q, should not preserve tracking params", link)
	}
}

func TestNegotiation_NonGetMethodPassesThrough(t *testing.T) {
	// POST /docs with an Accept header that excludes everything must
	// still reach the mux so it can produce the correct 405 with Allow,
	// not a misleading 406 from the negotiation layer.
	called := false
	html := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	handler := withNegotiationMux(html, stubMDMux(), "")
	req := httptest.NewRequest("POST", "/docs", nil)
	req.Header.Set("Accept", "image/png")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Fatal("downstream handler not invoked for POST /docs")
	}
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", w.Code)
	}
}

func TestNegotiation_WebmanifestBypasses(t *testing.T) {
	// /manifest.webmanifest is a registered static asset; an unfortunate
	// Accept header shouldn't 406 it before the static handler runs.
	called := false
	html := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/manifest+json")
		_, _ = w.Write([]byte(`{}`))
	})
	handler := withNegotiationMux(html, stubMDMux(), "")
	req := httptest.NewRequest("GET", "/manifest.webmanifest", nil)
	req.Header.Set("Accept", "application/manifest+json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Fatal("static handler not invoked for /manifest.webmanifest")
	}
	if w.Code != 200 {
		t.Errorf("status: got %d, want 200", w.Code)
	}
}

func TestNegotiation_DotMdSuffix(t *testing.T) {
	w := runNegotiation(t, "GET", "/docs.md", "")
	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type: got %q, want text/markdown", ct)
	}
	if !strings.Contains(w.Body.String(), "# Docs MD") {
		t.Errorf("body: got %q, want docs MD body", w.Body.String())
	}
}

func TestNegotiation_IndexDotMd(t *testing.T) {
	// /index.md is the homepage's Markdown sibling and what the layout
	// advertises in <link rel="alternate">. Must dispatch to the index
	// handler.
	w := runNegotiation(t, "GET", "/index.md", "")
	if w.Code != 200 {
		t.Fatalf("/index.md status: got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "# Index MD") {
		t.Errorf("/index.md body: got %q, want index MD", w.Body.String())
	}
}

func TestNegotiation_DotMdMissingRouteIs404(t *testing.T) {
	w := runNegotiation(t, "GET", "/no-such-page.md", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", w.Code)
	}
}

func TestNegotiation_QValuePicksHigher(t *testing.T) {
	w := runNegotiation(t, "GET", "/docs", "text/html;q=0.5, text/markdown;q=0.9")
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type: got %q, want text/markdown (higher q)", ct)
	}
}

func TestNegotiation_QValueRejectsHTML(t *testing.T) {
	// Specific range overrides wildcard regardless of q (RFC 9110).
	w := runNegotiation(t, "GET", "/docs", "text/html;q=0, */*;q=1")
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type: got %q, want text/markdown", ct)
	}
}

func TestNegotiation_Returns406WhenAllRejected(t *testing.T) {
	w := runNegotiation(t, "GET", "/docs", "image/png")
	if w.Code != http.StatusNotAcceptable {
		t.Fatalf("status: got %d, want 406", w.Code)
	}
	if vary := w.Header().Get("Vary"); !strings.EqualFold(vary, "Accept") {
		t.Errorf("Vary: got %q, want Accept", vary)
	}
}

func TestNegotiation_Returns406WhenHTMLRejectedAndNoMD(t *testing.T) {
	// /wp-packages-vs-wpackagist isn't in the stub mux, so it has no MD rep.
	// HTML is rejected with q=0, so we have nothing to serve.
	w := runNegotiation(t, "GET", "/wp-packages-vs-wpackagist", "text/markdown, text/html;q=0")
	if w.Code != http.StatusNotAcceptable {
		t.Fatalf("status: got %d, want 406", w.Code)
	}
}

func TestNegotiation_LinkHeaderOnHTML(t *testing.T) {
	w := runNegotiation(t, "GET", "/docs", "text/html")
	link := w.Header().Get("Link")
	if !strings.Contains(link, "</docs.md>") {
		t.Errorf("Link: got %q, want to contain </docs.md>", link)
	}
	if !strings.Contains(link, `rel="alternate"`) {
		t.Errorf("Link: got %q, want rel=\"alternate\"", link)
	}
	if !strings.Contains(link, `type="text/markdown"`) {
		t.Errorf("Link: got %q, want type=text/markdown", link)
	}
}

func TestNegotiation_LinkHeaderOnRoot(t *testing.T) {
	w := runNegotiation(t, "GET", "/", "text/html")
	link := w.Header().Get("Link")
	if !strings.Contains(link, "</index.md>") {
		t.Errorf("Link for /: got %q, want to contain </index.md>", link)
	}
}

func TestNegotiation_LinkHeaderAbsoluteWithAppURL(t *testing.T) {
	// When the negotiation middleware is given an AppURL, the Link
	// header should advertise an absolute URL. Same value gets stashed
	// in context for the layout's <link rel="alternate"> tag.
	handler := withNegotiationMux(stubHTMLHandler(), stubMDMux(), "https://wp-packages.org")
	req := httptest.NewRequest("GET", "/docs", nil)
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	link := w.Header().Get("Link")
	if !strings.Contains(link, "<https://wp-packages.org/docs.md>") {
		t.Errorf("Link: got %q, want absolute URL", link)
	}
}

func TestNegotiation_NoLinkHeaderWhenNoMDRep(t *testing.T) {
	// /wp-packages-vs-wpackagist isn't in the stub mux.
	w := runNegotiation(t, "GET", "/wp-packages-vs-wpackagist", "text/html")
	if link := w.Header().Get("Link"); strings.Contains(link, "alternate") {
		t.Errorf("Link: got %q, expected no alternate (no MD rep)", link)
	}
}

func TestNegotiation_NoMDFallsThroughToHTML(t *testing.T) {
	// Client prefers MD but path has no MD rep — fall through to HTML
	// since HTML is acceptable (default q=1 for omitted).
	w := runNegotiation(t, "GET", "/wp-packages-vs-wpackagist", "text/markdown, text/html")
	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: got %q, want text/html", ct)
	}
}

func TestNegotiation_ExcludedPathsBypass(t *testing.T) {
	cases := []string{
		"/api/stats",
		"/p2/wp-plugin/akismet.json",
		"/packages.json",
		"/health",
		"/feed.xml",
		"/sitemap.xml",
		"/sitemap-packages-0.xml",
		"/admin",
		"/admin/login",
		"/og/social/default.png",
		"/assets/styles/app.css",
		"/robots.txt",
	}
	handler := withNegotiationMux(stubHTMLHandler(), stubMDMux(), "")
	for _, p := range cases {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Accept", "text/markdown")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		// Excluded paths shouldn't get a Markdown body.
		if strings.HasPrefix(w.Header().Get("Content-Type"), "text/markdown") {
			t.Errorf("%s: dispatched to MD mux, should bypass", p)
		}
		// They also shouldn't have Vary: Accept added.
		if vary := w.Header().Get("Vary"); strings.Contains(strings.ToLower(vary), "accept") {
			t.Errorf("%s: got Vary %q, expected no Accept token", p, vary)
		}
	}
}

func TestNegotiation_BrowserStyleAccept(t *testing.T) {
	// Real-world Chrome Accept header — should pick HTML.
	accept := "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"
	w := runNegotiation(t, "GET", "/docs", accept)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: got %q, want text/html (browser default)", ct)
	}
}

func TestNegotiation_AppendsAcceptToExistingVary(t *testing.T) {
	// Simulate an upstream handler that already set Vary: Cookie.
	html := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Vary", "Cookie")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html></html>"))
	})
	handler := withNegotiationMux(html, stubMDMux(), "")
	req := httptest.NewRequest("GET", "/docs", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	vary := w.Header().Get("Vary")
	tokens := map[string]bool{}
	for _, t := range strings.Split(vary, ",") {
		tokens[strings.ToLower(strings.TrimSpace(t))] = true
	}
	if !tokens["cookie"] || !tokens["accept"] {
		t.Errorf("Vary: got %q, want both Cookie and Accept", vary)
	}
}

func TestNegotiation_DoesNotDuplicateVary(t *testing.T) {
	html := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Vary", "Accept, Cookie")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html></html>"))
	})
	handler := withNegotiationMux(html, stubMDMux(), "")
	req := httptest.NewRequest("GET", "/docs", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	vary := w.Header().Get("Vary")
	count := strings.Count(strings.ToLower(vary), "accept")
	if count != 1 {
		t.Errorf("Vary: got %q, want exactly one Accept token, found %d", vary, count)
	}
}
