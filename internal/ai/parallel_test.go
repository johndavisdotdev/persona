package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johndavisdev/persona/internal/persona"
)

// fakeAnthropic answers each agent with its submit tool call, after a delay,
// so the test can tell whether the agents ran at the same time.
type fakeAnthropic struct {
	mu       sync.Mutex
	calls    map[string]int
	inFlight atomic.Int32
	peak     atomic.Int32
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		System []struct{ Text string }
	}
	json.NewDecoder(r.Body).Decode(&req)
	sys := req.System[0].Text
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(150 * time.Millisecond)

	var agent string
	switch {
	case strings.Contains(sys, "YOUR PART: language runtimes"):
		agent = "runtimes"
	case strings.Contains(sys, "YOUR PART: skills"):
		agent = "extensions"
	case strings.Contains(sys, "YOUR PART: configure the harness"):
		agent = "tooling"
	default:
		agent = "guide"
	}
	f.mu.Lock()
	f.calls[agent]++
	call := f.calls[agent]
	f.mu.Unlock()

	var name string
	var input any
	switch agent {
	case "runtimes":
		name = "submit_toolchains"
		if call == 1 { // forget .NET: the reconsider nudge should catch it
			input = map[string]any{"toolchains": []any{}}
		} else {
			input = map[string]any{"toolchains": []any{map[string]any{"name": "dotnet", "version": "9", "reason": "the role is .NET"}}}
		}
	case "extensions":
		name = "submit_extensions"
		input = map[string]any{"plugins": []any{}, "skills": []any{map[string]any{"name": "dotnet-testing", "repo": "example/skills", "path": "dotnet-testing", "reason": "tests"}},
			"mcp_servers": []any{map[string]any{"name": "docs", "type": "http", "url": "https://learn.microsoft.com/api/mcp", "reason": "docs"}}}
	case "tooling":
		name = "submit_tooling"
		if call == 1 { // an invalid installer must go back to this agent
			input = map[string]any{"tools": []any{map[string]any{"name": "csharp-ls", "via": "brew", "package": "csharp-ls", "reason": "LSP"}}}
		} else {
			input = map[string]any{"tools": []any{map[string]any{"name": "csharp-ls", "via": "dotnet", "package": "csharp-ls", "reason": "LSP"}},
				"packages": []any{"sqlite"}}
		}
	case "guide":
		name = "submit_summary"
		input = map[string]any{"summary": "A .NET expert.", "instructions": "You build .NET services. Run the linters."}
	}
	in, _ := json.Marshal(input)
	json.NewEncoder(w).Encode(map[string]any{
		"stop_reason": "tool_use",
		"content":     []any{map[string]any{"type": "tool_use", "id": "t" + agent, "name": name, "input": json.RawMessage(in)}},
	})
}

func TestDesignParallel(t *testing.T) {
	fake := &fakeAnthropic{calls: map[string]int{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	var logMu sync.Mutex
	var log []string
	d := &Designer{Client: &Client{APIKey: "test", BaseURL: srv.URL, Model: "test", HTTP: srv.Client()}, Catalog: NewCatalog(),
		Progress: func(s string) { logMu.Lock(); log = append(log, s); logMu.Unlock() }}

	start := time.Now()
	p, err := d.Design(context.Background(), "dotnet-dev", "A senior .NET engineer who also writes some Rust", nil)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)

	if fake.peak.Load() < 3 {
		t.Errorf("agents didn't run in parallel: at most %d requests at once", fake.peak.Load())
	}
	// 2 runtimes turns + 2 tooling turns + 1 extensions in parallel, then the guide.
	if took > 1200*time.Millisecond {
		t.Errorf("took %v; parallel agents should take ~3 rounds of 150ms", took)
	}
	var names []string
	for _, tc := range p.Toolchains {
		names = append(names, tc.String())
	}
	slices.Sort(names)
	// dotnet@9 from the agent; rust guaranteed by the description; dotnet
	// also needed by the csharp-ls tool; pi needs node.
	if strings.Join(names, " ") != "dotnet@9 rust" {
		t.Errorf("toolchains = %v", names)
	}
	if p.Harness != persona.HarnessClaude || len(p.Tools) != 1 || p.Tools[0].Via != "dotnet" ||
		!slices.Contains(p.Packages, "sqlite") || len(p.Skills) != 1 || len(p.MCPServers) != 1 ||
		p.Instructions == "" || p.Summary != "A .NET expert." {
		t.Errorf("persona = %+v", p)
	}
	if fake.calls["runtimes"] != 2 || fake.calls["tooling"] != 2 || fake.calls["extensions"] != 1 || fake.calls["guide"] != 1 {
		t.Errorf("calls = %v", fake.calls)
	}
	joined := strings.Join(log, "\n")
	for _, want := range []string{"[runtimes] Asking to reconsider", "[tooling] Rejected submission", "Added what the description mentions: rust"} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress lacks %q:\n%s", want, joined)
		}
	}
}
