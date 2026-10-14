package composer

import (
	"fmt"
	"hash/crc32"
	"strings"
)

// PackageMeta holds optional metadata for Composer version entries.
type PackageMeta struct {
	Description   string
	Homepage      string
	Author        string
	RequiresPHP   string
	LastUpdated   string
	TrunkRevision *int64
	// PublishedTimes maps tagged versions to when WP Packages first published
	// them (RFC3339). Emitted as Composer's published-time, which the cooldown
	// policy relies on.
	PublishedTimes map[string]string
}

// VersionEntry is a single Composer version entry (e.g. one version of a package).
type VersionEntry = map[string]any

// ComposerVersion builds a single Composer version entry for a package.
func ComposerVersion(pkgType, slug, ver, downloadURL string, meta PackageMeta) VersionEntry {
	composerName := ComposerName(pkgType, slug)
	composerType := "wordpress-plugin"
	if pkgType == "theme" {
		composerType = "wordpress-theme"
	}

	svnBase := fmt.Sprintf("https://plugins.svn.wordpress.org/%s", slug)
	supportIssues := fmt.Sprintf("https://wordpress.org/support/plugin/%s", slug)
	supportChangelog := fmt.Sprintf("https://wordpress.org/plugins/%s/#developers", slug)
	if pkgType == "theme" {
		svnBase = fmt.Sprintf("https://themes.svn.wordpress.org/%s", slug)
		supportIssues = fmt.Sprintf("https://wordpress.org/support/theme/%s", slug)
		supportChangelog = fmt.Sprintf("https://wordpress.org/themes/%s/#developers", slug)
	}

	ref := fmt.Sprintf("tags/%s", ver)
	if pkgType == "theme" {
		ref = ver
	}
	if ver == "dev-trunk" {
		ref = "trunk"
		if meta.TrunkRevision != nil {
			ref = fmt.Sprintf("trunk@%d", *meta.TrunkRevision)
		}
	}

	entry := map[string]any{
		"name":    composerName,
		"version": ver,
		"type":    composerType,
		"source": map[string]any{
			"type":      "svn",
			"url":       svnBase + "/",
			"reference": ref,
		},
		"require": map[string]any{
			"composer/installers": "~1.0|~2.0",
		},
		"support": map[string]any{
			"source":    svnBase,
			"issues":    supportIssues,
			"changelog": supportChangelog,
		},
		"uid": crc32.ChecksumIEEE([]byte(fmt.Sprintf("%s/%s", composerName, ver))),
	}

	if ver != "dev-trunk" && downloadURL != "" {
		entry["dist"] = map[string]any{
			"type": "zip",
			"url":  downloadURL,
		}
	}

	if meta.Description != "" {
		entry["description"] = meta.Description
	}
	if meta.Homepage != "" {
		entry["homepage"] = meta.Homepage
	}
	if meta.Author != "" {
		entry["authors"] = []map[string]any{{"name": meta.Author}}
	}
	if meta.RequiresPHP != "" {
		req := entry["require"].(map[string]any)
		req["php"] = ">=" + meta.RequiresPHP
	}
	if meta.LastUpdated != "" {
		entry["time"] = meta.LastUpdated
	}
	if ver == "dev-trunk" {
		if meta.LastUpdated != "" {
			entry["published-time"] = meta.LastUpdated
		}
	} else if published := meta.PublishedTimes[ver]; published != "" {
		entry["time"] = published
		entry["published-time"] = published
	}

	return entry
}

// ComposerName returns the Composer package name for a WordPress package.
func ComposerName(pkgType, slug string) string {
	if pkgType == "theme" {
		return "wp-theme/" + slug
	}
	return "wp-plugin/" + slug
}

// DownloadURL returns the WordPress.org download URL for a specific version.
func DownloadURL(pkgType, slug, version string) string {
	if pkgType == "theme" {
		return fmt.Sprintf("https://downloads.wordpress.org/theme/%s.%s.zip", slug, version)
	}
	return fmt.Sprintf("https://downloads.wordpress.org/plugin/%s.%s.zip", slug, version)
}

// PackageType maps a Composer vendor prefix to a DB package type.
// "wp-plugin" → "plugin", "wp-theme" → "theme". Returns "" for unknown vendors.
func PackageType(vendor string) string {
	switch vendor {
	case "wp-plugin":
		return "plugin"
	case "wp-theme":
		return "theme"
	default:
		return ""
	}
}

// VendorFromComposerName extracts the path portion for filesystem layout.
// "wp-plugin/akismet" → "wp-plugin/akismet"
func VendorFromComposerName(name string) string {
	return name
}

// SlugFromComposerName extracts just the slug: "wp-plugin/akismet" → "akismet"
func SlugFromComposerName(name string) string {
	parts := strings.SplitN(name, "/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return name
}
