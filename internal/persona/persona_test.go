package persona

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/config"
)

func setup(t *testing.T) {
	t.Helper()
	viper.Reset()
	if err := config.Init("", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../base-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	DefaultBaseSettings = b
}

func TestEffectivePersonaWinsAndBaseMerges(t *testing.T) {
	p := &Persona{
		Name: "dev", Instructions: "Be terse.",
		Plugins:    []Plugin{{Name: "linear", Marketplace: "claude-plugins-official", Reason: "persona"}},
		MCPServers: []MCPServer{{Name: "gh", Type: "http", URL: "https://persona.example"}},
	}
	b := &Base{
		Instructions: "Never push to main.", Model: "sonnet",
		Plugins:     []Plugin{{Name: "linear", Marketplace: "claude-plugins-official", Reason: "base"}, {Name: "commit-commands", Marketplace: "claude-plugins-official"}},
		MCPServers:  []MCPServer{{Name: "gh", Type: "http", URL: "https://base.example"}},
		AptPackages: []string{"jq"},
	}
	e := Effective(p, b)
	if len(e.Plugins) != 2 || e.Plugins[0].Reason != "persona" {
		t.Fatalf("plugins not merged with persona precedence: %+v", e.Plugins)
	}
	if e.MCPServers[0].URL != "https://persona.example" || len(e.MCPServers) != 1 {
		t.Fatalf("persona MCP server should win: %+v", e.MCPServers)
	}
	if e.Model != "sonnet" || !strings.Contains(e.Instructions, "Never push to main.") {
		t.Fatalf("base model/instructions not applied: %q %q", e.Model, e.Instructions)
	}
	if len(p.Plugins) != 1 {
		t.Fatal("Effective must not mutate the persona")
	}
}

func TestRenderDockerfileIncludesBase(t *testing.T) {
	setup(t)
	if err := (&Base{Plugins: []Plugin{{Name: "linear", Marketplace: "claude-plugins-official"}},
		Marketplaces: []Marketplace{{Name: "claude-plugins-official", Source: "anthropics/claude-plugins-official"}}}).Save(); err != nil {
		t.Fatal(err)
	}
	df, err := RenderDockerfile(&Persona{Name: "x", MCPServers: []MCPServer{{Name: "m", Command: "uvx", Args: []string{"some-mcp"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"COPY --chown=claude:claude settings.json /home/claude/.claude/settings.json", "claude plugin install \"$id\"", "astral.sh/uv"} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
}

func TestHandEditedFilesAreProtected(t *testing.T) {
	setup(t)
	p := &Persona{Name: "x"}
	if _, err := p.Generate(false); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(SettingsPath("x"), []byte(`{"model":"haiku"}`)); err != nil {
		t.Fatal(err)
	}
	skipped, err := p.Generate(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != FileSettings {
		t.Fatalf("expected settings.json to be skipped, got %v", skipped)
	}
	if edited, _ := p.EditedFiles(); len(edited) != 1 {
		t.Fatalf("EditedFiles = %v", edited)
	}
	if _, err := p.Generate(true); err != nil {
		t.Fatal(err)
	}
	if edited, _ := p.EditedFiles(); len(edited) != 0 {
		t.Fatalf("force should regenerate, still edited: %v", edited)
	}
}

func TestSettingsLayering(t *testing.T) {
	setup(t)
	if err := (&Base{Plugins: []Plugin{{Name: "linear", Marketplace: "claude-plugins-official"}},
		Marketplaces: []Marketplace{{Name: "claude-plugins-official", Source: "anthropics/claude-plugins-official"}}}).Save(); err != nil {
		t.Fatal(err)
	}
	p := &Persona{
		Name:    "x",
		Model:   "sonnet",
		Plugins: []Plugin{{Name: "commit-commands", Marketplace: "claude-plugins-official"}},
		Settings: map[string]any{
			"theme":       "light",
			"permissions": map[string]any{"allow": []any{"Bash(git:*)"}},
		},
	}
	out, err := RenderSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatal(err)
	}
	en := s["enabledPlugins"].(map[string]any)
	for _, id := range []string{"prismantis@prismantis", "linear@claude-plugins-official", "commit-commands@claude-plugins-official"} {
		if en[id] != true {
			t.Errorf("plugin %s missing from merged settings: %v", id, en)
		}
	}
	mps := s["extraKnownMarketplaces"].(map[string]any)
	if mps["prismantis"] == nil || mps["claude-plugins-official"] == nil {
		t.Errorf("marketplaces not merged: %v", mps)
	}
	if s["model"] != "sonnet" || s["theme"] != "light" {
		t.Errorf("persona model/overrides should win: model=%v theme=%v", s["model"], s["theme"])
	}
	perms := s["permissions"].(map[string]any)
	if perms["defaultMode"] != "bypassPermissions" || perms["allow"] == nil {
		t.Errorf("nested objects should deep-merge: %v", perms)
	}
	if s["tui"] != "fullscreen" {
		t.Errorf("base settings key lost: %v", s["tui"])
	}
}

func TestBaseSettingsFileOverridesDefault(t *testing.T) {
	setup(t)
	if err := os.WriteFile(BaseSettingsPath(), []byte(`{"model":"haiku"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := RenderSettings(&Persona{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"model": "haiku"`) || strings.Contains(out, "prismantis") {
		t.Fatalf("user base-settings.json should replace the built-in default:\n%s", out)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"owner/repo": "owner/repo", "a b": "'a b'", "it's": `'it'"'"'s'`, "": "''"} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "data-eng", "x1"} {
		if ValidateName(ok) != nil {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-a", "A", "a_b", "../x"} {
		if ValidateName(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
