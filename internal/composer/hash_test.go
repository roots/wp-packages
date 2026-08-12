package composer

import "testing"

const testVersions = `{"5.3.7":"https://downloads.wordpress.org/plugin/akismet.5.3.7.zip","dev-trunk":""}`

func hashOrFail(t *testing.T, meta PackageMeta) string {
	t.Helper()
	h, err := HashContent("plugin", "akismet", testVersions, meta)
	if err != nil {
		t.Fatalf("HashContent: %v", err)
	}
	return h
}

// TestHashContentCoversMetadata guards the defect that blocked the Phase 3
// cutover: content_hash used to cover only versions_json + trunk_revision, while
// the serialized p2 output also embeds description, homepage, author and time.
// A package whose metadata changed without a version change stayed "clean" and
// its R2 files went permanently stale. wp.org readme-only commits move
// last_updated without touching versions, so this is routine, not a corner case.
func TestHashContentCoversMetadata(t *testing.T) {
	base := PackageMeta{
		Description: "Anti-spam",
		Homepage:    "https://akismet.com",
		Author:      "Automattic",
		LastUpdated: "2026-01-01T00:00:00Z",
	}
	baseHash := hashOrFail(t, base)

	mutations := map[string]func(*PackageMeta){
		"description":  func(m *PackageMeta) { m.Description = "Anti-spam, now with more spam" },
		"homepage":     func(m *PackageMeta) { m.Homepage = "https://example.com" },
		"author":       func(m *PackageMeta) { m.Author = "Someone Else" },
		"last updated": func(m *PackageMeta) { m.LastUpdated = "2026-06-01T00:00:00Z" },
		"requires php": func(m *PackageMeta) { m.RequiresPHP = "8.2" },
		"trunk rev":    func(m *PackageMeta) { r := int64(12345); m.TrunkRevision = &r },
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			m := base
			mutate(&m)
			if got := hashOrFail(t, m); got == baseHash {
				t.Errorf("hash unchanged after %s changed — the package would never be re-uploaded", name)
			}
		})
	}
}

func TestHashContentStableForIdenticalInput(t *testing.T) {
	meta := PackageMeta{Description: "Anti-spam", LastUpdated: "2026-01-01T00:00:00Z"}
	if hashOrFail(t, meta) != hashOrFail(t, meta) {
		t.Error("HashContent is not deterministic")
	}
}

func TestHashContentChangesWithVersions(t *testing.T) {
	meta := PackageMeta{Description: "Anti-spam"}
	a, err := HashContent("plugin", "akismet", testVersions, meta)
	if err != nil {
		t.Fatalf("HashContent: %v", err)
	}
	b, err := HashContent("plugin", "akismet",
		`{"5.3.8":"https://downloads.wordpress.org/plugin/akismet.5.3.8.zip","dev-trunk":""}`, meta)
	if err != nil {
		t.Fatalf("HashContent: %v", err)
	}
	if a == b {
		t.Error("hash unchanged after versions changed")
	}
}

// The hash covers object keys, so a package moving between types — which changes
// where its files live on R2 — is dirty and gets re-uploaded under the new keys.
func TestHashContentCoversObjectKeys(t *testing.T) {
	meta := PackageMeta{Description: "shared"}
	versions := `{"1.0.0":"https://downloads.wordpress.org/theme/x.1.0.0.zip"}`

	plugin, err := HashContent("plugin", "x", versions, meta)
	if err != nil {
		t.Fatalf("HashContent: %v", err)
	}
	theme, err := HashContent("theme", "x", versions, meta)
	if err != nil {
		t.Fatalf("HashContent: %v", err)
	}
	if plugin == theme {
		t.Error("plugin and theme with identical versions hash the same")
	}
}
