package http

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/roots/wp-packages/internal/app"
	"github.com/roots/wp-packages/internal/auth"
	"github.com/roots/wp-packages/internal/config"
	"github.com/roots/wp-packages/internal/db"
	"github.com/roots/wp-packages/internal/packagist"
)

func newTestApp(t *testing.T) *app.App {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	return &app.App{
		Config:    &config.Config{},
		DB:        database,
		Logger:    slog.Default(),
		Packagist: packagist.NewDownloadsCache(slog.Default()),
	}
}

// TestNewRouter_NoPanic verifies that all ServeMux patterns are valid and
// route registration does not panic.
func TestNewRouter_NoPanic(t *testing.T) {
	a := newTestApp(t)
	handler := NewRouter(a)
	if handler == nil {
		t.Fatal("NewRouter returned nil")
	}
}

func TestRouter_MethodNotAllowed(t *testing.T) {
	a := newTestApp(t)
	handler := NewRouter(a)

	// POST /health should return 405, not 404
	req := httptest.NewRequest("POST", "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /health: got %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow == "" {
		t.Error("POST /health: missing Allow header")
	}
}

func TestRouter_NotFound(t *testing.T) {
	a := newTestApp(t)
	handler := NewRouter(a)

	req := httptest.NewRequest("GET", "/nonexistent", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("GET /nonexistent: got %d, want 404", w.Code)
	}
}

func TestRouter_HandlerGenerated404PreservesBody(t *testing.T) {
	// A registered handler that returns 404 with its own body should not
	// have that body replaced by the custom not-found template.
	mux := http.NewServeMux()
	mux.Handle("GET /pkg/{name}", routeMarker(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "package not found", http.StatusNotFound)
	})))

	a := newTestApp(t)
	tmpl := loadTemplates("")
	handler := appHandler(mux, tmpl, a, nil)

	req := httptest.NewRequest("GET", "/pkg/nope", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /pkg/nope: got %d, want 404", w.Code)
	}
	body := w.Body.String()
	if body != "package not found\n" {
		t.Errorf("handler-generated 404 body was replaced: got %q", body)
	}
}

func TestRouter_UnmatchedRouteRendersTemplate(t *testing.T) {
	// An unmatched route should render the custom 404 template, not the
	// default "404 page not found" text from ServeMux.
	mux := http.NewServeMux()
	mux.Handle("GET /health", routeMarker(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})))

	a := newTestApp(t)
	tmpl := loadTemplates("")
	handler := appHandler(mux, tmpl, a, nil)

	req := httptest.NewRequest("GET", "/nonexistent", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /nonexistent: got %d, want 404", w.Code)
	}
	body := w.Body.String()
	if body == "404 page not found\n" {
		t.Error("unmatched route got default ServeMux 404 body instead of custom template")
	}
}

func TestTimeoutBypass_AppliesTimeoutToNormalPaths(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	})
	handler := timeoutBypass(slow, 50*time.Millisecond)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /health (slow): got %d, want 503 (timeout)", w.Code)
	}
}

func TestTimeoutBypass_SkipsTimeoutForStreamingPaths(t *testing.T) {
	var mu sync.Mutex
	var flusherAvailable bool

	handler := timeoutBypass(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := w.(http.Flusher)
		mu.Lock()
		flusherAvailable = ok
		mu.Unlock()
		_, _ = w.Write([]byte("streaming"))
	}), 50*time.Millisecond)

	req := httptest.NewRequest("GET", "/admin/logs/stream", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /admin/logs/stream: got %d, want 200", w.Code)
	}
	mu.Lock()
	if !flusherAvailable {
		t.Error("GET /admin/logs/stream: http.Flusher not available (timeout handler was not bypassed)")
	}
	mu.Unlock()
}

func TestStatusRecorder_ForwardsFlusher(t *testing.T) {
	inner := httptest.NewRecorder() // implements http.Flusher
	rec := &statusRecorder{ResponseWriter: inner, dispatched: true}

	// Assert via interface variable to test dynamic dispatch
	var w http.ResponseWriter = rec
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("statusRecorder does not implement http.Flusher")
	}
	// Should not panic
	flusher.Flush()
}

func TestRouter_LegacyRedirect_MarkdownClient(t *testing.T) {
	// MD-only clients hitting a legacy redirect path should get a 301
	// to the canonical Markdown URL, not a 406.
	a := newTestApp(t)
	handler := NewRouter(a)

	cases := []struct {
		path     string
		location string
	}{
		{"/wp-composer-vs-wpackagist", "/wp-packages-vs-wpackagist.md"},
		{"/roots-wordpress", "/wordpress-core.md"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.path, nil)
		req.Header.Set("Accept", "text/markdown")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusMovedPermanently {
			t.Errorf("%s: status %d, want 301", c.path, w.Code)
		}
		if loc := w.Header().Get("Location"); loc != c.location {
			t.Errorf("%s: Location %q, want %q", c.path, loc, c.location)
		}
	}
}

func TestRouter_LegacyRedirect_DotMd(t *testing.T) {
	// /wp-composer-vs-wpackagist.md should also redirect, since the
	// middleware strips .md and dispatches into the MD mux.
	a := newTestApp(t)
	handler := NewRouter(a)

	req := httptest.NewRequest("GET", "/wp-composer-vs-wpackagist.md", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMovedPermanently {
		t.Errorf("status %d, want 301", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/wp-packages-vs-wpackagist.md" {
		t.Errorf("Location %q, want /wp-packages-vs-wpackagist.md", loc)
	}
}

func TestRouter_SitemapPackagesRoutes(t *testing.T) {
	a := newTestApp(t)
	handler := NewRouter(a)

	// Should not 404 — the route should be matched even though
	// it can't be a ServeMux pattern.
	req := httptest.NewRequest("GET", "/sitemap-packages-0.xml", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// 200 (if sitemap data generates) or 500 (no DB tables) are both acceptable;
	// 404 means the route wasn't matched.
	if w.Code == http.StatusNotFound {
		t.Error("GET /sitemap-packages-0.xml returned 404 — route not matched")
	}
}

// newTestAppWithAuth creates a test app with users/sessions tables and an
// admin user+session. Returns the app and a valid session cookie value.
func newTestAppWithAuth(t *testing.T) (*app.App, string) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	_, err = database.Exec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	user, err := auth.CreateUser(ctx, database, "test@example.com", "Test", "hash", true)
	if err != nil {
		t.Fatal(err)
	}
	session, err := auth.CreateSession(ctx, database, user.ID, 60)
	if err != nil {
		t.Fatal(err)
	}

	a := &app.App{
		Config:    &config.Config{Session: config.SessionConfig{LifetimeMinutes: 60}},
		DB:        database,
		Logger:    slog.Default(),
		Packagist: packagist.NewDownloadsCache(slog.Default()),
	}
	return a, session
}

// TestRouter_LogStreamBypassesTimeout is an integration test that verifies
// /admin/logs/stream through the full NewRouter handler chain (RealIP →
// Sentry → Recoverer → timeoutBypass → appHandler → mux → StripPrefix →
// SessionAuth → RequireAdmin → handler) gets SSE headers and isn't cut off
// by http.TimeoutHandler.
func TestRouter_LogStreamBypassesTimeout(t *testing.T) {
	a, session := newTestAppWithAuth(t)
	handler := NewRouter(a)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := srv.Client()
	client.Timeout = 2 * time.Second

	httpReq, _ := http.NewRequest("GET", srv.URL+"/admin/logs/stream?file=wppackages", nil)
	httpReq.AddCookie(&http.Cookie{Name: "session", Value: session})

	resp, err := client.Do(httpReq)
	if err != nil {
		// Client timeout is expected since the stream never ends and the log
		// file doesn't exist. But we should get a response before that.
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/logs/stream: got %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type: got %q, want text/event-stream", ct)
	}
}
