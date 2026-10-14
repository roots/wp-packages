package composer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/roots/wp-packages/internal/version"
)

// PackageFile represents a single R2-uploadable file for a package.
type PackageFile struct {
	Key  string // R2 object key, e.g. "p2/wp-plugin/akismet.json"
	Data []byte
}

// PackageFiles returns all Composer p2 files that a package produces.
// Plugins produce up to 2 files (tagged + dev), themes produce 1 (tagged only).
// Returns nil if the package has no serializable versions.
func PackageFiles(pkgType, name, versionsJSON string, meta PackageMeta) ([]PackageFile, error) {
	composerName := ComposerName(pkgType, name)
	var files []PackageFile

	// Tagged versions (always attempted)
	tagged, err := SerializePackage(pkgType, name, versionsJSON, meta)
	if err != nil {
		return nil, err
	}
	if tagged != nil {
		files = append(files, PackageFile{
			Key:  "p2/" + composerName + ".json",
			Data: tagged,
		})
	}

	// Dev versions (plugins only — SerializePackage returns nil for themes)
	dev, err := SerializePackage(pkgType, name+"~dev", versionsJSON, meta)
	if err != nil {
		return nil, err
	}
	if dev != nil {
		files = append(files, PackageFile{
			Key:  "p2/" + composerName + "~dev.json",
			Data: dev,
		})
	}

	return files, nil
}

// ObjectKeys returns all possible storage keys for a package,
// regardless of whether the files currently exist. Used for deletion.
func ObjectKeys(pkgType, name string) []string {
	composerName := ComposerName(pkgType, name)
	return []string{
		"p2/" + composerName + ".json",
		"p2/" + composerName + "~dev.json",
	}
}

// HashVersions computes a content hash over the normalized versions_json,
// trunk_revision, and published_times_json. trunk_revision and published times
// are included because they affect the serialized output (source.reference
// includes trunk@<rev>; published-time) even though they're not part of
// versions_json.
//
// The JSON inputs are already deterministic — json.Marshal sorts map keys, and
// NormalizeAndStoreVersions always produces the same output for the same input.
// So we hash the strings directly rather than round-tripping through parse/sort.
func HashVersions(versionsJSON string, trunkRevision *int64, publishedTimesJSON *string) string {
	h := sha256.New()
	h.Write([]byte(versionsJSON))
	if trunkRevision != nil {
		h.Write([]byte(strconv.FormatInt(*trunkRevision, 10)))
	}
	if publishedTimesJSON != nil {
		h.Write([]byte(*publishedTimesJSON))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ParsePublishedTimes decodes published_times_json. NULL or invalid JSON yields
// nil, so published-time is simply omitted rather than failing serialization.
// Keys are normalized like the versions they're looked up by, so rows stored
// before a normalization change still match; on a collision the latest time
// wins.
func ParsePublishedTimes(publishedTimesJSON *string) map[string]string {
	if publishedTimesJSON == nil {
		return nil
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(*publishedTimesJSON), &raw); err != nil {
		return nil
	}
	times := make(map[string]string, len(raw))
	for ver, t := range raw {
		ver = version.Normalize(ver)
		if ver == "" {
			continue
		}
		if t > times[ver] {
			times[ver] = t
		}
	}
	return times
}

// SerializePackage produces the Composer p2 JSON for a single package file.
//
// The name parameter determines which versions to include:
//   - "akismet"     → tagged versions (all non-dev-* versions)
//   - "akismet~dev" → dev versions only (dev-trunk)
//
// Plugins with zero tagged versions get dev-trunk in the main (non-~dev) file.
// Themes never produce dev versions.
//
// Returns nil with no error when there are no versions to serialize (e.g.
// theme ~dev request, or theme with no tagged versions).
func SerializePackage(pkgType, name string, versionsJSON string, meta PackageMeta) ([]byte, error) {
	isDev := strings.HasSuffix(name, "~dev")
	slug := strings.TrimSuffix(name, "~dev")

	// Themes never produce dev files
	if isDev && pkgType == "theme" {
		return nil, nil
	}

	var versions map[string]string
	if err := json.Unmarshal([]byte(versionsJSON), &versions); err != nil {
		return nil, fmt.Errorf("parsing versions_json for %s/%s: %w", pkgType, slug, err)
	}
	versions = version.NormalizeVersions(versions)

	composerName := ComposerName(pkgType, slug)

	var entries map[string]VersionEntry
	if isDev {
		entries = map[string]VersionEntry{
			"dev-trunk": ComposerVersion(pkgType, slug, "dev-trunk", "", meta),
		}
	} else {
		entries = make(map[string]VersionEntry)
		for ver, dlURL := range versions {
			if !strings.HasPrefix(ver, "dev-") {
				entries[ver] = ComposerVersion(pkgType, slug, ver, dlURL, meta)
			}
		}
		// Trunk-only plugins: put dev-trunk in the main file
		if len(entries) == 0 && pkgType == "plugin" {
			entries["dev-trunk"] = ComposerVersion(pkgType, slug, "dev-trunk", "", meta)
		}
	}

	if len(entries) == 0 {
		return nil, nil
	}

	payload := map[string]any{
		"packages": map[string]any{
			composerName: entries,
		},
	}
	return json.Marshal(payload)
}
