package persona

import (
	"strings"
	"testing"
)

func TestToolsRenderAndPullToolchains(t *testing.T) {
	setup(t)
	p := &Persona{Name: "x", Harness: HarnessClaude, Tools: []Tool{
		{Name: "ruff", Via: "uv", Package: "ruff"},
		{Name: "gopls", Via: "go", Package: "golang.org/x/tools/gopls"},
		{Name: "eslint", Via: "npm", Package: "eslint"},
		{Name: "shellcheck", Via: "nix", Package: "shellcheck"},
		{Name: "hadolint", Via: "apt", Package: "hadolint"},
		{Name: "bad", Via: "brew", Package: "x"},
		{Name: "evil", Via: "npm", Package: "x; rm -rf /"},
	}}
	d, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	user, flake := d.Files[FileSetupUser], d.Files[FileFlake]
	for _, want := range []string{"uv tool install ruff || warn", "go install golang.org/x/tools/gopls@latest ||", "npm install -g eslint ||"} {
		if !strings.Contains(user, want) {
			t.Errorf("setup-user.sh lacks %q", want)
		}
	}
	for _, want := range []string{`"shellcheck"`, `"hadolint"`, "# go", "# node", "# python"} { // pulled-in toolchains
		if !strings.Contains(flake, want) {
			t.Errorf("flake lacks %q", want)
		}
	}
	if !strings.Contains(d.Support["instructions.md"], "## Developer tools") {
		t.Error("instructions lack tools")
	}
	for _, bad := range []string{"brew", "x; rm -rf"} {
		if strings.Contains(user+flake, bad) {
			t.Errorf("invalid tool rendered: %q", bad)
		}
	}
}

func TestMCPLaunchersAlwaysPresent(t *testing.T) {
	setup(t)
	d, err := Render(&Persona{Name: "x", Harness: HarnessClaude})
	if err != nil {
		t.Fatal(err)
	}
	flake := d.Files[FileFlake]
	for _, want := range []string{"# node (npx/uvx for MCP servers)", "# python (npx/uvx for MCP servers)", `(attr "uv")`, `(newest "node" nodeLTS`} {
		if !strings.Contains(flake, want) {
			t.Errorf("flake lacks %q", want)
		}
	}
	if !strings.Contains(d.Files[FileSetupUser], "npx uv uvx") {
		t.Error("setup doesn't verify npx/uvx")
	}
	// A persona with the toolchains doesn't get them twice.
	d, _ = Render(&Persona{Name: "y", Harness: HarnessClaude, Toolchains: []Toolchain{{Name: "node"}, {Name: "python", Version: "3.12"}}})
	if strings.Contains(d.Files[FileFlake], "for MCP servers") {
		t.Error("node/python added twice")
	}
}
