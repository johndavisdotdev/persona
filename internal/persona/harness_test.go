package persona

import (
	"slices"
	"strings"
	"testing"
)

func TestHarnessDefaultsToClaude(t *testing.T) {
	setup(t)
	d, err := Render(&Persona{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Files[FileSetupUser], "claude.ai/install.sh") || strings.Contains(d.Files[FileSetupUser], "pi-coding-agent") ||
		!strings.Contains(d.Files[FileFlake], "PERSONA_HARNESS=claude") {
		t.Error("the default harness should be Claude Code")
	}
}

func TestPiHarnessWithPackages(t *testing.T) {
	setup(t)
	d, err := Render(&Persona{Name: "x", Harness: HarnessPi})
	if err != nil {
		t.Fatal(err)
	}
	user := d.Files[FileSetupUser]
	for _, want := range []string{
		"npm install -g --ignore-scripts @earendil-works/pi-coding-agent",
		" npm:@companion-ai/feynman ", "npm:pi-code", `pi install "$p" </dev/null || warn`,
		"~/.pi/agent/extensions/persona-status.ts", "/home/claude/.pi/agent/AGENTS.md",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("pi setup-user.sh lacks %q", want)
		}
	}
	for _, unwanted := range []string{"claude.ai/install.sh", "claude plugin install"} {
		if strings.Contains(user, unwanted) {
			t.Errorf("pi setup contains Claude Code step %q", unwanted)
		}
	}
	if strings.Contains(d.Files[FileSetupRoot], ManagedSettingsPath) {
		t.Error("managed settings are for Claude Code personas")
	}
	for _, want := range []string{"PERSONA_HARNESS=pi", `nodejs_`, "# node"} { // pi needs Node
		if !strings.Contains(d.Files[FileFlake], want) {
			t.Errorf("flake lacks %q", want)
		}
	}
	if !strings.Contains(d.Support["mcp.json"], MarketplaceDir+"/plugins/beacon/bin/persona-beacon") {
		t.Error("pi lacks the beacon MCP server")
	}
	if !strings.Contains(d.Support["pi/settings.json"], `"defaultProvider": "openrouter"`) ||
		!strings.Contains(d.Support["bin/persona-entrypoint"], `pi "$PERSONA_PROMPT"`) {
		t.Error("pi files")
	}
}

func TestHarnessPackagesMerge(t *testing.T) {
	setup(t)
	e := Effective(&Persona{Name: "x", Harness: HarnessPi, HarnessPackages: []string{"npm:extra", "-npm:bigpowers"}}, nil)
	if !slices.Contains(e.HarnessPackages, "npm:extra") || slices.Contains(e.HarnessPackages, "npm:bigpowers") ||
		len(e.HarnessPackages) != len(DefaultPiPackages) {
		t.Errorf("packages = %v", e.HarnessPackages)
	}
	none := []string{}
	b := &Base{Harness: HarnessClaude, HarnessPackages: &none}
	if e := Effective(&Persona{Name: "x"}, b); e.Harness != HarnessClaude || len(e.HarnessPackages) != 0 {
		t.Errorf("base claude: %s %v", e.Harness, e.HarnessPackages)
	}
	if e := Effective(&Persona{Name: "x", Harness: HarnessPi}, b); e.Harness != HarnessPi || len(e.HarnessPackages) != 0 {
		t.Errorf("persona pi with empty base list: %v", e.HarnessPackages)
	}
	for _, bad := range []string{"pi-code", "npm:x; rm -rf /", "npm:"} {
		if ValidateHarnessPackage(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if ValidateHarness("codex") == nil || ValidateHarness("pi") != nil || ValidateHarness("") != nil {
		t.Error("ValidateHarness")
	}
}
