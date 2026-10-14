package http

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/roots/wp-packages/internal/app"
	"github.com/roots/wp-packages/internal/config"
	"github.com/roots/wp-packages/internal/db"
)

func setupTestApp(t *testing.T) *app.App {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	_, _ = database.Exec(`
		CREATE TABLE packages (
			id INTEGER PRIMARY KEY,
			type TEXT NOT NULL CHECK(type IN ('plugin','theme')),
			name TEXT NOT NULL, display_name TEXT, description TEXT, author TEXT,
			homepage TEXT, slug_url TEXT,
			versions_json TEXT NOT NULL DEFAULT '{}',
			downloads INTEGER NOT NULL DEFAULT 0, active_installs INTEGER NOT NULL DEFAULT 0,
			current_version TEXT, rating REAL, num_ratings INTEGER NOT NULL DEFAULT 0,
			is_active INTEGER NOT NULL DEFAULT 1,
			last_committed TEXT, last_synced_at TEXT, last_sync_run_id INTEGER,
			wp_packages_installs_total INTEGER NOT NULL DEFAULT 0,
			wp_packages_installs_30d INTEGER NOT NULL DEFAULT 0,
			last_installed_at TEXT,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			UNIQUE(type, name)
		);
		CREATE TABLE install_events (
			id INTEGER PRIMARY KEY,
			package_id INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
			version TEXT NOT NULL,
			ip_hash TEXT NOT NULL, user_agent_hash TEXT NOT NULL,
			dedupe_bucket INTEGER NOT NULL, dedupe_hash TEXT NOT NULL,
			created_at TEXT NOT NULL,
			UNIQUE(dedupe_hash, dedupe_bucket)
		);
	`)
	_, _ = database.Exec(`INSERT INTO packages (type, name, versions_json, is_active, created_at, updated_at)
		VALUES ('plugin', 'akismet', '{}', 1, datetime('now'), datetime('now'))`)

	t.Cleanup(func() { _ = database.Close() })

	return &app.App{
		Config: &config.Config{
			Telemetry: config.TelemetryConfig{DedupeWindowSeconds: 3600},
		},
		DB:     database,
		Logger: slog.Default(),
	}
}

func TestDownloads_Always200(t *testing.T) {
	a := setupTestApp(t)
	handler := handleDownloads(a)

	// Valid request
	body := `{"downloads":[{"name":"wp-plugin/akismet","version":"5.0"}]}`
	req := httptest.NewRequest("POST", "/downloads", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("valid request: status = %d, want 200", w.Code)
	}
}

func TestDownloads_MalformedPayload(t *testing.T) {
	a := setupTestApp(t)
	handler := handleDownloads(a)

	req := httptest.NewRequest("POST", "/downloads", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("malformed payload: status = %d, want 200", w.Code)
	}
}

func TestDownloads_EmptyBody(t *testing.T) {
	a := setupTestApp(t)
	handler := handleDownloads(a)

	req := httptest.NewRequest("POST", "/downloads", strings.NewReader(""))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("empty body: status = %d, want 200", w.Code)
	}
}

func TestDownloads_UnknownPackage(t *testing.T) {
	a := setupTestApp(t)
	handler := handleDownloads(a)

	body := `{"downloads":[{"name":"wp-plugin/nonexistent","version":"1.0"}]}`
	req := httptest.NewRequest("POST", "/downloads", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("unknown package: status = %d, want 200", w.Code)
	}

	// No events should be recorded
	var count int
	_ = a.DB.QueryRow("SELECT COUNT(*) FROM install_events").Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 events for unknown package, got %d", count)
	}
}

func TestDownloads_BatchCap(t *testing.T) {
	a := setupTestApp(t)
	handler := handleDownloads(a)

	// Build 150-item batch with distinct versions so each produces a unique event
	var items []string
	for i := range 150 {
		items = append(items, fmt.Sprintf(`{"name":"wp-plugin/akismet","version":"%d.0"}`, i))
	}
	body := `{"downloads":[` + strings.Join(items, ",") + `]}`

	req := httptest.NewRequest("POST", "/downloads", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("batch cap: status = %d, want 200", w.Code)
	}

	// With distinct versions, each item is a unique event.
	// Cap should limit processing to 100 items.
	var count int
	_ = a.DB.QueryRow("SELECT COUNT(*) FROM install_events").Scan(&count)
	if count != 100 {
		t.Errorf("expected exactly 100 events (batch capped from 150), got %d", count)
	}
}

func TestDownloads_DeduplicatesWithinWindow(t *testing.T) {
	a := setupTestApp(t)
	handler := handleDownloads(a)

	body := `{"downloads":[{"name":"wp-plugin/akismet","version":"5.0"}]}`

	// First request
	req := httptest.NewRequest("POST", "/downloads", strings.NewReader(body))
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("User-Agent", "Composer/2.0")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Second request from same IP (different port)
	req = httptest.NewRequest("POST", "/downloads", strings.NewReader(body))
	req.RemoteAddr = "10.0.0.1:54321"
	req.Header.Set("User-Agent", "Composer/2.0")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var count int
	_ = a.DB.QueryRow("SELECT COUNT(*) FROM install_events").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 event (deduplicated across ports), got %d", count)
	}
}

func TestDownloads_RequestBodyTooLarge(t *testing.T) {
	a := setupTestApp(t)
	handler := handleDownloads(a)

	largeVersion := strings.Repeat("1", maxDownloadsRequestBodyBytes+100)
	body := `{"downloads":[{"name":"wp-plugin/akismet","version":"` + largeVersion + `"}]}`

	req := httptest.NewRequest("POST", "/downloads", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("oversized payload: status = %d, want 200", w.Code)
	}

	var count int
	_ = a.DB.QueryRow("SELECT COUNT(*) FROM install_events").Scan(&count)
	if count != 0 {
		t.Fatalf("expected 0 events for oversized payload, got %d", count)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		remoteAddr string
		want       string
	}{
		{"192.168.1.1:12345", "192.168.1.1"},
		{"10.0.0.1:80", "10.0.0.1"},
		{"[::1]:8080", "::1"},
		{"192.168.1.1", "192.168.1.1"}, // no port
	}
	for _, tt := range tests {
		t.Run(tt.remoteAddr, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tt.remoteAddr}
			got := clientIP(r)
			if got != tt.want {
				t.Errorf("clientIP(%q) = %q, want %q", tt.remoteAddr, got, tt.want)
			}
		})
	}
}
