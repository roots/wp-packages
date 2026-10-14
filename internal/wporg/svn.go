package wporg

import (
	"bufio"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type SVNEntry struct {
	Slug          string
	LastCommitted *time.Time
}

// SVNListingResult holds the parsed SVN listing along with the repository revision.
type SVNListingResult struct {
	Revision int64
}

// ParseSVNListing fetches the SVN HTML directory listing and extracts slugs.
// It also returns the current SVN revision from the page header.
func (c *Client) ParseSVNListing(ctx context.Context, baseURL string, fn func(SVNEntry) error) (*SVNListingResult, error) {
	client := &http.Client{Timeout: 600 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating SVN request: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching SVN listing: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SVN listing returned status %d", resp.StatusCode)
	}

	return parseSVNHTML(ctx, resp.Body, fn, c.logger)
}

func parseSVNHTML(ctx context.Context, r interface{ Read([]byte) (int, error) }, fn func(SVNEntry) error, logger *slog.Logger) (*SVNListingResult, error) {
	scanner := bufio.NewScanner(r)
	result := &SVNListingResult{}

	var count int
	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		line := scanner.Text()

		// Extract revision from title: " - Revision 3483213: /"
		if result.Revision == 0 {
			if rev := parseSVNRevision(line); rev > 0 {
				result.Revision = rev
			}
		}

		// Each entry is: <li><a href="slug-name/">slug-name/</a></li>
		idx := strings.Index(line, `<a href="`)
		if idx < 0 {
			continue
		}
		rest := line[idx+len(`<a href="`):]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			continue
		}
		href := rest[:end]

		slug := strings.TrimSuffix(href, "/")
		if slug == "" || slug == ".." || strings.HasPrefix(slug, "!svn") {
			continue
		}

		if err := fn(SVNEntry{Slug: slug}); err != nil {
			return result, err
		}

		count++
		if count%10000 == 0 {
			logger.Info("SVN discovery progress", "entries", count)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading SVN listing: %w", err)
	}

	logger.Info("SVN discovery complete", "total_entries", count, "revision", result.Revision)
	return result, nil
}

// parseSVNRevision extracts a revision number from lines like:
//
//	<title> - Revision 3483213: /</title>
//	<h2> - Revision 3483213: /</h2>
func parseSVNRevision(line string) int64 {
	const marker = "Revision "
	idx := strings.Index(line, marker)
	if idx < 0 {
		return 0
	}
	rest := line[idx+len(marker):]
	end := strings.IndexAny(rest, ":< ")
	if end < 0 {
		return 0
	}
	rev, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil {
		return 0
	}
	return rev
}

// svnLogReport is the XML structure for an SVN DAV log-report response.
type svnLogReport struct {
	Items []svnLogItem `xml:"log-item"`
}

type svnLogItem struct {
	Revision      int64     `xml:"version-name"`
	Date          string    `xml:"date"`
	AddedPaths    []svnPath `xml:"added-path"`
	ModifiedPaths []svnPath `xml:"modified-path"`
	DeletedPaths  []svnPath `xml:"deleted-path"`
	ReplacedPaths []svnPath `xml:"replaced-path"`
}

type svnPath struct {
	Path     string `xml:",chardata"`
	Kind     string `xml:"node-kind,attr"`
	PropMods string `xml:"prop-mods,attr"`
}

// SVNLog is a parsed SVN changelog for a revision range.
type SVNLog struct {
	// SlugRevisions maps each top-level slug (plugin/theme name) to the highest
	// revision that touched it within the range.
	SlugRevisions map[string]int64
	// MaxRev is the highest revision present in the response.
	MaxRev int64
	// MinRev is the lowest revision present in the response (zero if empty).
	MinRev int64
	// MinDate is the earliest commit date in the response (zero if empty or
	// no dates parsed).
	MinDate time.Time
	// Commits holds every log item in the response.
	Commits []SVNCommit
}

// SVNCommit is a single revision from the SVN changelog.
type SVNCommit struct {
	Revision int64
	Date     time.Time
	Added    []string
	Modified []string
	Deleted  []string
	Replaced []string
	// PlainFilePaths holds the paths logged as files without property changes
	// (node-kind="file", prop-mods="false").
	PlainFilePaths map[string]bool
}

// SVNLogLimit is the maximum number of log items FetchSVNLog requests. A range
// spanning more revisions than this comes back truncated to the newest ones.
const SVNLogLimit = 50000

// FetchSVNLog queries the SVN DAV log between two revisions (with changed
// paths). The caller must advance its watermark to the returned MaxRev (not to
// an externally-assumed HEAD), because the REPORT endpoint can lag behind
// other endpoints; advancing past revisions the REPORT never returned would
// skip those commits permanently.
func (c *Client) FetchSVNLog(ctx context.Context, baseURL string, fromRev, toRev int64) (*SVNLog, error) {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>`+
		`<S:log-report xmlns:S="svn:" xmlns:D="DAV:">`+
		`<S:start-revision>%d</S:start-revision>`+
		`<S:end-revision>%d</S:end-revision>`+
		`<S:discover-changed-paths/>`+
		`<S:limit>%d</S:limit>`+
		`</S:log-report>`, toRev, fromRev, SVNLogLimit)

	reqURL := strings.TrimSuffix(baseURL, "/") + "/!svn/bc/0/"
	req, err := http.NewRequestWithContext(ctx, "REPORT", reqURL, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating SVN log request: %w", err)
	}
	req.Header.Set("Content-Type", "text/xml")
	req.Header.Set("User-Agent", UserAgent)

	// Use a generous timeout — catch-up runs after downtime can span many
	// revisions and produce large responses.
	davClient := &http.Client{Timeout: 600 * time.Second}
	resp, err := davClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching SVN log: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("SVN log returned status %d: %s", resp.StatusCode, string(respBody))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading SVN log response: %w", err)
	}

	return parseSVNLog(data)
}

// sanitizeXML strips illegal XML 1.0 characters (control chars except tab, newline, carriage return).
func sanitizeXML(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for _, b := range data {
		if b == 0x09 || b == 0x0A || b == 0x0D || b >= 0x20 {
			out = append(out, b)
		}
	}
	return out
}

// parseSVNLog parses SVN log XML. It maps each unique top-level slug to the
// highest revision that touched it, and records the highest revision present in
// the response (across all log-items, regardless of whether a path yielded a
// slug) so callers can advance their watermark to exactly what the REPORT
// covered.
// Paths look like "/plugin-name/trunk/file.php" — we extract "plugin-name".
func parseSVNLog(data []byte) (*SVNLog, error) {
	data = sanitizeXML(data)
	var report svnLogReport
	if err := xml.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parsing SVN log XML: %w", err)
	}

	log := &SVNLog{SlugRevisions: make(map[string]int64)}
	for _, item := range report.Items {
		if item.Revision > log.MaxRev {
			log.MaxRev = item.Revision
		}
		if log.MinRev == 0 || item.Revision < log.MinRev {
			log.MinRev = item.Revision
		}

		commit := SVNCommit{Revision: item.Revision, PlainFilePaths: make(map[string]bool)}
		collect := func(paths []svnPath) []string {
			out := make([]string, 0, len(paths))
			for _, p := range paths {
				out = append(out, p.Path)
				if p.Kind == "file" && p.PropMods == "false" {
					commit.PlainFilePaths[p.Path] = true
				}
			}
			return out
		}
		commit.Added = collect(item.AddedPaths)
		commit.Modified = collect(item.ModifiedPaths)
		commit.Deleted = collect(item.DeletedPaths)
		commit.Replaced = collect(item.ReplacedPaths)
		if t, err := time.Parse(time.RFC3339Nano, item.Date); err == nil {
			commit.Date = t.UTC()
			if log.MinDate.IsZero() || commit.Date.Before(log.MinDate) {
				log.MinDate = commit.Date
			}
		}
		log.Commits = append(log.Commits, commit)

		for _, p := range commit.paths() {
			if slug := slugFromPath(p); slug != "" {
				if item.Revision > log.SlugRevisions[slug] {
					log.SlugRevisions[slug] = item.Revision
				}
			}
		}
	}

	return log, nil
}

func (c SVNCommit) paths() []string {
	all := make([]string, 0, len(c.Added)+len(c.Modified)+len(c.Deleted)+len(c.Replaced))
	all = append(all, c.Added...)
	all = append(all, c.Modified...)
	all = append(all, c.Deleted...)
	all = append(all, c.Replaced...)
	return all
}

// TagChangeSet holds the tag changes found in an SVN log.
type TagChangeSet struct {
	// Tags maps slug → tag name → latest date that tag was created or changed.
	Tags map[string]map[string]time.Time
	// All maps slug → latest date every tag of the package may have changed at
	// once: the tags directory (or the package root) was added or replaced.
	// SVN logs only the copied directory, not the files inside it, so a copy
	// over /<slug>/tags would otherwise go unseen.
	All map[string]time.Time
}

// TagChanges returns the tags whose contents were created or changed in the
// log, with the latest commit date that did so.
//
// Plugin tags live at /<slug>/tags/<tag>/; theme versions live directly at
// /<slug>/<version>/. Any added, modified, replaced, or deleted path inside a
// tag counts, including the tag directory itself:
//   - Deleting a tag can change what a version resolves to when another raw
//     tag normalizes to the same version (e.g. deleting 1.0 leaves v1.0).
//   - A modified directory is a property-only change, but the log doesn't say
//     which property; svn:externals inside a tag changes what Composer's SVN
//     source checkout (and wp.org's BuddyPress zip) contains.
//
// Property changes on /<slug>/tags or /<slug> are ignored: externals defined
// there don't apply when a tag is checked out, and wp.org builds zips from the
// tag directory. Deleting those directories isn't a change either.
//
// Commits that only touch the tag's readme file are ignored: authors routinely
// bump "Tested up to" in the stable tag, and treating that as a new release
// would keep popular plugins permanently inside Composer's cooldown. Only paths
// logged as files without property changes qualify; a directory copied to a
// readme path (whose contents SVN doesn't log) is a change, and so is a
// property change such as svn:special, which turns the readme into a symlink
// in source checkouts. Replacing a readme always counts: the log shows
// only the new node's kind, so a readme directory with code in it replaced by a
// file looks like a file edit.
func (l *SVNLog) TagChanges(pkgType string) TagChangeSet {
	changes := make(map[string]map[string]time.Time)
	all := make(map[string]time.Time)
	for _, c := range l.Commits {
		for _, paths := range [][]string{c.Added, c.Replaced} {
			for _, p := range paths {
				slug, ok := tagsAncestor(pkgType, p)
				if !ok {
					continue
				}
				if prev, seen := all[slug]; !seen || c.Date.After(prev) {
					all[slug] = c.Date
				}
			}
		}

		// tag key (slug + "/" + tag) → touched by a non-readme change
		touched := make(map[[2]string]bool)
		mark := func(p string, readmeExempt bool) {
			slug, tag, rest, ok := splitTagPath(pkgType, p)
			if !ok {
				return
			}
			key := [2]string{slug, tag}
			if readmeExempt && c.PlainFilePaths[p] && isReadme(rest) {
				if _, seen := touched[key]; !seen {
					touched[key] = false
				}
				return
			}
			touched[key] = true
		}
		for _, paths := range [][]string{c.Added, c.Modified, c.Deleted} {
			for _, p := range paths {
				mark(p, true)
			}
		}
		for _, p := range c.Replaced {
			mark(p, false)
		}

		for key, changed := range touched {
			if !changed {
				continue
			}
			slug, tag := key[0], key[1]
			if changes[slug] == nil {
				changes[slug] = make(map[string]time.Time)
			}
			if prev, ok := changes[slug][tag]; !ok || c.Date.After(prev) {
				changes[slug][tag] = c.Date
			}
		}
	}
	return TagChangeSet{Tags: changes, All: all}
}

// tagsAncestor reports whether p is a directory containing every tag of a
// package: /<slug> or /<slug>/tags for plugins, /<slug> for themes.
func tagsAncestor(pkgType, p string) (slug string, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if parts[0] == "" {
		return "", false
	}
	if len(parts) == 1 {
		return parts[0], true
	}
	if pkgType != "theme" && len(parts) == 2 && parts[1] == "tags" {
		return parts[0], true
	}
	return "", false
}

// splitTagPath splits an SVN path into slug, tag name, and the path inside the
// tag ("" for the tag directory itself). ok is false for paths outside a tag.
// Plugin tag names are mapped the way wp.org maps them to API versions.
func splitTagPath(pkgType, p string) (slug, tag, rest string, ok bool) {
	parts := strings.SplitN(strings.Trim(p, "/"), "/", 4)
	if pkgType == "theme" {
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", "", "", false
		}
		slug, tag = parts[0], parts[1]
		rest = strings.Join(parts[2:], "/")
		return slug, tag, rest, true
	}
	if len(parts) < 3 || parts[0] == "" || parts[1] != "tags" || parts[2] == "" {
		return "", "", "", false
	}
	slug, tag = parts[0], parts[2]
	// wp.org lists a tag like ".1" as version "0.1" (plugin-directory
	// cli/class-import.php), so map it the same way to match versions_json.
	if strings.HasPrefix(tag, ".") {
		tag = "0" + tag
	}
	if len(parts) == 4 {
		rest = parts[3]
	}
	return slug, tag, rest, true
}

func isReadme(rest string) bool {
	switch strings.ToLower(rest) {
	case "readme.txt", "readme.md":
		return true
	}
	return false
}

// slugFromPath extracts the top-level directory (slug) from an SVN path.
// e.g. "/akismet/trunk/akismet.php" → "akismet"
func slugFromPath(path string) string {
	path = strings.TrimPrefix(path, "/")
	if idx := strings.IndexByte(path, '/'); idx > 0 {
		return path[:idx]
	}
	// bare directory entry like "akismet"
	if path != "" && path != ".." {
		return path
	}
	return ""
}
