package wporg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"errors"

	"github.com/roots/wp-packages/internal/config"
)

// UserAgent identifies outbound requests to WordPress.org.
const UserAgent = "wp-packages/1.0 (+https://wp-packages.org)"

// ErrNotFound is returned when a package does not exist on WordPress.org.
var ErrNotFound = errors.New("package not found")

// ErrClosedPermanent is returned when a package is closed with "This closure
// is permanent." in its description — these will never reopen and can be
// tombstoned to stop polling. Temporary or unknown closures still return
// ErrNotFound.
var ErrClosedPermanent = errors.New("package closed permanently")

type Client struct {
	http       *http.Client
	logger     *slog.Logger
	maxRetries int
	retryDelay time.Duration
	baseURL    string // override for testing; defaults to "https://api.wordpress.org"
}

func NewClient(cfg config.DiscoveryConfig, logger *slog.Logger) *Client {
	concurrency := max(cfg.Concurrency, 10)
	transport := &http.Transport{
		MaxIdleConns:        concurrency + 10,
		MaxIdleConnsPerHost: concurrency,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	return &Client{
		http: &http.Client{
			Timeout:   time.Duration(cfg.APITimeoutS) * time.Second,
			Transport: transport,
		},
		logger:     logger,
		maxRetries: cfg.MaxRetries,
		retryDelay: time.Duration(cfg.RetryDelayMs) * time.Millisecond,
		baseURL:    "https://api.wordpress.org",
	}
}

// SetBaseURL overrides the WordPress.org API base URL (for testing).
func (c *Client) SetBaseURL(u string) {
	c.baseURL = u
}

func (c *Client) FetchPlugin(ctx context.Context, slug string) (map[string]any, error) {
	u := c.baseURL + "/plugins/info/1.2/?action=plugin_information" +
		"&request%5Bslug%5D=" + url.QueryEscape(slug) +
		"&request%5Bfields%5D%5Bversions%5D=true" +
		"&request%5Bfields%5D%5Bdescription%5D=false" +
		"&request%5Bfields%5D%5Bsections%5D=false" +
		"&request%5Bfields%5D%5Bcompatibility%5D=false" +
		"&request%5Bfields%5D%5Breviews%5D=false" +
		"&request%5Bfields%5D%5Bbanners%5D=false" +
		"&request%5Bfields%5D%5Bicons%5D=false" +
		"&request%5Bfields%5D%5Bdonate_link%5D=false" +
		"&request%5Bfields%5D%5Bratings%5D=false" +
		"&request%5Bfields%5D%5Bcontributors%5D=false" +
		"&request%5Bfields%5D%5Btags%5D=false" +
		"&request%5Bfields%5D%5Bactive_installs%5D=true" +
		"&request%5Bfields%5D%5Brequires%5D=true" +
		"&request%5Bfields%5D%5Btested%5D=true" +
		"&request%5Bfields%5D%5Brequires_php%5D=true" +
		"&request%5Bfields%5D%5Bauthor%5D=true" +
		"&request%5Bfields%5D%5Bshort_description%5D=true" +
		"&request%5Bfields%5D%5Bhomepage%5D=true" +
		"&request%5Bfields%5D%5Blast_updated%5D=true" +
		"&request%5Bfields%5D%5Badded%5D=true" +
		"&request%5Bfields%5D%5Bdownload_link%5D=true"

	return c.fetchJSON(ctx, u)
}

func (c *Client) FetchTheme(ctx context.Context, slug string) (map[string]any, error) {
	u := c.baseURL + "/themes/info/1.2/?action=theme_information" +
		"&request%5Bslug%5D=" + url.QueryEscape(slug) +
		"&request%5Bfields%5D%5Bversions%5D=true" +
		"&request%5Bfields%5D%5Bactive_installs%5D=true" +
		"&request%5Bfields%5D%5Bsections%5D=true" +
		"&request%5Bfields%5D%5Bauthor%5D=true" +
		"&request%5Bfields%5D%5Bhomepage%5D=true" +
		"&request%5Bfields%5D%5Blast_updated%5D=true"

	return c.fetchJSON(ctx, u)
}

// FetchLastUpdated fetches only the last_updated date for a package (minimal API call for discovery).
func (c *Client) FetchLastUpdated(ctx context.Context, pkgType, slug string) (*time.Time, error) {
	var u string
	if pkgType == "plugin" {
		u = c.baseURL + "/plugins/info/1.2/?action=plugin_information" +
			"&request%5Bslug%5D=" + url.QueryEscape(slug) +
			"&request%5Bfields%5D%5Blast_updated%5D=true" +
			"&request%5Bfields%5D%5Bdescription%5D=false" +
			"&request%5Bfields%5D%5Bsections%5D=false" +
			"&request%5Bfields%5D%5Bversions%5D=false"
	} else {
		u = c.baseURL + "/themes/info/1.2/?action=theme_information" +
			"&request%5Bslug%5D=" + url.QueryEscape(slug) +
			"&request%5Bfields%5D%5Blast_updated%5D=true" +
			"&request%5Bfields%5D%5Bversions%5D=false"
	}

	data, err := c.fetchJSON(ctx, u)
	if err != nil {
		return nil, err
	}

	if lu, ok := data["last_updated"].(string); ok && lu != "" {
		for _, f := range []string{
			"2006-01-02 3:04pm MST",
			"2006-01-02 15:04:05",
			time.RFC3339,
			"2006-01-02",
		} {
			if t, err := time.Parse(f, lu); err == nil {
				t = t.UTC()
				return &t, nil
			}
		}
	}

	return nil, nil
}

func (c *Client) fetchJSON(ctx context.Context, rawURL string) (map[string]any, error) {
	var lastErr error

	for attempt := range c.maxRetries {
		if attempt > 0 {
			delay := c.retryDelay * time.Duration(math.Pow(2, float64(attempt-1)))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("User-Agent", UserAgent)

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("fetching %s: %w", rawURL, err)
			c.logger.Warn("API request failed, retrying", "attempt", attempt+1, "error", err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if err != nil {
			lastErr = fmt.Errorf("reading response: %w", err)
			continue
		}

		// wp.org returns HTTP 404 for closed plugins with a JSON body
		// describing the closure, so we can't short-circuit on status code
		// alone — try to parse the body first and only fall back to
		// ErrNotFound if it doesn't look like a closure payload.
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
			lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
			c.logger.Warn("API returned error status, retrying", "status", resp.StatusCode, "attempt", attempt+1)
			continue
		}

		var result map[string]any
		if err := json.Unmarshal(body, &result); err != nil {
			if resp.StatusCode == http.StatusNotFound {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("parsing JSON response: %w", err)
		}

		// Closed plugins arrive as {"error":"closed","description":"...","closed":true,...}.
		// The `description` field carries the closure blurb — if it contains
		// "This closure is permanent." the row can be tombstoned. Anything
		// else (temporary, unknown) falls through to ErrNotFound.
		if errMsg, ok := result["error"]; ok {
			if errMsg == "closed" {
				if desc, _ := result["description"].(string); strings.Contains(desc, "This closure is permanent.") {
					return nil, ErrClosedPermanent
				}
				return nil, ErrNotFound
			}
			if resp.StatusCode == http.StatusNotFound {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("API error: %v", errMsg)
		}

		if resp.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}

		return result, nil
	}

	return nil, fmt.Errorf("all %d attempts failed: %w", c.maxRetries, lastErr)
}
