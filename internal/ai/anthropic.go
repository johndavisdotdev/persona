// Package ai designs personas with Claude: it interprets a user's description,
// searches plugin marketplaces and the web, and emits a persona spec.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a minimal Anthropic Messages API client with tool use.
type Client struct {
	APIKey  string
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// Tool is a tool definition exposed to the model.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    []block   `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
}

type block struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	CacheControl map[string]string `json:"cache_control,omitempty"`
}

type toolResult struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

type response struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type apiError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ToolHandler executes a tool call. Returning done=true ends the loop.
type ToolHandler func(ctx context.Context, name string, input json.RawMessage) (result string, done bool, err error)

// RunTools runs an agentic tool-use loop until a handler reports done,
// the model stops calling tools, or maxTurns is reached.
func (c *Client) RunTools(ctx context.Context, system, user string, tools []Tool, handle ToolHandler, maxTurns int) error {
	if c.APIKey == "" {
		return errors.New("no Anthropic API key: set ANTHROPIC_API_KEY or anthropic_api_key in the persona config")
	}
	msgs := []message{{Role: "user", Content: user}}
	sys := []block{{Type: "text", Text: system, CacheControl: map[string]string{"type": "ephemeral"}}}

	for turn := 0; turn < maxTurns; turn++ {
		resp, err := c.create(ctx, request{Model: c.Model, MaxTokens: 16000, System: sys, Messages: msgs, Tools: tools})
		if err != nil {
			return err
		}
		// Echo assistant content back verbatim (preserves any thinking signatures).
		msgs = append(msgs, message{Role: "assistant", Content: resp.Content})

		var results []toolResult
		finished := false
		for _, raw := range resp.Content {
			var b contentBlock
			if err := json.Unmarshal(raw, &b); err != nil || b.Type != "tool_use" {
				continue
			}
			out, done, herr := handle(ctx, b.Name, b.Input)
			r := toolResult{Type: "tool_result", ToolUseID: b.ID, Content: out}
			if herr != nil {
				r.Content, r.IsError = herr.Error(), true
			}
			if r.Content == "" {
				r.Content = "(empty)"
			}
			results = append(results, r)
			if done && herr == nil {
				finished = true
			}
		}
		if finished {
			return nil
		}
		if len(results) == 0 {
			if resp.StopReason == "max_tokens" {
				return errors.New("model response was truncated (max_tokens)")
			}
			// Nudge the model to finish via the submit tool.
			msgs = append(msgs, message{Role: "user", Content: "Please finish by calling the submit_persona tool."})
			continue
		}
		msgs = append(msgs, message{Role: "user", Content: results})
	}
	return fmt.Errorf("persona design did not finish within %d turns", maxTurns)
}

func (c *Client) create(ctx context.Context, req request) (*response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("content-type", "application/json")
		httpReq.Header.Set("x-api-key", c.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")

		res, err := hc.Do(httpReq)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if res.StatusCode == http.StatusOK {
			var out response
			if err := json.Unmarshal(data, &out); err != nil {
				return nil, fmt.Errorf("decode Anthropic response: %w", err)
			}
			return &out, nil
		}
		var ae apiError
		_ = json.Unmarshal(data, &ae)
		lastErr = fmt.Errorf("anthropic API %d %s: %s", res.StatusCode, ae.Error.Type, ae.Error.Message)
		if res.StatusCode != 429 && res.StatusCode != 529 && res.StatusCode < 500 {
			return nil, lastErr
		}
	}
	return nil, lastErr
}
