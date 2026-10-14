package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roots/wp-packages/internal/app"
	"github.com/roots/wp-packages/internal/config"
	"github.com/roots/wp-packages/internal/db"
)

func setupComposerTestApp(t *testing.T) *app.App {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	_, _ = database.Exec(`
		CREATE TABLE packages (
			id INTEGER PRIMARY KEY,
			type TEXT NOT NULL CHECK(type IN ('plugin','theme')),
			name TEXT NOT NULL,
			description TEXT, homepage TEXT, author TEXT,
			versions_json TEXT NOT NULL DEFAULT '{}',
			is_active INTEGER NOT NULL DEFAULT 1,
			last_committed TEXT,
			trunk_revision INTEGER,
			published_times_json TEXT,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			UNIQUE(type, name)
		)
	`)

	t.Cleanup(func() { _ = database.Close() })

	return &app.App{
		Config: &config.Config{},
		DB:     database,
		Logger: slog.Default(),
	}
}

func seedPackage(t *testing.T, a *app.App, pkgType, name, versionsJSON string) {
	t.Helper()
	_, err := a.DB.Exec(`INSERT INTO packages (type, name, versions_json, is_active, created_at, updated_at)
		VALUES (?, ?, ?, 1, datetime('now'), datetime('now'))`, pkgType, name, versionsJSON)
	if err != nil {
		t.Fatal(err)
	}
}

// serveP2 routes a request through a mux so PathValue works with {vendor}/{file} wildcards.
func serveP2(handler http.HandlerFunc, path string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /p2/{vendor}/{file}", handler)
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestPackagesJSON(t *testing.T) {
	a := setupComposerTestApp(t)
	handler := handlePackagesJSON(a)

	req := httptest.NewRequest("GET", "/packages.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body["metadata-url"] != "/p2/%package%.json" {
		t.Errorf("metadata-url = %v", body["metadata-url"])
	}
	if body["notify-batch"] != "/downloads" {
		t.Errorf("notify-batch = %v", body["notify-batch"])
	}
	patterns, ok := body["available-package-patterns"].([]any)
	if !ok || len(patterns) != 2 {
		t.Errorf("available-package-patterns = %v", body["available-package-patterns"])
	}
}

func TestPackagesJSON_WithAppURL(t *testing.T) {
	a := setupComposerTestApp(t)
	a.Config.AppURL = "https://wp-packages.example.com"
	handler := handlePackagesJSON(a)

	req := httptest.NewRequest("GET", "/packages.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body["notify-batch"] != "https://wp-packages.example.com/downloads" {
		t.Errorf("notify-batch = %v", body["notify-batch"])
	}
	if body["metadata-changes-url"] != "https://wp-packages.example.com/metadata/changes.json" {
		t.Errorf("metadata-changes-url = %v", body["metadata-changes-url"])
	}
}

func TestP2Package_TaggedVersions(t *testing.T) {
	a := setupComposerTestApp(t)
	seedPackage(t, a, "plugin", "akismet", `{"5.3.7":"https://downloads.wordpress.org/plugin/akismet.5.3.7.zip"}`)

	w := serveP2(handleP2Package(a), "/p2/wp-plugin/akismet.json")

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	packages := body["packages"].(map[string]any)
	if _, ok := packages["wp-plugin/akismet"]; !ok {
		t.Error("missing wp-plugin/akismet in packages")
	}
}

func TestP2Package_PublishedTime(t *testing.T) {
	a := setupComposerTestApp(t)
	_, err := a.DB.Exec(`INSERT INTO packages (type, name, versions_json, published_times_json, is_active, created_at, updated_at)
		VALUES ('plugin', 'akismet', ?, ?, 1, datetime('now'), datetime('now'))`,
		`{"5.3.7":"https://downloads.wordpress.org/plugin/akismet.5.3.7.zip"}`,
		`{"5.3.7":"2026-09-01T00:00:00Z"}`)
	if err != nil {
		t.Fatal(err)
	}

	w := serveP2(handleP2Package(a), "/p2/wp-plugin/akismet.json")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var body struct {
		Packages map[string]map[string]map[string]any `json:"packages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := body.Packages["wp-plugin/akismet"]["5.3.7"]["published-time"]; got != "2026-09-01T00:00:00Z" {
		t.Errorf("published-time = %v, want 2026-09-01T00:00:00Z", got)
	}
}

func TestP2Package_DevFile(t *testing.T) {
	a := setupComposerTestApp(t)
	seedPackage(t, a, "plugin", "akismet", `{"5.3.7":"https://downloads.wordpress.org/plugin/akismet.5.3.7.zip"}`)

	w := serveP2(handleP2Package(a), "/p2/wp-plugin/akismet~dev.json")

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	packages := body["packages"].(map[string]any)
	akismet := packages["wp-plugin/akismet"].(map[string]any)
	devTrunk := akismet["dev-trunk"].(map[string]any)

	// Dev versions should not have dist
	if _, ok := devTrunk["dist"]; ok {
		t.Error("dev-trunk should not have dist")
	}
}

func TestP2Package_ThemeNoDevFile(t *testing.T) {
	a := setupComposerTestApp(t)
	seedPackage(t, a, "theme", "astra", `{"3.0.0":"https://downloads.wordpress.org/theme/astra.3.0.0.zip"}`)

	w := serveP2(handleP2Package(a), "/p2/wp-theme/astra~dev.json")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for theme ~dev.json", w.Code)
	}
}

func TestP2Package_NotFound(t *testing.T) {
	a := setupComposerTestApp(t)

	w := serveP2(handleP2Package(a), "/p2/wp-plugin/nonexistent.json")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestP2Package_InvalidVendor(t *testing.T) {
	a := setupComposerTestApp(t)

	w := serveP2(handleP2Package(a), "/p2/wp-foo/bar.json")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestP2Package_InactivePackage(t *testing.T) {
	a := setupComposerTestApp(t)
	seedPackage(t, a, "plugin", "inactive-plugin", `{"1.0.0":"https://example.com/test.zip"}`)
	_, _ = a.DB.Exec(`UPDATE packages SET is_active = 0 WHERE name = 'inactive-plugin'`)

	w := serveP2(handleP2Package(a), "/p2/wp-plugin/inactive-plugin.json")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for inactive package", w.Code)
	}
}
