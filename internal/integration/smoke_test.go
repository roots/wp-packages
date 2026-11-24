//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roots/wp-packages/internal/app"
	"github.com/roots/wp-packages/internal/config"
	apphttp "github.com/roots/wp-packages/internal/http"
	"github.com/roots/wp-packages/internal/packagist"
	"github.com/roots/wp-packages/internal/repository"
	"github.com/roots/wp-packages/internal/testutil"
	"github.com/roots/wp-packages/internal/wporg"
)

func TestSmoke(t *testing.T) {
	// --- Setup ---
	fixtureDir := filepath.Join("..", "wporg", "testdata")
	mock := wporg.NewMockServer(fixtureDir)
	defer mock.Close()

	db := testutil.OpenTestDB(t)
	testutil.SeedFromFixtures(t, db, mock.URL)

	// --- Start app server (unstarted to get URL before build) ---
	cfg := &config.Config{
		Env: "test",
	}
	a := &app.App{
		Config:    cfg,
		DB:        db,
		Logger:    testLogger(t),
		Packagist: packagist.NewStubCache(),
	}
	router := apphttp.NewRouter(a)
	srv := httptest.NewUnstartedServer(router)
	srv.Start()
	defer srv.Close()

	// Set AppURL to the real app server so notify-batch points there
	cfg.AppURL = srv.URL

	// --- Build ---
	buildDir := t.TempDir()
	buildsDir := filepath.Join(buildDir, "builds")
	repoDir := buildDir

	result, err := repository.Build(t.Context(), db, repository.BuildOpts{
		OutputDir: buildsDir,
		AppURL:    srv.URL,
		Force:     true,
		Logger:    testLogger(t),
	})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if result.PackagesTotal == 0 {
		t.Fatal("build produced zero packages")
	}

	// Symlink current -> build
	currentLink := filepath.Join(repoDir, "current")
	if err := os.Symlink(filepath.Join("builds", result.BuildID), currentLink); err != nil {
		t.Fatalf("creating current symlink: %v", err)
	}

	// Serve repository files from the build directory
	actualBuildDir := filepath.Join(buildsDir, result.BuildID)
	repoServer := httptest.NewServer(http.FileServer(http.Dir(actualBuildDir)))
	defer repoServer.Close()

	// --- Composer metadata endpoints ---
	t.Run("packages.json", func(t *testing.T) {
		body := httpGet(t, repoServer.URL+"/packages.json")
		var pkgJSON map[string]any
		if err := json.Unmarshal([]byte(body), &pkgJSON); err != nil {
			t.Fatalf("invalid packages.json: %v", err)
		}

		// notify-batch URL should point to the app server
		if nb, ok := pkgJSON["notify-batch"].(string); !ok || nb != srv.URL+"/downloads" {
			t.Errorf("notify-batch = %q, want %q", nb, srv.URL+"/downloads")
		}

		// metadata-url
		if mu, ok := pkgJSON["metadata-url"].(string); !ok || mu != "/p2/%package%.json" {
			t.Errorf("unexpected metadata-url: %v", pkgJSON["metadata-url"])
		}

		// metadata-changes-url
		if mcu, ok := pkgJSON["metadata-changes-url"].(string); !ok || mcu != srv.URL+"/metadata/changes.json" {
			t.Errorf("metadata-changes-url = %q, want %q", mcu, srv.URL+"/metadata/changes.json")
		}

		// providers-url and provider-includes should NOT exist (v1 removed)
		if _, ok := pkgJSON["providers-url"]; ok {
			t.Error("packages.json should not contain providers-url")
		}
		if _, ok := pkgJSON["provider-includes"]; ok {
			t.Error("packages.json should not contain provider-includes")
		}
	})

	t.Run("p2 endpoint", func(t *testing.T) {
		body := httpGet(t, repoServer.URL+"/p2/wp-plugin/akismet.json")
		var data map[string]any
		if err := json.Unmarshal([]byte(body), &data); err != nil {
			t.Fatalf("invalid p2 response: %v", err)
		}

		pkgs, ok := data["packages"].(map[string]any)
		if !ok {
			t.Fatal("missing 'packages' key in p2 response")
		}

		akismet, ok := pkgs["wp-plugin/akismet"].([]any)
		if !ok {
			t.Fatalf("wp-plugin/akismet should be a list of versions, got %T", pkgs["wp-plugin/akismet"])
		}

		// Check at least one version entry
		if len(akismet) == 0 {
			t.Fatal("no version entries for akismet")
		}

		// Verify every version entry's structure
		for i, entry := range akismet {
			e, ok := entry.(map[string]any)
			if !ok {
				t.Fatalf("entry %d is not an object", i)
			}
			ver := e["version"]

			// Required fields
			for _, field := range []string{"name", "version", "type", "dist", "source", "require"} {
				if _, ok := e[field]; !ok {
					t.Errorf("version %v missing field: %s", ver, field)
				}
			}

			// dist URL should point to downloads.wordpress.org
			if dist, ok := e["dist"].(map[string]any); ok {
				if url, ok := dist["url"].(string); ok {
					if !strings.Contains(url, "downloads.wordpress.org") {
						t.Errorf("version %v dist URL unexpected: %s", ver, url)
					}
				}
			}

			// type should be wordpress-plugin
			if typ, ok := e["type"].(string); ok {
				if typ != "wordpress-plugin" {
					t.Errorf("version %v type: got %s, want wordpress-plugin", ver, typ)
				}
			}
		}
	})

	t.Run("p directory should not exist", func(t *testing.T) {
		resp, err := http.Get(repoServer.URL + "/p/wp-plugin/akismet.json")
		if err != nil {
			t.Fatalf("GET p/ endpoint: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == 200 {
			t.Error("p/ directory should not exist after dropping Composer v1 support")
		}
	})

	t.Run("package detail page", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/packages/wp-plugin/akismet")
		if err != nil {
			t.Fatalf("GET package detail: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			t.Errorf("package detail status: got %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "composer require") {
			t.Error("package detail page missing 'composer require'")
		}
	})

	t.Run("package detail 404", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/packages/wp-plugin/nonexistent")
		if err != nil {
			t.Fatalf("GET nonexistent package: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 404 {
			t.Errorf("nonexistent package status: got %d, want 404", resp.StatusCode)
		}
	})

	// --- Composer install ---
	t.Run("composer install", func(t *testing.T) {
		composerPath, err := exec.LookPath("composer")
		if err != nil {
			t.Skip("composer not in PATH")
		}

		dir := t.TempDir()
		writeComposerJSON(t, dir, repoServer.URL, map[string]string{
			"wp-plugin/akismet":        "*",
			"wp-plugin/classic-editor": "*",
			"wp-theme/astra":           "*",
		})
		out := runComposer(t, composerPath, dir, "install", "--no-interaction", "--no-progress")
		for _, pkg := range []string{"wp-plugin/akismet", "wp-plugin/classic-editor", "wp-theme/astra"} {
			if !strings.Contains(out, pkg) {
				t.Errorf("composer install output missing %s", pkg)
			}
		}
	})

	t.Run("install events recorded", func(t *testing.T) {
		if _, err := exec.LookPath("composer"); err != nil {
			t.Skip("composer not in PATH — install events require composer install to run first")
		}
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM install_events").Scan(&count); err != nil {
			t.Fatalf("querying install_events: %v", err)
		}
		if count == 0 {
			t.Error("no install events recorded — notify-batch may not be reaching the app server")
		}
	})

	t.Run("composer version pinning", func(t *testing.T) {
		composerPath, err := exec.LookPath("composer")
		if err != nil {
			t.Skip("composer not in PATH")
		}

		dir := t.TempDir()
		writeComposerJSON(t, dir, repoServer.URL, map[string]string{
			"wp-plugin/akismet": "5.3.3",
		})
		out := runComposer(t, composerPath, dir, "install", "--no-interaction", "--no-progress")
		if !strings.Contains(out, "5.3.3") {
			t.Errorf("composer install did not lock pinned version 5.3.3, output: %s", out)
		}
	})

	// --- Build integrity ---
	t.Run("build integrity", func(t *testing.T) {
		manifestPath := filepath.Join(actualBuildDir, "manifest.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatalf("reading manifest: %v", err)
		}
		var manifest map[string]any
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatalf("invalid manifest: %v", err)
		}
		if _, ok := manifest["root_hash"]; !ok {
			t.Error("manifest missing root_hash")
		}
		if count, ok := manifest["artifact_count"].(float64); !ok || count == 0 {
			t.Error("manifest missing or zero artifact_count")
		}

		// Run full integrity validation
		errs := repository.ValidateIntegrity(actualBuildDir)
		if len(errs) > 0 {
			for _, e := range errs {
				t.Errorf("integrity error: %s", e)
			}
		}
	})
}
