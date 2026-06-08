package version

import (
	"cmp"
	"regexp"
	"strconv"
	"strings"
)

// validVersion matches WordPress version strings: digits separated by dots (1-4 parts),
// optionally followed by a Composer-compatible pre-release suffix.
// alpha/beta/a/b/rc/p/pl/patch allow an optional (dotted or bare) numeric qualifier: -beta1, -RC.2
// dev/stable only allow bare or dot-separated numeric qualifier: -dev, -dev.1 (not -dev1)
var validVersion = regexp.MustCompile(`(?i)^\d+(\.\d+){0,3}(-((alpha|beta|a|b|rc|p|pl|patch)(\.?\d+)?|(dev|stable)(\.\d+)?))?$`)

// Normalize converts a WordPress version string to a Composer-compatible form.
// Returns empty string for invalid versions.
func Normalize(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	// Strip leading v/V to match Composer's VersionParser behavior.
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	if strings.EqualFold(v, "trunk") || v == "dev-trunk" {
		return "dev-trunk"
	}
	if !IsValid(v) {
		return ""
	}
	return v
}

// IsValid checks whether a version string is a valid WordPress version.
func IsValid(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if strings.EqualFold(v, "trunk") || v == "dev-trunk" {
		return true
	}
	return validVersion.MatchString(v)
}

// NormalizeVersions filters and normalizes a version map (version -> download URL),
// returning only entries with valid versions.
func NormalizeVersions(versions map[string]string) map[string]string {
	result := make(map[string]string, len(versions))
	for v, url := range versions {
		normalized := Normalize(v)
		if normalized != "" {
			result[normalized] = url
		}
	}
	return result
}

// FilterNewerThan removes tagged versions newer than maxVersion. dev-trunk is
// preserved; an invalid maxVersion disables filtering.
func FilterNewerThan(versions map[string]string, maxVersion string) map[string]string {
	max := Normalize(maxVersion)
	if max == "" || max == "dev-trunk" {
		return versions
	}

	result := make(map[string]string, len(versions))
	for v, url := range versions {
		if v == "dev-trunk" || Compare(v, max) <= 0 {
			result[v] = url
		}
	}
	return result
}

// Compare compares two version strings numerically by segment.
// Pre-release suffixes (e.g. -beta1) are compared lexically when
// numeric segments are equal. Returns -1, 0, or 1.
func Compare(a, b string) int {
	aBase, aSuffix := splitSuffix(a)
	bBase, bSuffix := splitSuffix(b)

	aParts := strings.Split(aBase, ".")
	bParts := strings.Split(bBase, ".")
	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}
	for i := range maxLen {
		var av, bv int
		if i < len(aParts) {
			av, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bv, _ = strconv.Atoi(bParts[i])
		}
		if c := cmp.Compare(av, bv); c != 0 {
			return c
		}
	}

	// Same base version — compare suffixes.
	// No suffix (stable) > any suffix (pre-release).
	if aSuffix == "" && bSuffix != "" {
		return 1
	}
	if aSuffix != "" && bSuffix == "" {
		return -1
	}
	return cmp.Compare(strings.ToLower(aSuffix), strings.ToLower(bSuffix))
}

// splitSuffix splits "1.0-beta1" into ("1.0", "beta1").
func splitSuffix(v string) (string, string) {
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

// IsStable returns true if the version has no pre-release suffix.
func IsStable(v string) bool {
	return !strings.Contains(v, "-")
}

// Latest returns the highest stable version from a map of version -> download URL,
// excluding dev-trunk. Falls back to the highest pre-release version if no stable
// versions exist.
func Latest(versions map[string]string) string {
	var latestStable, latestAny string
	for v := range versions {
		if v == "dev-trunk" {
			continue
		}
		if IsStable(v) {
			if latestStable == "" || Compare(v, latestStable) > 0 {
				latestStable = v
			}
		}
		if latestAny == "" || Compare(v, latestAny) > 0 {
			latestAny = v
		}
	}
	if latestStable != "" {
		return latestStable
	}
	return latestAny
}
