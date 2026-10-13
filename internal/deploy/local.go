package deploy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

// Promote atomically switches the "current" symlink to point at the given build.
func Promote(repoDir, buildID string, logger *slog.Logger) error {
	buildDir := filepath.Join(repoDir, "builds", buildID)
	if err := ValidateBuild(buildDir); err != nil {
		return fmt.Errorf("invalid build %s: %w", buildID, err)
	}

	currentLink := filepath.Join(repoDir, "current")
	tmpLink := currentLink + ".tmp"

	// Remove stale tmp link
	_ = os.Remove(tmpLink)

	// Symlink target is relative to the symlink's parent (repoDir)
	symlinkTarget := filepath.Join("builds", buildID)
	if err := os.Symlink(symlinkTarget, tmpLink); err != nil {
		return fmt.Errorf("creating temp symlink: %w", err)
	}
	if err := os.Rename(tmpLink, currentLink); err != nil {
		_ = os.Remove(tmpLink)
		return fmt.Errorf("atomic rename: %w", err)
	}

	logger.Info("promoted build", "build_id", buildID)
	return nil
}

// Rollback promotes a previous build. If targetID is empty, uses the most recent
// non-current build.
func Rollback(repoDir, targetID string, logger *slog.Logger) (string, error) {
	currentID, _ := CurrentBuildID(repoDir) // ok if missing

	if targetID == "" {
		builds, err := ListBuilds(repoDir)
		if err != nil {
			return "", err
		}
		for _, build := range slices.Backward(builds) {
			if build != currentID {
				targetID = build
				break
			}
		}
		if targetID == "" {
			return "", fmt.Errorf("no previous build available for rollback")
		}
	}

	if err := Promote(repoDir, targetID, logger); err != nil {
		return "", err
	}

	logger.Info("rolled back", "from", currentID, "to", targetID)
	return targetID, nil
}

// Cleanup removes old builds, keeping the current build and up to retainCount others.
func Cleanup(repoDir string, retainCount int, logger *slog.Logger) (int, error) {
	if retainCount < 5 {
		logger.Warn("retain count below minimum, clamping to 5", "requested", retainCount)
		retainCount = 5
	}

	currentID, _ := CurrentBuildID(repoDir)
	builds, err := ListBuilds(repoDir)
	if err != nil {
		return 0, err
	}

	// Determine which builds to keep
	keep := make(map[string]bool)
	if currentID != "" {
		keep[currentID] = true
	}

	// Keep most recent N builds (excluding current which is already kept)
	kept := 0
	for i := len(builds) - 1; i >= 0 && kept < retainCount; i-- {
		if !keep[builds[i]] {
			keep[builds[i]] = true
			kept++
		}
	}

	var removed int
	for _, id := range builds {
		if keep[id] {
			continue
		}
		buildDir := filepath.Join(repoDir, "builds", id)
		if err := os.RemoveAll(buildDir); err != nil {
			logger.Warn("failed to remove build", "build_id", id, "error", err)
			continue
		}
		removed++
		logger.Debug("removed build", "build_id", id)
	}

	if removed > 0 {
		logger.Info("cleanup complete", "removed", removed, "retained", len(builds)-removed)
	}
	return removed, nil
}

// CurrentBuildID returns the build ID of the currently promoted build.
func CurrentBuildID(repoDir string) (string, error) {
	target, err := os.Readlink(filepath.Join(repoDir, "current"))
	if err != nil {
		return "", fmt.Errorf("reading current symlink: %w", err)
	}
	return filepath.Base(target), nil
}

// LatestBuildID returns the most recent build ID by sorted directory name.
func LatestBuildID(repoDir string) (string, error) {
	builds, err := ListBuilds(repoDir)
	if err != nil {
		return "", err
	}
	if len(builds) == 0 {
		return "", fmt.Errorf("no builds found")
	}
	return builds[len(builds)-1], nil
}

// ListBuilds returns sorted build IDs from the builds directory.
func ListBuilds(repoDir string) ([]string, error) {
	buildsDir := filepath.Join(repoDir, "builds")
	entries, err := os.ReadDir(buildsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing builds: %w", err)
	}

	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// ValidateBuild checks that required artifacts (packages.json, manifest.json) exist.
func ValidateBuild(buildDir string) error {
	for _, f := range []string{"packages.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(buildDir, f)); err != nil {
			return fmt.Errorf("%s missing", f)
		}
	}
	return nil
}

// ReadManifest reads and parses manifest.json from a build directory.
func ReadManifest(buildDir string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(buildDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// BuildDirFromID constructs the build directory path.
func BuildDirFromID(repoDir, buildID string) string {
	return filepath.Join(repoDir, "builds", buildID)
}

// FormatBuildAge returns a human-readable age string.
func FormatBuildAge(buildID string) string {
	t, err := time.Parse("20060102-150405", buildID)
	if err != nil {
		return "unknown"
	}
	d := time.Since(t).Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
