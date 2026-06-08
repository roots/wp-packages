package version

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Numeric versions
		{"1.0", "1.0"},
		{"1.0.0", "1.0.0"},
		{"1.0.0.0", "1.0.0.0"},
		{"5.3.2", "5.3.2"},

		// Trunk
		{"trunk", "dev-trunk"},
		{"Trunk", "dev-trunk"},
		{"TRUNK", "dev-trunk"},
		{"dev-trunk", "dev-trunk"},
		{"vtrunk", "dev-trunk"},
		{"Vtrunk", "dev-trunk"},

		// Valid pre-release suffixes
		{"1.0-beta1", "1.0-beta1"},
		{"1.0-RC2", "1.0-RC2"},
		{"1.0-alpha", "1.0-alpha"},
		{"2.0.0-beta.1", "2.0.0-beta.1"},
		{"1.0-alpha3", "1.0-alpha3"},
		{"1.0-a1", "1.0-a1"},
		{"1.0-b2", "1.0-b2"},
		{"1.0-p", "1.0-p"},
		{"1.0-p1", "1.0-p1"},
		{"1.0-pl", "1.0-pl"},
		{"1.0-pl1", "1.0-pl1"},
		{"1.0-patch", "1.0-patch"},
		{"1.0-patch1", "1.0-patch1"},
		{"1.0-dev", "1.0-dev"},
		{"1.0-dev.1", "1.0-dev.1"},
		{"1.0-stable", "1.0-stable"},
		{"1.0-stable.1", "1.0-stable.1"},

		// Case-insensitive pre-release
		{"1.0-Beta1", "1.0-Beta1"},
		{"1.0-rc2", "1.0-rc2"},
		{"1.0-ALPHA", "1.0-ALPHA"},
		{"1.0-DEV", "1.0-DEV"},

		// Invalid: empty/whitespace
		{"", ""},
		{"  ", ""},

		// Invalid: non-version strings
		{"stable", ""},
		{"latest", ""},
		{"not a version", ""},

		// Leading v stripped (Composer VersionParser compatibility, issue #19)
		{"v1.0", "1.0"},
		{"v1.0.0", "1.0.0"},
		{"V2.0.0", "2.0.0"},
		{"v1.13.11-beta.0", "1.13.11-beta.0"},
		{"v20100102", "20100102"},

		// Invalid: structural
		{"1.0.0.0.1", ""}, // 5+ parts

		// Invalid: non-Composer pre-release suffixes (issue #17)
		{"3.1.0-dev1", ""}, // dev + bare number
		{"3.1.0-dev2", ""},
		{"3.1.0-free", ""}, // not a Composer keyword
		{"1.0f", ""},       // trailing letter, no hyphen
		{"1.0-foo", ""},    // arbitrary suffix

		// Invalid: trailing dot with no digits
		{"1.0-beta.", ""},
		{"1.0-rc.", ""},
		{"1.0-alpha.", ""},
		{"1.0-dev.", ""},
		{"1.0-stable.", ""},

		// Invalid: Wordfence corpus samples
		{"1.0 12319", ""}, // space in version
		{"1.0.1 Lite", ""},
		{"1.1(Beta)", ""},
		{"1.3..4", ""},     // double dot
		{"08-03-2018", ""}, // date format
		{"2.24080000-WP6.6.1", ""},
		{"2025r1", ""},
		{"3.0 (Beta r7)", ""},
		{"3.1.37.11.L", ""}, // 5 parts + letter
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := Normalize(tt.input)
			if got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsValid(t *testing.T) {
	valid := []string{
		"1.0", "1.0.0", "1.0.0.0", "trunk", "dev-trunk",
		"1.0-beta1", "1.0-RC2", "1.0-alpha",
		"1.0-dev", "1.0-dev.1", "1.0-stable",
	}
	for _, v := range valid {
		if !IsValid(v) {
			t.Errorf("IsValid(%q) = false, want true", v)
		}
	}

	invalid := []string{
		"", "stable", "1.0.0.0.1", "not valid",
		"3.1.0-dev1", "3.1.0-dev2", "3.1.0-free",
		"1.0f", "1.0-foo",
		"1.0-beta.", "1.0-rc.", "1.0-alpha.", "1.0-dev.", "1.0-stable.",
	}
	for _, v := range invalid {
		if IsValid(v) {
			t.Errorf("IsValid(%q) = true, want false", v)
		}
	}
}

func TestNormalizeVersions(t *testing.T) {
	input := map[string]string{
		"1.0":        "https://example.com/1.0.zip",
		"2.0":        "https://example.com/2.0.zip",
		"dev-trunk":  "https://example.com/trunk.zip",
		"v3.0":       "https://example.com/v3.0.zip",
		"":           "https://example.com/empty.zip",
		"bad!":       "https://example.com/bad.zip",
		"3.1.0-dev1": "https://example.com/dev1.zip",
	}

	got := NormalizeVersions(input)
	if len(got) != 4 {
		t.Fatalf("NormalizeVersions returned %d entries, want 4", len(got))
	}
	if got["1.0"] != "https://example.com/1.0.zip" {
		t.Error("missing 1.0")
	}
	if got["dev-trunk"] != "https://example.com/trunk.zip" {
		t.Error("missing dev-trunk")
	}
	if got["3.0"] != "https://example.com/v3.0.zip" {
		t.Error("v3.0 should have been normalized to 3.0")
	}
	if _, ok := got["3.1.0-dev1"]; ok {
		t.Error("3.1.0-dev1 should have been filtered out")
	}
}

func TestFilterNewerThan(t *testing.T) {
	versions := map[string]string{
		"10.6.0":    "https://example.com/10.6.0.zip",
		"10.7.0":    "https://example.com/10.7.0.zip",
		"10.8.0":    "https://example.com/10.8.0.zip",
		"dev-trunk": "https://example.com/trunk.zip",
	}

	got := FilterNewerThan(versions, "10.7.0")
	if len(got) != 3 {
		t.Fatalf("FilterNewerThan returned %d entries, want 3", len(got))
	}
	if _, ok := got["10.8.0"]; ok {
		t.Fatal("10.8.0 should have been filtered out")
	}
	if got["10.7.0"] == "" {
		t.Fatal("10.7.0 should remain")
	}
	if got["dev-trunk"] == "" {
		t.Fatal("dev-trunk should remain")
	}

	unfiltered := FilterNewerThan(versions, "not-a-version")
	if len(unfiltered) != len(versions) {
		t.Fatalf("invalid ceiling should not filter versions")
	}
}

func TestIsStable(t *testing.T) {
	stable := []string{"1.0", "1.0.0", "10.6.2", "5.3.2"}
	for _, v := range stable {
		if !IsStable(v) {
			t.Errorf("IsStable(%q) = false, want true", v)
		}
	}

	unstable := []string{"1.0-beta1", "10.7.0-beta.1", "1.0-RC2", "1.0-alpha", "1.0-dev", "1.0-dev.1"}
	for _, v := range unstable {
		if IsStable(v) {
			t.Errorf("IsStable(%q) = true, want false", v)
		}
	}
}

func TestLatest(t *testing.T) {
	tests := []struct {
		name     string
		versions map[string]string
		want     string
	}{
		{
			name:     "stable wins over higher beta",
			versions: map[string]string{"10.6.2": "", "10.7.0-beta.1": "", "10.6.1": ""},
			want:     "10.6.2",
		},
		{
			name:     "only betas returns highest beta",
			versions: map[string]string{"1.0-beta1": "", "1.0-beta2": ""},
			want:     "1.0-beta2",
		},
		{
			name:     "dev-trunk excluded",
			versions: map[string]string{"dev-trunk": "", "1.0": ""},
			want:     "1.0",
		},
		{
			name:     "only dev-trunk returns empty",
			versions: map[string]string{"dev-trunk": ""},
			want:     "",
		},
		{
			name:     "empty map",
			versions: map[string]string{},
			want:     "",
		},
		{
			name:     "stable with RC",
			versions: map[string]string{"2.0": "", "2.1-RC1": "", "1.9": ""},
			want:     "2.0",
		},
		{
			name:     "multiple stable picks highest",
			versions: map[string]string{"1.0": "", "2.0": "", "3.0": ""},
			want:     "3.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Latest(tt.versions)
			if got != tt.want {
				t.Errorf("Latest() = %q, want %q", got, tt.want)
			}
		})
	}
}
