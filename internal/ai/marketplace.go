package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/johndavisdev/persona/internal/config"
)

// CatalogPlugin is a plugin entry in a marketplace.
type CatalogPlugin struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Category    string   `json:"category,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// Marketplace is a fetched marketplace manifest.
type Marketplace struct {
	Name    string          `json:"name"`
	Source  string          `json:"-"`
	Plugins []CatalogPlugin `json:"plugins"`
}

// Catalog caches and searches marketplaces.
type Catalog struct {
	mu   sync.Mutex
	byID map[string]*Marketplace // keyed by marketplace name
	http *http.Client
}

// NewCatalog returns an empty catalog.
func NewCatalog() *Catalog {
	return &Catalog{byID: map[string]*Marketplace{}, http: &http.Client{Timeout: 30 * time.Second}}
}

var ghRe = regexp.MustCompile(`^(?:https?://github\.com/|git@github\.com:)?([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// NormalizeSource turns a GitHub URL into owner/repo; other sources pass through.
func NormalizeSource(src string) string {
	src = strings.TrimSpace(src)
	if m := ghRe.FindStringSubmatch(src); m != nil {
		return m[1] + "/" + m[2]
	}
	return src
}

func manifestURL(src string) (string, error) {
	if strings.HasSuffix(src, ".json") && strings.HasPrefix(src, "http") {
		return src, nil
	}
	norm := NormalizeSource(src)
	if strings.Count(norm, "/") == 1 && !strings.Contains(norm, ":") {
		return "https://raw.githubusercontent.com/" + norm + "/HEAD/.claude-plugin/marketplace.json", nil
	}
	return "", fmt.Errorf("cannot inspect marketplace %q: only GitHub repos or direct marketplace.json URLs are supported", src)
}

// Load fetches (or reads from cache) a marketplace by source.
func (c *Catalog) Load(ctx context.Context, src string) (*Marketplace, error) {
	src = NormalizeSource(src)
	c.mu.Lock()
	for _, m := range c.byID {
		if m.Source == src {
			c.mu.Unlock()
			return m, nil
		}
	}
	c.mu.Unlock()

	url, err := manifestURL(src)
	if err != nil {
		return nil, err
	}
	data, err := c.fetchCached(ctx, url)
	if err != nil {
		return nil, err
	}
	var m Marketplace
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s is not a valid marketplace.json: %w", src, err)
	}
	if m.Name == "" {
		return nil, fmt.Errorf("%s: marketplace.json has no name", src)
	}
	m.Source = src
	c.mu.Lock()
	c.byID[m.Name] = &m
	c.mu.Unlock()
	return &m, nil
}

// LoadAll loads the given sources concurrently, returning per-source errors.
func (c *Catalog) LoadAll(ctx context.Context, sources []string) []error {
	var wg sync.WaitGroup
	errs := make([]error, len(sources))
	for i, s := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Load(ctx, s); err != nil {
				errs[i] = err
			}
		}()
	}
	wg.Wait()
	var out []error
	for _, e := range errs {
		if e != nil {
			out = append(out, e)
		}
	}
	return out
}

// Get returns a loaded marketplace by name.
func (c *Catalog) Get(name string) (*Marketplace, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.byID[name]
	return m, ok
}

// Has reports whether plugin@marketplace exists in a loaded marketplace.
func (c *Catalog) Has(plugin, marketplace string) bool {
	m, ok := c.Get(marketplace)
	if !ok {
		return false
	}
	for _, p := range m.Plugins {
		if p.Name == plugin {
			return true
		}
	}
	return false
}

// Names lists loaded marketplaces as "name (source, N plugins)".
func (c *Catalog) Names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.byID {
		out = append(out, fmt.Sprintf("%s (source: %s, %d plugins)", m.Name, m.Source, len(m.Plugins)))
	}
	sort.Strings(out)
	return out
}

// Hit is a search result.
type Hit struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Category    string `json:"category,omitempty"`
	score       int
}

// Search scores plugins across all loaded marketplaces by keyword overlap.
func (c *Catalog) Search(query, marketplace string, limit int) []Hit {
	terms := tokenize(query)
	if limit <= 0 {
		limit = 15
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var hits []Hit
	for _, m := range c.byID {
		if marketplace != "" && m.Name != marketplace {
			continue
		}
		for _, p := range m.Plugins {
			name := strings.ToLower(p.Name)
			kw := strings.ToLower(strings.Join(append(append([]string{p.Category}, p.Keywords...), p.Tags...), " "))
			desc := strings.ToLower(p.Description)
			score := 0
			for _, t := range terms {
				if strings.Contains(name, t) {
					score += 5
				}
				if strings.Contains(kw, t) {
					score += 3
				}
				if strings.Contains(desc, t) {
					score++
				}
			}
			if score > 0 || len(terms) == 0 {
				hits = append(hits, Hit{ID: p.Name + "@" + m.Name, Description: truncate(p.Description, 240), Category: p.Category, score: score})
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].ID < hits[j].ID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

var stop = map[string]bool{"a": true, "an": true, "the": true, "and": true, "or": true, "for": true, "to": true, "of": true, "in": true, "with": true, "on": true}

func tokenize(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '#')
	}) {
		if len(f) > 1 && !stop[f] {
			out = append(out, f)
		}
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func (c *Catalog) fetchCached(ctx context.Context, url string) ([]byte, error) {
	key := strings.NewReplacer("https://", "", "http://", "", "/", "_", ":", "_").Replace(url)
	path := filepath.Join(config.CacheDir(), "marketplaces", key)
	if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < 6*time.Hour {
		if b, err := os.ReadFile(path); err == nil {
			return b, nil
		}
	}
	b, err := httpGet(ctx, c.http, url, 8<<20)
	if err != nil {
		if cached, cerr := os.ReadFile(path); cerr == nil {
			return cached, nil // stale cache beats nothing
		}
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		_ = os.WriteFile(path, b, 0o644)
	}
	return b, nil
}

func httpGet(ctx context.Context, hc *http.Client, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "persona-cli")
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, errors.New("not found: " + url)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	return io.ReadAll(io.LimitReader(res.Body, max))
}
