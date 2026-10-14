package composer

import (
	"encoding/json"
	"testing"
)

func TestComposerVersionPublishedTime(t *testing.T) {
	meta := PackageMeta{
		LastUpdated:    "2026-10-12T00:00:00Z",
		PublishedTimes: map[string]string{"1.0": "2026-09-01T00:00:00Z"},
	}

	tagged := ComposerVersion("plugin", "akismet", "1.0", "https://example.com/1.0.zip", meta)
	if got := tagged["published-time"]; got != "2026-09-01T00:00:00Z" {
		t.Errorf("tagged published-time = %v, want stored time", got)
	}
	if got := tagged["time"]; got != "2026-09-01T00:00:00Z" {
		t.Errorf("tagged time = %v, want published time", got)
	}

	unknown := ComposerVersion("plugin", "akismet", "2.0", "https://example.com/2.0.zip", meta)
	if _, ok := unknown["published-time"]; ok {
		t.Error("version without a published time should omit published-time")
	}
	if got := unknown["time"]; got != "2026-10-12T00:00:00Z" {
		t.Errorf("untracked time = %v, want last updated", got)
	}

	trunk := ComposerVersion("plugin", "akismet", "dev-trunk", "", meta)
	if got := trunk["published-time"]; got != "2026-10-12T00:00:00Z" {
		t.Errorf("dev-trunk published-time = %v, want last updated", got)
	}
}

func TestSerializePackagePublishedTime(t *testing.T) {
	meta := PackageMeta{
		LastUpdated:    "2026-10-12T00:00:00Z",
		PublishedTimes: ParsePublishedTimes(strPtr(`{"1.0":"2026-09-01T00:00:00Z"}`)),
	}
	data, err := SerializePackage("plugin", "akismet", `{"1.0":"https://example.com/1.0.zip"}`, meta)
	if err != nil {
		t.Fatal(err)
	}

	var payload struct {
		Packages map[string]map[string]map[string]any `json:"packages"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload.Packages["wp-plugin/akismet"]["1.0"]["published-time"]; got != "2026-09-01T00:00:00Z" {
		t.Errorf("serialized published-time = %v", got)
	}
}

func TestParsePublishedTimes(t *testing.T) {
	if ParsePublishedTimes(nil) != nil {
		t.Error("NULL should parse to nil")
	}
	if ParsePublishedTimes(strPtr("not json")) != nil {
		t.Error("invalid JSON should parse to nil")
	}
	if got := ParsePublishedTimes(strPtr("{}")); got == nil {
		t.Error("{} should parse to an empty, non-nil map")
	}

	// Keys stored before normalization was idempotent (" 2.1") still match.
	got := ParsePublishedTimes(strPtr(`{" 2.1":"2026-01-01T00:00:00Z","2.1":"2026-02-01T00:00:00Z","v3.0":"2026-03-01T00:00:00Z","bad!":"x"}`))
	want := map[string]string{"2.1": "2026-02-01T00:00:00Z", "3.0": "2026-03-01T00:00:00Z"}
	if len(got) != len(want) || got["2.1"] != want["2.1"] || got["3.0"] != want["3.0"] {
		t.Errorf("ParsePublishedTimes = %v, want %v", got, want)
	}
}

// TestSerializePackageEveryTaggedVersionHasPublishedTime guards against key
// mismatches between versions_json and published_times_json: Composer 2.11
// errors if any tagged version lacks published-time.
func TestSerializePackageEveryTaggedVersionHasPublishedTime(t *testing.T) {
	versionsJSON := `{" 2.1":"https://example.com/a.zip","1.0":"https://example.com/b.zip","v3.0":"https://example.com/c.zip"}`
	meta := PackageMeta{
		PublishedTimes: ParsePublishedTimes(strPtr(`{" 2.1":"2026-09-01T00:00:00Z","1.0":"2026-09-02T00:00:00Z","v3.0":"2026-09-03T00:00:00Z"}`)),
	}
	data, err := SerializePackage("plugin", "akismet", versionsJSON, meta)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Packages map[string]map[string]map[string]any `json:"packages"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	entries := payload.Packages["wp-plugin/akismet"]
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	for ver, e := range entries {
		if e["published-time"] == nil {
			t.Errorf("version %q has no published-time", ver)
		}
	}
}

func TestDevTrunkPublishedTimeRequiresBackfill(t *testing.T) {
	meta := PackageMeta{LastUpdated: "2026-10-12T00:00:00Z"}
	if _, ok := ComposerVersion("plugin", "akismet", "dev-trunk", "", meta)["published-time"]; ok {
		t.Error("dev-trunk should omit published-time until the package is backfilled")
	}
	meta.PublishedTimes = map[string]string{}
	if got := ComposerVersion("plugin", "akismet", "dev-trunk", "", meta)["published-time"]; got != "2026-10-12T00:00:00Z" {
		t.Errorf("dev-trunk published-time = %v, want last updated", got)
	}
}

func TestHashVersionsIncludesPublishedTimes(t *testing.T) {
	v := `{"1.0":"a"}`
	base := HashVersions(v, nil, nil)
	a := HashVersions(v, nil, strPtr(`{"1.0":"2026-01-01T00:00:00Z"}`))
	b := HashVersions(v, nil, strPtr(`{"1.0":"2026-10-13T00:00:00Z"}`))
	if base == a || a == b {
		t.Error("published times should change the content hash")
	}
}

func strPtr(s string) *string { return &s }
