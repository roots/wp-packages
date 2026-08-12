package composer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

// HashContent computes a content hash over the exact bytes a package serializes
// to, including the object keys those bytes are uploaded under.
//
// Hashing the serialized output rather than its inputs is what makes the sync
// step's diff query correct: "content_hash changed" and "the files on R2 are
// stale" become the same statement by construction. Hashing inputs instead
// (versions_json + trunk_revision, as this did originally) silently misses
// every field that reaches the output by another route — description, homepage,
// author, and last_committed all land in the serialized entry via PackageMeta,
// so a readme-only wp.org commit would leave R2 permanently stale.
//
// Keys are included so that a type change or rename also marks the package
// dirty. The 0x00 separators keep key/data boundaries unambiguous.
func HashContent(pkgType, name, versionsJSON string, meta PackageMeta) (string, error) {
	files, err := PackageFiles(pkgType, name, versionsJSON, meta)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.Key))
		h.Write([]byte{0})
		h.Write(f.Data)
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
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
