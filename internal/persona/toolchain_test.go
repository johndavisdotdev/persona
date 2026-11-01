package persona

import (
	"strings"
	"testing"
)

func TestValidateToolchain(t *testing.T) {
	ok := []Toolchain{
		{Name: "dotnet"}, {Name: "dotnet", Version: "8"}, {Name: "go", Version: "1.23"}, {Name: "node", Version: "lts"},
		{Name: "zig", Nix: "zig"},
		{Name: "zig", Source: "https://ziglang.org/download/", Install: "curl -fsSL https://ziglang.org/x | tar -x"},
	}
	for _, tc := range ok {
		if err := ValidateToolchain(tc); err != nil {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	bad := map[string]Toolchain{
		"shell in version":   {Name: "go", Version: "1; rm -rf /"},
		"built-in with nix":  {Name: "go", Nix: "go"},
		"custom without how": {Name: "zig"},
		"install, no source": {Name: "zig", Install: "curl x"},
		"http source":        {Name: "zig", Install: "curl x", Source: "http://ziglang.org"},
		"bad nix attr":       {Name: "zig", Nix: "zig; curl evil"},
		"bad path":           {Name: "zig", Nix: "zig", Path: []string{"/opt/zig:$(evil)"}},
		"uppercase name":     {Name: "Go"},
	}
	for name, tc := range bad {
		if err := ValidateToolchain(tc); err == nil {
			t.Errorf("%s: accepted %+v", name, tc)
		}
	}
}

func TestNormalizeToolchains(t *testing.T) {
	got := normalizeToolchains([]Toolchain{
		{Name: "playwright"}, {Name: "zig", Nix: "zig"}, {Name: "dotnet", Version: "9"}, {Name: "dotnet", Version: "8"}, {Name: "bogus"},
	})
	var names []string
	for _, tc := range got {
		names = append(names, tc.String())
	}
	if strings.Join(names, " ") != "dotnet@9 node playwright zig" {
		t.Errorf("normalized = %v", names)
	}
}

func TestToolchainsInFlake(t *testing.T) {
	setup(t)
	p := &Persona{Name: "web-dev", Harness: HarnessClaude, Toolchains: []Toolchain{{Name: "playwright"}, {Name: "dotnet"},
		{Name: "python", Version: "3.12"}, {Name: "java", Version: "21"}, {Name: "rust"}, {Name: "go", Version: "1.27.1"},
		{Name: "lua", Nix: "lua"}, {Name: "zig", Source: "https://ziglang.org/download/", Install: "echo install zig", Path: []string{"/opt/zig"}}}}
	e := Effective(p, nil)
	found := false
	for _, pl := range e.Plugins {
		found = found || pl.ID() == PlaywrightPlugin.ID()
	}
	if !found {
		t.Errorf("playwright plugin not implied: %+v", e.Plugins)
	}
	d, err := render(e, "{}")
	if err != nil {
		t.Fatal(err)
	}
	flake, root, user := d.Files[FileFlake], d.Files[FileSetupRoot], d.Files[FileSetupUser]
	for _, want := range []string{
		`(newest "dotnet" stable [ "dotnet-sdk_20"`, `(withPip (attr "python312"))`, `(attr "jdk21")`, `(attr "go_1_27")`,
		`(attr "rustup")`, `(attr "lua")`, `(newest "node" nodeLTS`, "libsOf browserLibs", "PATH=/opt/zig:",
		"LD_LIBRARY_PATH=/lib", "DOTNET_ROOT=", "JAVA_HOME=",
	} {
		if !strings.Contains(flake, want) {
			t.Errorf("flake lacks %q", want)
		}
	}
	if !strings.Contains(root, "installed as documented at https://ziglang.org/download/") || !strings.Contains(root, "echo install zig") {
		t.Error("custom install missing from setup-root.sh")
	}
	for _, want := range []string{"rustup default stable", "@playwright/cli@latest", "playwright-cli install-browser chromium", "playwright-cli install --skills"} {
		if !strings.Contains(user, want) {
			t.Errorf("setup-user.sh lacks %q", want)
		}
	}
	if !strings.Contains(d.Support["instructions.md"], "- lua, nixpkgs lua") {
		t.Errorf("instructions: %s", d.Support["instructions.md"])
	}
	for _, f := range []string{flake, root, user} {
		if strings.Contains(f, "%!") || strings.Contains(f, "<no value>") {
			t.Errorf("template error:\n%s", f)
		}
	}
}

func TestAptMigratesToNix(t *testing.T) {
	setup(t)
	d, err := Render(&Persona{Name: "x", AptPackages: []string{"build-essential", "sqlite3", "python3-venv", "ffmpeg", "libssl-dev", "libpq-dev", "tshark"}})
	if err != nil {
		t.Fatal(err)
	}
	f := d.Files[FileFlake]
	for _, want := range []string{`"sqlite"`, `"ffmpeg"`, `"gnumake"`, `"openssl.dev"`, `"libpq.dev|postgresql.dev"`, `"wireshark-cli"`, "CPATH=/include"} {
		if !strings.Contains(f, want) {
			t.Errorf("flake lacks %s", want)
		}
	}
	if strings.Contains(f, "python3-venv") || strings.Contains(f, `"sqlite3"`) {
		t.Error("apt names leaked into the flake")
	}
}

func TestBuiltinMarketplaceDeclared(t *testing.T) {
	p := Effective(&Persona{Name: "w", Toolchains: []Toolchain{{Name: "playwright"}}}, nil)
	mps, _ := specSettings(p)["extraKnownMarketplaces"].(map[string]any)
	src, _ := mps["claude-plugins-official"].(map[string]any)["source"].(map[string]string)
	if src["repo"] != "anthropics/claude-plugins-official" {
		t.Errorf("claude-plugins-official not declared: %+v", mps)
	}
	local, _ := mps["persona"].(map[string]any)["source"].(map[string]string)
	if local["source"] != "directory" || local["path"] != MarketplaceDir {
		t.Errorf("persona marketplace: %+v", local)
	}
}
