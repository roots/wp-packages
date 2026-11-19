package blog

import (
	"encoding/json"
	"html"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Post represents a blog post from the WordPress REST API.
type Post struct {
	Title string
	Link  string
}

// PostsCache fetches and caches recent blog posts from roots.io.
type PostsCache struct {
	mu     sync.RWMutex
	posts  []Post
	logger *slog.Logger
}

func NewPostsCache(logger *slog.Logger) *PostsCache {
	c := &PostsCache{logger: logger}
	c.fetch()
	go c.loop()
	return c
}

// NewStubCache returns a PostsCache that never fetches, for use in tests.
func NewStubCache() *PostsCache {
	return &PostsCache{logger: slog.Default()}
}

func (c *PostsCache) Posts() []Post {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.posts
}

func (c *PostsCache) loop() {
	ticker := time.NewTicker(15 * time.Minute)
	for range ticker.C {
		c.fetch()
	}
}

func (c *PostsCache) fetch() {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://roots.io/wp-json/wp/v2/posts?tags=22&per_page=5&_fields=title,link")
	if err != nil {
		c.logger.Warn("blog posts fetch failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		c.logger.Warn("blog posts fetch failed", "status", resp.StatusCode)
		return
	}

	var raw []struct {
		Title struct {
			Rendered string `json:"rendered"`
		} `json:"title"`
		Link string `json:"link"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		c.logger.Warn("blog posts decode failed", "error", err)
		return
	}

	posts := make([]Post, len(raw))
	for i, r := range raw {
		// title.rendered is HTML (wptexturize converts quotes to entities like
		// &#8217;); decode it so the template's auto-escaping doesn't double-escape.
		posts[i] = Post{Title: html.UnescapeString(r.Title.Rendered), Link: r.Link}
	}

	c.mu.Lock()
	c.posts = posts
	c.mu.Unlock()
	c.logger.Info("blog posts updated", "count", len(posts))
}
