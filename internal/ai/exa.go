package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Exa is a minimal client for the Exa search API.
type Exa struct {
	APIKey string
	HTTP   *http.Client
}

// ExaResult is one search or contents result.
type ExaResult struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Text  string `json:"text,omitempty"`
}

// Enabled reports whether an API key is configured.
func (e *Exa) Enabled() bool { return e != nil && e.APIKey != "" }

// Search runs a neural/keyword search and returns page text snippets.
func (e *Exa) Search(ctx context.Context, query string, n int) ([]ExaResult, error) {
	if n <= 0 || n > 10 {
		n = 8
	}
	body := map[string]any{
		"query":      query,
		"numResults": n,
		"type":       "auto",
		"contents":   map[string]any{"text": map[string]any{"maxCharacters": 1500}},
	}
	var out struct {
		Results []ExaResult `json:"results"`
	}
	return out.Results, e.post(ctx, "/search", body, &out)
}

// Contents fetches the text of the given URLs.
func (e *Exa) Contents(ctx context.Context, urls []string, maxChars int) ([]ExaResult, error) {
	body := map[string]any{"urls": urls, "text": map[string]any{"maxCharacters": maxChars}}
	var out struct {
		Results []ExaResult `json:"results"`
	}
	return out.Results, e.post(ctx, "/contents", body, &out)
}

func (e *Exa) post(ctx context.Context, path string, body, out any) error {
	if !e.Enabled() {
		return errors.New("Exa is not configured (set EXA_API_KEY)")
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.exa.ai"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", e.APIKey)
	hc := e.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("exa %s: %s: %s", path, res.Status, truncate(string(data), 300))
	}
	return json.Unmarshal(data, out)
}
