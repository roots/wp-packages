package http

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPaginationURL_PreservesAllQueryParams(t *testing.T) {
	got := paginationURL(3, "/untagged.md", "", "filter=trunk-only&search=woo&sort=updated&page=2")
	// Only `page` should change; everything else preserved.
	for _, want := range []string{"filter=trunk-only", "search=woo", "sort=updated", "page=3"} {
		if !strings.Contains(got, want) {
			t.Errorf("paginationURL: got %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "page=2") {
		t.Errorf("paginationURL: got %q, should not still contain page=2", got)
	}
}

func TestPaginationURL_PrependsAppURL(t *testing.T) {
	got := paginationURL(2, "/untagged.md", "https://wp-packages.org", "")
	if !strings.HasPrefix(got, "https://wp-packages.org/untagged.md") {
		t.Errorf("paginationURL: got %q, want absolute URL with AppURL prefix", got)
	}
}

func TestSetPaginationLinkHeader_NextOnly(t *testing.T) {
	w := httptest.NewRecorder()
	// page 1 of 5 → only "next"
	setPaginationLinkHeader(w, 1, 100, 20, "/untagged.md", "", "")
	link := w.Header().Get("Link")
	if !strings.Contains(link, `rel="next"`) {
		t.Errorf("Link: got %q, want next entry", link)
	}
	if strings.Contains(link, `rel="prev"`) {
		t.Errorf("Link: got %q, should not have prev on page 1", link)
	}
}

func TestSetPaginationLinkHeader_PrevOnly(t *testing.T) {
	w := httptest.NewRecorder()
	// page 5 of 5 → only "prev"
	setPaginationLinkHeader(w, 5, 100, 20, "/untagged.md", "", "")
	link := w.Header().Get("Link")
	if !strings.Contains(link, `rel="prev"`) {
		t.Errorf("Link: got %q, want prev entry", link)
	}
	if strings.Contains(link, `rel="next"`) {
		t.Errorf("Link: got %q, should not have next on last page", link)
	}
}

func TestSetPaginationLinkHeader_BothPrevAndNext(t *testing.T) {
	w := httptest.NewRecorder()
	// page 3 of 5 → both
	setPaginationLinkHeader(w, 3, 100, 20, "/untagged.md", "", "filter=trunk-only")
	link := w.Header().Get("Link")
	if !strings.Contains(link, `rel="prev"`) || !strings.Contains(link, `rel="next"`) {
		t.Errorf("Link: got %q, want both prev and next", link)
	}
	if !strings.Contains(link, "filter=trunk-only") {
		t.Errorf("Link: got %q, want preserved query", link)
	}
}

func TestSetPaginationLinkHeader_SinglePageNoEntries(t *testing.T) {
	w := httptest.NewRecorder()
	setPaginationLinkHeader(w, 1, 5, 20, "/untagged.md", "", "")
	if link := w.Header().Get("Link"); link != "" {
		t.Errorf("Link: got %q, want empty for single page", link)
	}
}

func TestSetPaginationLinkHeader_AppendsToExistingLink(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("Link", `</foo.md>; rel="alternate"; type="text/markdown"`)
	setPaginationLinkHeader(w, 2, 100, 20, "/untagged.md", "", "")
	link := w.Header().Get("Link")
	if !strings.Contains(link, `rel="alternate"`) {
		t.Errorf("Link: got %q, want existing alternate preserved", link)
	}
	if !strings.Contains(link, `rel="prev"`) || !strings.Contains(link, `rel="next"`) {
		t.Errorf("Link: got %q, want pagination entries appended", link)
	}
}

func TestAppendPaginationFooter_OmittedForSinglePage(t *testing.T) {
	var b strings.Builder
	appendPaginationFooter(&b, 1, 5, 20, "/untagged.md", "", "")
	if b.Len() != 0 {
		t.Errorf("got %q, want empty for single-page result", b.String())
	}
}

func TestAppendPaginationFooter_FirstPage(t *testing.T) {
	var b strings.Builder
	appendPaginationFooter(&b, 1, 100, 20, "/untagged.md", "", "")
	out := b.String()
	if !strings.Contains(out, "Page 1 of 5") {
		t.Errorf("got %q, want page indicator", out)
	}
	if !strings.Contains(out, "Next page →") {
		t.Errorf("got %q, want next link", out)
	}
	if strings.Contains(out, "Previous") {
		t.Errorf("got %q, should not have prev on first page", out)
	}
}

func TestAppendPaginationFooter_MiddlePage(t *testing.T) {
	var b strings.Builder
	appendPaginationFooter(&b, 3, 100, 20, "/untagged.md", "", "filter=trunk-only")
	out := b.String()
	if !strings.Contains(out, "Previous page") || !strings.Contains(out, "Next page") {
		t.Errorf("got %q, want both nav links", out)
	}
	if !strings.Contains(out, "filter=trunk-only") {
		t.Errorf("got %q, want preserved query in nav links", out)
	}
}

func TestAppendPaginationFooter_LastPage(t *testing.T) {
	var b strings.Builder
	appendPaginationFooter(&b, 5, 100, 20, "/untagged.md", "", "")
	out := b.String()
	if !strings.Contains(out, "Previous page") {
		t.Errorf("got %q, want prev link", out)
	}
	if strings.Contains(out, "Next page") {
		t.Errorf("got %q, should not have next on last page", out)
	}
}

func TestTotalPages(t *testing.T) {
	cases := []struct {
		total, perPage, want int
	}{
		{0, 20, 1},   // empty result still reads as page 1 of 1
		{1, 20, 1},   // partial first page
		{20, 20, 1},  // exact fit
		{21, 20, 2},  // overflow into 2nd page
		{100, 20, 5}, // even multiple
		{99, 20, 5},  // last page partial
	}
	for _, c := range cases {
		got := totalPages(c.total, c.perPage)
		if got != c.want {
			t.Errorf("totalPages(%d, %d) = %d, want %d", c.total, c.perPage, got, c.want)
		}
	}
}
