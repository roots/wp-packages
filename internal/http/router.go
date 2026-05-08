package http

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	sentryhttp "github.com/getsentry/sentry-go/http"
	"github.com/roots/wp-packages/internal/app"
)

// cacheControl wraps an http.Handler and sets the Cache-Control header.
func cacheControl(value string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", value)
		next.ServeHTTP(w, r)
	})
}

// hashPattern matches the content hash inserted by assetPath (e.g. ".a1b2c3d4e5f6").
var hashPattern = regexp.MustCompile(`\.[0-9a-f]{12}(\.[^.]+)$`)

// stripAssetHash removes the content hash from the URL path so the embedded
// file server can find the original file.
// e.g. "/assets/styles/app.a1b2c3d4e5f6.css" → "/assets/styles/app.css"
func stripAssetHash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = hashPattern.ReplaceAllString(r.URL.Path, "$1")
		next.ServeHTTP(w, r)
	})
}

func NewRouter(a *app.App) http.Handler {
	mux := http.NewServeMux()
	tmpl := loadTemplates(a.Config.Env)

	// route registers a handler on the mux, wrapping it with routeMarker so
	// appHandler can distinguish matched routes from mux-internal 404/405.
	route := func(pattern string, handler http.Handler) {
		mux.Handle(pattern, routeMarker(handler))
	}
	routeFunc := func(pattern string, handler http.HandlerFunc) {
		route(pattern, handler)
	}

	sentryMiddleware := sentryhttp.New(sentryhttp.Options{Repanic: true})

	routeFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	staticSub, _ := fs.Sub(staticFS, "static")
	staticServer := http.FileServer(http.FS(staticSub))
	cachedStatic := cacheControl("public, max-age=31536000, immutable", stripAssetHash(staticServer))
	for _, f := range []string{"/favicon.ico", "/icon.svg", "/icon-192.png", "/icon-512.png", "/apple-touch-icon.png", "/manifest.webmanifest"} {
		route("GET "+f, cachedStatic)
	}
	route("GET /assets/", cachedStatic)

	// Ensure fallback OG image exists (uploads to R2 in production)
	ensureLocalFallbackOG(a.Config)

	// Serve OG images from local disk (dev mode — production uses CDN)
	if a.Config.R2.CDNPublicURL == "" {
		routeFunc("GET /og/", handleOGImage())
	}

	routeFunc("GET /feed.xml", handleFeed(a))
	routeFunc("GET /robots.txt", handleRobotsTxt(a))
	sitemaps := &sitemapData{}
	routeFunc("GET /sitemap.xml", handleSitemapIndex(a, sitemaps))
	routeFunc("GET /sitemap-pages.xml", handleSitemapPages(a, sitemaps))
	// sitemap-packages routes are handled in appHandler (prefix can't be a ServeMux pattern)
	sitemapPackagesHandler := handleSitemapPackages(a, sitemaps)

	routeFunc("GET /{$}", handleIndex(a, tmpl))
	routeFunc("GET /packages-partial", handleIndexPartial(a, tmpl))
	routeFunc("GET /packages/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
	})
	routeFunc("GET /packages/{type}/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
	})
	routeFunc("GET /packages/{type}/{name}", handleDetail(a, tmpl))
	routeFunc("GET /wp-composer-vs-wpackagist", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/wp-packages-vs-wpackagist", http.StatusMovedPermanently)
	})
	routeFunc("GET /wp-packages-vs-wpackagist", handleCompare(a, tmpl))
	routeFunc("GET /docs", handleDocs(a, tmpl))
	routeFunc("GET /roots-wordpress", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/wordpress-core", http.StatusMovedPermanently)
	})
	routeFunc("GET /wordpress-core", handleWordpressCore(a, tmpl))
	routeFunc("GET /status", handleStatus(a, tmpl))
	routeFunc("GET /untagged", handleUntagged(a, tmpl))
	routeFunc("GET /untagged-partial", handleUntaggedPartial(a, tmpl))
	routeFunc("GET /untagged-authors", handleUntaggedAuthors(a))

	routeFunc("POST /downloads", handleDownloads(a))
	routeFunc("GET /metadata/changes.json", handleMetadataChanges(a))

	apiLimiter := newAPIRateLimiter()
	route("GET /api/stats", apiLimiter.RateLimit(http.HandlerFunc(handleAPIStats(a))))
	route("GET /api/stats/packages/{type}/{name}", apiLimiter.RateLimit(http.HandlerFunc(handleAPIMonthlyInstalls(a))))
	route("GET /api/stats/packages/{type}/{name}/total", apiLimiter.RateLimit(http.HandlerFunc(handleAPIPackageTotal(a))))
	route("GET /api/packages/{type}/closed", apiLimiter.RateLimit(http.HandlerFunc(handleAPIClosedPackages(a, false))))
	route("GET /api/packages/{type}/closed/permanent", apiLimiter.RateLimit(http.HandlerFunc(handleAPIClosedPackages(a, true))))

	// Serve Composer repository metadata from DB
	routeFunc("GET /packages.json", handlePackagesJSON(a))
	routeFunc("GET /p2/{vendor}/{file}", handleP2Package(a))

	// Admin subrouter — all admin handlers are behind routeMarker via StripPrefix
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /login", handleLoginPage(a))
	adminMux.HandleFunc("POST /login", handleLogin(a))
	adminMux.HandleFunc("POST /logout", handleLogout(a))

	protectedMux := http.NewServeMux()
	protectedMux.HandleFunc("GET /{$}", handleAdminLogs(tmpl))
	protectedMux.HandleFunc("GET /logs", handleAdminLogs(tmpl))
	protectedMux.HandleFunc("GET /logs/stream", handleAdminLogStream(a))
	protectedMux.HandleFunc("GET /wporg-check", handleAdminWporgCheck(a))
	adminMux.Handle("/", Chain(protectedMux, SessionAuth(a.DB), RequireAdmin))

	// Register both /admin and /admin/ so Go's ServeMux doesn't auto-redirect
	// the bare form to the trailing-slash form — Caddy strips trailing slashes
	// and would bounce it right back, creating a loop.
	adminHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/admin")
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		adminMux.ServeHTTP(w, r)
	})
	route("/admin", adminHandler)
	route("/admin/", adminHandler)

	// Build handler chain
	handler := appHandler(mux, tmpl, a, sitemapPackagesHandler)
	handler = withNegotiation(a, handler)
	handler = timeoutBypass(handler, 60*time.Second)
	handler = Recoverer(handler, a.Logger)
	handler = sentryMiddleware.Handle(handler)
	handler = RealIP(handler)

	return handler
}

// appHandler routes requests that ServeMux can't express (mid-segment prefixes)
// and renders a custom 404 template for unmatched routes. It preserves stdlib
// 405 behavior and does not interfere with handler-generated 404s.
//
// The approach: record the status the mux writes. If it's 404 and no registered
// handler touched the response (checked via a context flag set by routeMarker),
// replace the default body with the custom template. 405s and handler-generated
// 404s pass through untouched.
func appHandler(mux *http.ServeMux, tmpl *templateSet, a *app.App, sitemapPackages http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sitemap-packages prefix can't be expressed as a ServeMux pattern
		// (wildcards must be full path segments).
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sitemap-packages-") {
			sitemapPackages(w, r)
			return
		}

		rec := &statusRecorder{ResponseWriter: w}
		mux.ServeHTTP(rec, r)

		// Only replace the default ServeMux 404 — not handler-generated ones.
		// A registered handler sets the dispatched flag via routeMarker middleware,
		// so rec.dispatched is false only when the mux itself returned 404/405/etc.
		if rec.code == http.StatusNotFound && !rec.dispatched {
			w.WriteHeader(http.StatusNotFound)
			render(w, r, tmpl.notFound, "layout", map[string]any{"Gone": false, "CDNURL": a.Config.R2.CDNPublicURL})
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code       int
	dispatched bool // true if a registered handler wrote the response
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	if r.dispatched || code != http.StatusNotFound {
		r.ResponseWriter.WriteHeader(code)
	}
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.dispatched && r.code == http.StatusNotFound {
		return len(b), nil // swallow default 404 body
	}
	return r.ResponseWriter.Write(b)
}

// markDispatched sets the dispatched flag on the statusRecorder, indicating
// that a registered handler is handling this request (as opposed to the
// mux's internal 404/405 handler).
func (r *statusRecorder) markDispatched() {
	r.dispatched = true
}

// routeMarker wraps a handler to mark the response as dispatched by a
// registered route. This lets appHandler distinguish mux-generated 404s
// from handler-generated 404s.
func routeMarker(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec, ok := w.(*statusRecorder); ok {
			rec.markDispatched()
		}
		h.ServeHTTP(w, r)
	})
}

// Chain applies middleware in order (first argument is innermost).
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// Recoverer recovers from panics, logs the error, and returns 500.
func Recoverer(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				logger.Error("panic recovered",
					"error", fmt.Sprintf("%v", err),
					"stack", string(debug.Stack()),
				)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Paths that bypass the global http.TimeoutHandler because they are
// long-lived streaming connections (SSE). http.TimeoutHandler replaces the
// ResponseWriter with a buffering writer that doesn't implement
// http.Flusher, so these paths must be excluded before TimeoutHandler runs.
var noTimeoutPaths = map[string]bool{
	"/admin/logs/stream": true,
}

// timeoutBypass applies http.TimeoutHandler to all requests except those
// whose path is in noTimeoutPaths.
func timeoutBypass(next http.Handler, dt time.Duration) http.Handler {
	th := http.TimeoutHandler(next, dt, "")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noTimeoutPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		th.ServeHTTP(w, r)
	})
}

// RealIP sets r.RemoteAddr to the client IP from X-Forwarded-For or
// X-Real-IP headers. Only enable behind a trusted reverse proxy.
func RealIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := r.Header.Get("X-Real-IP"); ip != "" {
			r.RemoteAddr = ip
		} else if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// First entry is the original client
			if i := strings.IndexByte(xff, ','); i > 0 {
				ip = strings.TrimSpace(xff[:i])
			} else {
				ip = strings.TrimSpace(xff)
			}
			r.RemoteAddr = ip
		}
		next.ServeHTTP(w, r)
	})
}
