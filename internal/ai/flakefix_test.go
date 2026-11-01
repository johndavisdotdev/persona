package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// scriptedAPI answers each request with the next tool call from script (the
// last one repeats), and records the tool results it was sent.
type scriptedAPI struct {
	mu      sync.Mutex
	script  []func(n int) (string, any)
	calls   int
	results []string
}

func (s *scriptedAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	if last := req.Messages[len(req.Messages)-1]; last.Role == "user" {
		var rs []struct{ Content string }
		if json.Unmarshal(last.Content, &rs) == nil {
			for _, x := range rs {
				s.results = append(s.results, x.Content)
			}
		}
	}
	i := s.calls
	s.calls++
	s.mu.Unlock()
	step := s.script[min(i, len(s.script)-1)]
	name, input := step(i)
	in, _ := json.Marshal(input)
	json.NewEncoder(w).Encode(map[string]any{
		"stop_reason": "tool_use",
		"content":     []any{map[string]any{"type": "tool_use", "id": "t", "name": name, "input": json.RawMessage(in)}},
	})
}

func fixer(t *testing.T, api *scriptedAPI) *FlakeFixer {
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return &FlakeFixer{Client: &Client{APIKey: "test", BaseURL: srv.URL, Model: "test", HTTP: srv.Client()}}
}

func TestFlakeFixerFixes(t *testing.T) {
	api := &scriptedAPI{script: []func(int) (string, any){
		func(int) (string, any) {
			return "check_attrs", map[string]any{"attrs": []string{"beamPackages.elixir"}}
		},
		func(int) (string, any) { return "rebuild", map[string]any{"flake": "v1", "change": "try v1"} },
		func(int) (string, any) {
			return "rebuild", map[string]any{"flake": "v2", "change": "use beamPackages.elixir"}
		},
	}}
	var built []string
	var checked []string
	res, err := fixer(t, api).Fix(context.Background(), FlakeFixInput{
		Name: "p", Flake: "v0", Log: "error: attribute 'elixir' missing",
		Rebuild: func(_ context.Context, flake string) (string, error) {
			built = append(built, flake)
			if flake == "v2" {
				return "", nil
			}
			return "error: still broken", errors.New("build failed")
		},
		CheckAttrs: func(_ context.Context, attrs []string) (string, error) {
			checked = append(checked, attrs...)
			return `{"beamPackages.elixir":"ok 1.18.5"}`, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fixed || res.Rebuilds != 2 || res.Summary != "use beamPackages.elixir" {
		t.Errorf("result = %+v", res)
	}
	if strings.Join(built, ",") != "v1,v2" || len(checked) != 1 {
		t.Errorf("built %v, checked %v", built, checked)
	}
	// The failed rebuild's log went back to the agent.
	if !strings.Contains(strings.Join(api.results, "\n"), "error: still broken") {
		t.Errorf("agent never saw the second failure: %q", api.results)
	}
}

func TestFlakeFixerStopsAtMaxRebuilds(t *testing.T) {
	api := &scriptedAPI{script: []func(int) (string, any){
		func(n int) (string, any) { return "rebuild", map[string]any{"flake": "attempt", "change": "again"} },
	}}
	builds := 0
	res, err := fixer(t, api).Fix(context.Background(), FlakeFixInput{
		Name: "p", Flake: "v0", Log: "boom",
		Rebuild: func(context.Context, string) (string, error) {
			builds++
			return "boom", errors.New("build failed")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fixed || res.Rebuilds != MaxFlakeRebuilds || builds != MaxFlakeRebuilds {
		t.Errorf("result = %+v after %d builds, want %d failed rebuilds", res, builds, MaxFlakeRebuilds)
	}
	if api.calls != MaxFlakeRebuilds {
		t.Errorf("%d agent turns; the loop should end with the last rebuild", api.calls)
	}
}

func TestFlakeFixerGivesUp(t *testing.T) {
	api := &scriptedAPI{script: []func(int) (string, any){
		func(int) (string, any) { return "give_up", map[string]any{"reason": "Docker is not running"} },
	}}
	res, err := fixer(t, api).Fix(context.Background(), FlakeFixInput{
		Name: "p", Flake: "v0", Log: "Cannot connect to the Docker daemon",
		Rebuild: func(context.Context, string) (string, error) {
			t.Error("rebuilt after giving up")
			return "", nil
		},
	})
	if err != nil || res.Fixed || res.Rebuilds != 0 || res.Summary != "Docker is not running" {
		t.Errorf("result = %+v, %v", res, err)
	}
}
