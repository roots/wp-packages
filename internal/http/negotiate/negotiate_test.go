package negotiate

import "testing"

func TestPreferred(t *testing.T) {
	produces := []string{"text/html", "text/markdown"}

	tests := []struct {
		name     string
		header   string
		produces []string
		want     string
	}{
		{"empty header defaults to first", "", produces, "text/html"},
		{"wildcard picks first", "*/*", produces, "text/html"},
		{"explicit html", "text/html", produces, "text/html"},
		{"explicit markdown", "text/markdown", produces, "text/markdown"},
		{"order favors markdown when tied", "text/markdown, text/html", produces, "text/markdown"},
		{"order favors html when listed first", "text/html, text/markdown", produces, "text/html"},
		{"q-values pick higher", "text/html;q=0.5, text/markdown;q=0.9", produces, "text/markdown"},
		{"q-values pick higher reversed", "text/markdown;q=0.5, text/html;q=0.9", produces, "text/html"},
		{"q=0 rejects markdown but */* keeps html", "text/markdown;q=0, */*", produces, "text/html"},
		{"q=0 rejects html but */* keeps markdown", "text/html;q=0, text/markdown", produces, "text/markdown"},
		{"q=0 with wildcard q=1 still rejects specific (RFC 9110)", "text/html;q=0, */*;q=1", produces, "text/markdown"},
		{"both rejected returns empty", "text/html;q=0, text/markdown;q=0", produces, ""},
		{"image/png matches nothing", "image/png", produces, ""},
		{"text/* glob picks first by client order", "text/*", produces, "text/html"},
		{"unknown plus markdown", "application/json, text/markdown", produces, "text/markdown"},
		{"browser-style accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8", produces, "text/html"},
		{"agent asking for markdown first", "text/markdown, text/html;q=0.9, */*;q=0.5", produces, "text/markdown"},
		{"agent asking for markdown only with html rejected", "text/markdown, text/html;q=0", produces, "text/markdown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Preferred(tt.header, tt.produces)
			if got != tt.want {
				t.Errorf("Preferred(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestPreferredHTMLOnlyProducer(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"", "text/html"},
		{"*/*", "text/html"},
		{"text/html", "text/html"},
		{"text/markdown", ""},
		{"text/html;q=0", ""},
	}
	for _, tt := range tests {
		got := Preferred(tt.header, []string{"text/html"})
		if got != tt.want {
			t.Errorf("Preferred(%q, [text/html]) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestParse(t *testing.T) {
	got := Parse("text/markdown;q=0.9, text/html, */*;q=0.5")
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if got[0].Type != "text/markdown" || got[0].Q != 0.9 || got[0].Specificity != 2 {
		t.Errorf("entry 0: %+v", got[0])
	}
	if got[1].Type != "text/html" || got[1].Q != 1.0 || got[1].Specificity != 2 {
		t.Errorf("entry 1: %+v", got[1])
	}
	if got[2].Type != "*/*" || got[2].Q != 0.5 || got[2].Specificity != 0 {
		t.Errorf("entry 2: %+v", got[2])
	}
}

func TestParseClampsQ(t *testing.T) {
	got := Parse("text/html;q=2.0, text/markdown;q=-1")
	if got[0].Q != 1.0 {
		t.Errorf("q=2.0 should clamp to 1, got %v", got[0].Q)
	}
	if got[1].Q != 0.0 {
		t.Errorf("q=-1 should clamp to 0, got %v", got[1].Q)
	}
}
