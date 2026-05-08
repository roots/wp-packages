// Package negotiate parses the HTTP Accept header and chooses a
// representation from a server-defined list of producible media types.
//
// Behavior follows RFC 9110 §12.5.1: a more specific media range
// overrides a less specific one regardless of q-value, so
// `text/html;q=0, */*;q=1` correctly rejects text/html.
package negotiate

import (
	"strconv"
	"strings"
)

// Entry is one parsed media range from an Accept header.
type Entry struct {
	Type        string
	Q           float64
	Specificity int // 0=*/*, 1=type/*, 2=exact
}

// Parse splits an Accept header into entries, preserving client order.
// Position is significant — used as the tiebreaker when two ranges
// have equal specificity and q-value.
func Parse(header string) []Entry {
	if header == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	out := make([]Entry, 0, len(parts))
	for _, raw := range parts {
		segments := strings.Split(strings.TrimSpace(raw), ";")
		t := strings.ToLower(strings.TrimSpace(segments[0]))
		if t == "" {
			continue
		}
		q := 1.0
		for _, p := range segments[1:] {
			eq := strings.IndexByte(p, '=')
			if eq < 0 {
				continue
			}
			name := strings.TrimSpace(p[:eq])
			value := strings.TrimSpace(p[eq+1:])
			if name == "q" {
				if parsed, err := strconv.ParseFloat(value, 64); err == nil {
					if parsed < 0 {
						parsed = 0
					}
					if parsed > 1 {
						parsed = 1
					}
					q = parsed
				}
			}
		}
		spec := 2
		switch {
		case t == "*/*":
			spec = 0
		case strings.HasSuffix(t, "/*"):
			spec = 1
		}
		out = append(out, Entry{Type: t, Q: q, Specificity: spec})
	}
	return out
}

// Preferred returns the producible media type the client most prefers,
// or "" if the client rejects every option.
//
// Special case: an empty Accept header means "give me anything," so the
// first item in produces wins. A non-empty header that excludes every
// option returns "" — the caller should answer 406.
func Preferred(header string, produces []string) string {
	if header == "" {
		if len(produces) == 0 {
			return ""
		}
		return produces[0]
	}
	entries := Parse(header)
	if len(entries) == 0 {
		if len(produces) == 0 {
			return ""
		}
		return produces[0]
	}

	bestType := ""
	bestQ := -1.0
	bestPos := -1

	for _, candidate := range produces {
		// For each candidate, find the *most specific* matching range.
		// RFC 9110 §12.5.1: specific ranges override less specific ones
		// regardless of q — so `text/html;q=0, */*;q=1` correctly rejects
		// text/html instead of letting the wildcard override.
		matchedQ := -1.0
		matchedSpec := -1
		matchedPos := -1
		for i, e := range entries {
			if !matches(e, candidate) {
				continue
			}
			if matchedPos < 0 ||
				e.Specificity > matchedSpec ||
				(e.Specificity == matchedSpec && i < matchedPos) {
				matchedQ = e.Q
				matchedSpec = e.Specificity
				matchedPos = i
			}
		}
		if matchedPos < 0 || matchedQ <= 0 {
			continue
		}

		// Across candidates: highest q wins; tie-break on client order
		// so `Accept: text/markdown, text/html, */*` picks text/markdown.
		if matchedQ > bestQ || (matchedQ == bestQ && (bestPos < 0 || matchedPos < bestPos)) {
			bestQ = matchedQ
			bestPos = matchedPos
			bestType = candidate
		}
	}
	return bestType
}

func matches(e Entry, candidate string) bool {
	if e.Type == "*/*" {
		return true
	}
	if strings.HasSuffix(e.Type, "/*") {
		return strings.HasPrefix(candidate, e.Type[:len(e.Type)-1])
	}
	return e.Type == candidate
}
