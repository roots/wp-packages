//go:build wporg_live

package integration

import (
	"encoding/json"
	"log/slog"
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
	"github.com/roots/wp-packages/internal/packages"
	"github.com/roots/wp-packages/internal/packagist"
	"github.com/roots/wp-packages/internal/repository"
	"github.com/roots/wp-packages/internal/testutil"
	"github.com/roots/wp-packages/internal/wporg"
)

// TestWpOrgLive tests the full pipeline against the real WordPress.org API.
// Run with: go test -tags=wporg_live -count=1 -timeout=5m ./internal/integration/...
func TestWpOrgLive(t *testing.T) {
	ctx := t.Context()
	db := testutil.OpenTestDB(t)

	cfg := config.DiscoveryConfig{
		APITimeoutS:  30,
		MaxRetries:   3,
		RetryDelayMs: 1000,
		Concurrency:  5,
	}
	client := wporg.NewClient(cfg, slog.Default())

	// Discover + update a small set of packages against real wp.org
	seeds := []struct {
		slug    string
		pkgType string
	}{
		{"akismet", "plugin"},
		{"classic-editor", "plugin"},
		{"astra", "theme"},
	}

	for _, s := range seeds {
		lastUpdated, err := client.FetchLastUpdated(ctx, s.pkgType, s.slug)
		if err != nil {
			t.Fatalf("discover %s: %v", s.slug, err)
		}
		if err := packages.UpsertShellPackage(ctx, db, s.pkgType, s.slug, lastUpdated); err != nil {
			t.Fatalf("upsert shell %s: %v", s.slug, err)
		}
	}

	syncRun, err := packages.AllocateSyncRunID(ctx, db)
	if err != nil {
		t.Fatalf("allocate sync run: %v", err)
	}

	pkgs, err := packages.GetPackagesNeedingUpdate(ctx, db, packages.UpdateQueryOpts{
		Force: true,
		Type:  "all",
	})
	if err != nil {
		t.Fatalf("get packages needing update: %v", err)
	}

	for _, p := range pkgs {
		var data map[string]any
		var fetchErr error
		if p.Type == "plugin" {
			data, fetchErr = client.FetchPlugin(ctx, p.Name)
		} else {
			data, fetchErr = client.FetchTheme(ctx, p.Name)
		}
		if fetchErr != nil {
			t.Fatalf("fetch %s/%s: %v", p.Type, p.Name, fetchErr)
		}

		pkg := packages.PackageFromAPIData(data, p.Type)
		pkg.ID = p.ID
		if _, _, err := pkg.NormalizeAndStoreVersions(); err != nil {
			t.Fatalf("normalize %s: %v", p.Name, err)
		}
		pkg.LastSyncRunID = &syncRun.RunID

		if err := packages.UpsertPackage(ctx, db, pkg); err != nil {
			t.Fatalf("upsert %s: %v", p.Name, err)
		}
	}

	if err := packages.FinishSyncRun(ctx, db, syncRun.RowID, "completed", nil); err != nil {
		t.Fatalf("finish sync run: %v", err)
	}

	// Build
	buildDir := t.TempDir()
	buildsDir := filepath.Join(buildDir, "builds")
	result, err := repository.Build(ctx, db, repository.BuildOpts{
		OutputDir: buildsDir,
		AppURL:    "http://test.local",
		Force:     true,
		Logger:    testLogger(t),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if result.PackagesTotal == 0 {
		t.Fatal("build produced zero packages")
	}

	// Symlink current
	if err := os.Symlink(filepath.Join("builds", result.BuildID), filepath.Join(buildDir, "current")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Serve repository
	actualBuildDir := filepath.Join(buildsDir, result.BuildID)
	repoServer := httptest.NewServer(http.FileServer(http.Dir(actualBuildDir)))
	defer repoServer.Close()

	// Start app server
	appCfg := &config.Config{
		AppURL: "http://test.local",
		Env:    "test",
	}
	a := &app.App{
		Config:    appCfg,
		DB:        db,
		Logger:    testLogger(t),
		Packagist: packagist.NewStubCache(),
	}
	router := apphttp.NewRouter(a)
	appSrv := httptest.NewServer(router)
	defer appSrv.Close()

	// Verify packages.json
	t.Run("packages.json", func(t *testing.T) {
		body := httpGet(t, repoServer.URL+"/packages.json")
		var pkgJSON map[string]any
		if err := json.Unmarshal([]byte(body), &pkgJSON); err != nil {
			t.Fatalf("invalid packages.json: %v", err)
		}
		if _, ok := pkgJSON["provider-includes"]; ok {
			t.Error("packages.json should not contain provider-includes")
		}
		if _, ok := pkgJSON["providers-url"]; ok {
			t.Error("packages.json should not contain providers-url")
		}
	})

	// Verify p2 endpoint
	t.Run("p2 endpoint", func(t *testing.T) {
		body := httpGet(t, repoServer.URL+"/p2/wp-plugin/akismet.json")
		var data map[string]any
		if err := json.Unmarshal([]byte(body), &data); err != nil {
			t.Fatalf("invalid p2 response: %v", err)
		}
		if _, ok := data["packages"]; !ok {
			t.Error("missing packages key")
		}
	})

	// Verify integrity
	t.Run("build integrity", func(t *testing.T) {
		errs := repository.ValidateIntegrity(actualBuildDir)
		if len(errs) > 0 {
			for _, e := range errs {
				t.Errorf("integrity error: %s", e)
			}
		}
	})

	// Composer install
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
		for _, pkg := range []string{"akismet", "classic-editor", "astra"} {
			if !strings.Contains(out, pkg) {
				t.Errorf("composer output missing %s", pkg)
			}
		}
	})
}
