//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/roots/wp-packages/internal/composer"
	"github.com/roots/wp-packages/internal/packages"
	"github.com/roots/wp-packages/internal/repository"
	"github.com/roots/wp-packages/internal/testutil"
	"github.com/roots/wp-packages/internal/wporg"
)

// TestSerializerParity asserts that composer.PackageFiles() produces byte-identical
// output to what repository.Build() writes to disk, for every seeded package.
//
// This is the safety net for the Phase 3 cutover. The old deploy path decides what
// to upload by byte-comparing built files; the new one decides from content_hash.
// Those two are only equivalent if the serializers agree — so verify that here, in
// CI, rather than discovering a divergence on R2.
//
// Delete this test along with internal/repository/ once the cutover is complete.
func TestSerializerParity(t *testing.T) {
	ctx := context.Background()

	fixtureDir := filepath.Join("..", "wporg", "testdata")
	mock := wporg.NewMockServer(fixtureDir)
	defer mock.Close()

	db := testutil.OpenTestDB(t)
	testutil.SeedFromFixtures(t, db, mock.URL)

	buildOutputDir := filepath.Join(t.TempDir(), "builds")
	result, err := repository.Build(ctx, db, repository.BuildOpts{
		OutputDir: buildOutputDir,
		AppURL:    "http://test.local",
		Force:     true,
		Logger:    testLogger(t),
	})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	buildDir := filepath.Join(buildOutputDir, result.BuildID)

	pkgs, err := packages.GetAllActiveForHashing(ctx, db)
	if err != nil {
		t.Fatalf("loading packages: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("no seeded packages to compare")
	}

	compared := 0
	for _, p := range pkgs {
		files, err := composer.PackageFiles(p.Type, p.Name, p.VersionsJSON, p.ComposerMeta())
		if err != nil {
			t.Fatalf("serializing %s/%s: %v", p.Type, p.Name, err)
		}

		for _, f := range files {
			built, err := os.ReadFile(filepath.Join(buildDir, filepath.FromSlash(f.Key)))
			if err != nil {
				t.Errorf("%s/%s: builder produced no file at %s (serializer did): %v",
					p.Type, p.Name, f.Key, err)
				continue
			}
			if string(built) != string(f.Data) {
				t.Errorf("%s/%s: serializer output differs from builder output at %s\n built: %s\n  sync: %s",
					p.Type, p.Name, f.Key, truncate(built), truncate(f.Data))
			}
			compared++
		}

		// The reverse direction: any file the builder wrote that the serializer
		// would not produce becomes an orphan on R2 after cutover.
		for _, key := range composer.ObjectKeys(p.Type, p.Name) {
			if _, err := os.Stat(filepath.Join(buildDir, filepath.FromSlash(key))); err != nil {
				continue // builder didn't write it either — fine
			}
			found := false
			for _, f := range files {
				if f.Key == key {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s/%s: builder wrote %s but serializer would not produce it",
					p.Type, p.Name, key)
			}
		}
	}

	t.Logf("compared %d files across %d packages", compared, len(pkgs))
}

func truncate(b []byte) string {
	const max = 300
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}
