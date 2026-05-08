package http

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/roots/wp-packages/internal/app"
	"github.com/roots/wp-packages/internal/http/negotiate"
)

type markdownURLKey struct{}

// markdownURLFromContext returns the URL of the Markdown sibling for the
// current request, or "" when the path has no Markdown representation.
// Set by the negotiation middleware before dispatching HTML.
func markdownURLFromContext(ctx context.Context) string {
	v, _ := ctx.Value(markdownURLKey{}).(string)
	return v
}

// Paths whose representation is non-negotiable (JSON APIs, feeds, sitemaps,
// static assets, admin, framework internals). The negotiation middleware
// passes these straight through without setting Vary: Accept or inspecting
// the Accept header — they don't have a Markdown sibling and shouldn't
// participate in content negotiation at all.
var nonNegotiablePrefixes = []string{
	"/api/",
	"/p2/",
	"/admin",
	"/og/",
	"/assets/",
	"/_",
}

var nonNegotiableExact = map[string]struct{}{
	"/health":                {},
	"/feed.xml":              {},
	"/robots.txt":            {},
	"/sitemap.xml":           {},
	"/packages.json":         {},
	"/metadata/changes.json": {},
	"/downloads":             {},
	"/packages-partial":      {},
	"/untagged-partial":      {},
	"/untagged-authors":      {},
}

// extension allow-list reused from the static-asset escape hatch idiom: any
// path with one of these suffixes skips negotiation entirely. Mirrors the
// list in the acceptmarkdown.com middleware.
var staticExtensions = map[string]struct{}{
	".css": {}, ".js": {}, ".mjs": {}, ".map": {},
	".png": {}, ".jpg": {}, ".jpeg": {}, ".webp": {}, ".gif": {}, ".svg": {}, ".avif": {}, ".ico": {},
	".woff": {}, ".woff2": {}, ".ttf": {}, ".otf": {}, ".eot": {},
	".xml": {}, ".txt": {}, ".json": {}, ".pdf": {}, ".webmanifest": {},
	".mp4": {}, ".webm": {}, ".mp3": {}, ".wav": {}, ".ogg": {}, ".zip": {},
}

// mdContentParams lists the query parameters each Markdown route's
// handler actually inspects. Anything outside this set — utm_source,
// gclid, fbclid, fragments leaking in via JS, etc. — is treated as
// noise: stripped from the advertised sibling URL and ignored when
// deciding whether the route has a meaningful Markdown rep for the
// current request. Routes that don't appear here have no
// content-affecting params at all (any query string is noise).
var mdContentParams = map[string][]string{
	"GET /{$}":      {"search", "type", "sort", "page"},
	"GET /untagged": {"filter", "search", "author", "sort", "page"},
}

// extractContentQuery returns the URL-encoded subset of rawQuery that
// affects the response for a given Markdown mux pattern, or "" when
// rawQuery contains no content-affecting params.
func extractContentQuery(pattern, rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	params := mdContentParams[pattern]
	if len(params) == 0 {
		return ""
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	out := url.Values{}
	for _, p := range params {
		if v := q.Get(p); v != "" {
			out.Set(p, v)
		}
	}
	return out.Encode()
}

// mdRouteInfo reports whether the Markdown mux has a handler for r and,
// if so, what content-affecting query string applies (could be empty
// for query-irrelevant requests).
func mdRouteInfo(mdMux *http.ServeMux, r *http.Request) (hasMD bool, contentQuery string) {
	_, pattern := mdMux.Handler(r)
	if pattern == "" {
		return false, ""
	}
	return true, extractContentQuery(pattern, r.URL.RawQuery)
}

func isNonNegotiable(path string) bool {
	if _, ok := nonNegotiableExact[path]; ok {
		return true
	}
	for _, prefix := range nonNegotiablePrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	if strings.HasPrefix(path, "/sitemap-") && strings.HasSuffix(path, ".xml") {
		return true
	}
	if dot := strings.LastIndexByte(path, '.'); dot >= 0 {
		ext := strings.ToLower(path[dot:])
		if ext == ".md" {
			return false // .md is the negotiation suffix, not a static asset
		}
		if _, ok := staticExtensions[ext]; ok {
			return true
		}
	}
	return false
}

// appendVaryAccept adds "Accept" to the existing Vary header if not already
// present. Vary tokens are case-insensitive per RFC 9110 §12.5.5.
func appendVaryAccept(h http.Header) {
	existing := h.Get("Vary")
	if existing == "" {
		h.Set("Vary", "Accept")
		return
	}
	for _, t := range strings.Split(existing, ",") {
		if strings.EqualFold(strings.TrimSpace(t), "Accept") {
			return
		}
	}
	h.Set("Vary", existing+", Accept")
}

// negotiationWriter wraps an http.ResponseWriter so headers we want on every
// negotiable HTML response (Vary, Link rel=alternate) are injected lazily,
// just before the inner handler commits the status line. Setting them up
// front would work for most handlers, but ETag short-circuits go through
// WriteHeader directly and skip body writes — wrapping is safer.
type negotiationWriter struct {
	http.ResponseWriter
	mdSibling   string // "" if no Markdown rep
	wroteHeader bool
}

func (n *negotiationWriter) WriteHeader(status int) {
	if !n.wroteHeader {
		n.wroteHeader = true
		appendVaryAccept(n.Header())
		if n.mdSibling != "" {
			link := "<" + n.mdSibling + ">; rel=\"alternate\"; type=\"text/markdown\""
			if existing := n.Header().Get("Link"); existing != "" {
				link = existing + ", " + link
			}
			n.Header().Set("Link", link)
		}
	}
	n.ResponseWriter.WriteHeader(status)
}

func (n *negotiationWriter) Write(b []byte) (int, error) {
	if !n.wroteHeader {
		n.WriteHeader(http.StatusOK)
	}
	return n.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer when it implements
// http.Flusher, mirroring statusRecorder. Without this, a streaming
// handler downstream — wrapped first by statusRecorder, then by this
// type — would silently lose flush capability: the type assertion
// would succeed against statusRecorder but its Flush() unwraps to
// negotiationWriter, which (without Flush) doesn't satisfy
// http.Flusher and the call becomes a no-op.
func (n *negotiationWriter) Flush() {
	if f, ok := n.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the inner writer to type-asserting middleware.
func (n *negotiationWriter) Unwrap() http.ResponseWriter {
	return n.ResponseWriter
}

// withNegotiation wraps htmlHandler with HTML/Markdown content negotiation,
// using newMarkdownMux to produce Markdown representations.
//
// Behavior:
//   - Paths in the non-negotiable list are passed straight through.
//   - `/foo.md` strips the suffix and dispatches to the Markdown mux.
//   - Otherwise, parse the Accept header. If the client prefers
//     `text/markdown` and a Markdown rep exists, dispatch to the Markdown
//     mux. If the client rejects every representation we produce, return
//     406. Else fall through to HTML, with `Vary: Accept` and a Link
//     header advertising the Markdown sibling.
func withNegotiation(a *app.App, htmlHandler http.Handler) http.Handler {
	return withNegotiationMux(htmlHandler, newMarkdownMux(a), a.Config.AppURL)
}

// withNegotiationMux is the testable core: same semantics as
// withNegotiation, but takes an explicit Markdown mux so tests can stub
// out the data-driven handlers. appURL is the canonical site origin
// (e.g. "https://wp-packages.org") prepended to Markdown sibling URLs;
// pass "" to emit relative paths instead.
func withNegotiationMux(htmlHandler http.Handler, mdMux *http.ServeMux, appURL string) http.Handler {
	produces := []string{"text/html", "text/markdown"}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if isNonNegotiable(path) {
			htmlHandler.ServeHTTP(w, r)
			return
		}

		// Negotiation is a property of GET/HEAD only. For other methods
		// we'd return 406 before the mux gets a chance to answer 405
		// with Allow, which is misleading. Pass through and let the mux
		// produce the correct error.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			htmlHandler.ServeHTTP(w, r)
			return
		}

		// `.md` suffix forces a Markdown response regardless of Accept.
		if strings.HasSuffix(path, ".md") {
			stripped := strings.TrimSuffix(path, ".md")
			// `/index.md` is the homepage's Markdown sibling — collapse
			// to `/` so it dispatches to the index handler. Empty case
			// covers a bare `.md` request.
			if stripped == "" || stripped == "/index" {
				stripped = "/"
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = stripped
			if !hasMarkdownRoute(mdMux, r2) {
				http.NotFound(w, r)
				return
			}
			appendVaryAccept(w.Header())
			// `.md` URLs are a separately addressable parallel
			// representation of the HTML page; tell crawlers not to
			// index them as their own entries. Accept-based Markdown
			// responses live at the canonical URL and don't need this
			// — `Vary: Accept` already keeps caches and crawlers from
			// confusing the two.
			w.Header().Set("X-Robots-Tag", "noindex")
			mdMux.ServeHTTP(w, r2)
			return
		}

		accept := r.Header.Get("Accept")
		chosen := negotiate.Preferred(accept, produces)

		if chosen == "" {
			// Client sent an Accept header that excludes everything we
			// produce — answer 406. (`Preferred` only returns "" for a
			// non-empty header, since an empty header defaults to the
			// first producible type.)
			appendVaryAccept(w.Header())
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = w.Write([]byte("Not Acceptable\n\nAvailable: text/html, text/markdown\n"))
			return
		}

		// A Markdown rep exists when the MD mux has a handler for this
		// path. We dispatch regardless of unknown query params (utm_*,
		// gclid, etc.) — only content-affecting params, listed in
		// mdContentParams, are preserved on the advertised sibling URL.
		hasMD, contentQuery := mdRouteInfo(mdMux, r)

		if chosen == "text/markdown" {
			if hasMD {
				appendVaryAccept(w.Header())
				mdMux.ServeHTTP(w, r)
				return
			}
			// Client prefers Markdown but this path has no MD rep. Only
			// fall through to HTML if HTML is still acceptable.
			if negotiate.Preferred(accept, []string{"text/html"}) == "" {
				appendVaryAccept(w.Header())
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusNotAcceptable)
				_, _ = w.Write([]byte("Not Acceptable\n\nMarkdown representation not available and HTML is not acceptable.\n"))
				return
			}
		}

		// HTML path. Wrap the writer so Vary: Accept and the Link header
		// are injected just before WriteHeader, on top of whatever the
		// underlying handler sets.
		wrapped := &negotiationWriter{ResponseWriter: w}
		if hasMD {
			sibling := path + ".md"
			if path == "/" {
				sibling = "/index.md"
			}
			if contentQuery != "" {
				sibling += "?" + contentQuery
			}
			if appURL != "" {
				sibling = appURL + sibling
			}
			wrapped.mdSibling = sibling
			// Also expose the URL via request context so the layout can
			// emit `<link rel="alternate" type="text/markdown">` in the
			// document head.
			r = r.WithContext(context.WithValue(r.Context(), markdownURLKey{}, sibling))
		}
		htmlHandler.ServeHTTP(wrapped, r)
	})
}
