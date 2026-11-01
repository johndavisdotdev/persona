package persona

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Tool is a developer tool (linter, formatter, language server, CLI)
// from nixpkgs ("nix") or with a toolchain's package manager. "apt" is from
// Debian-based specs and maps to nixpkgs (see AptToNix).
type Tool struct {
	Name    string `yaml:"name" json:"name"`
	Via     string `yaml:"via" json:"via"`         // nix, npm, uv, go, cargo, rustup, dotnet (apt: legacy)
	Package string `yaml:"package" json:"package"` // what the installer takes
	Reason  string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

type toolInstaller struct {
	toolchain string // toolchain it needs ("" for apt)
	cmd       string // install command; %s is the quoted package
}

var toolInstallers = map[string]toolInstaller{
	"nix":    {},
	"apt":    {},
	"npm":    {toolchain: "node", cmd: "npm install -g %s"},
	"uv":     {toolchain: "python", cmd: "uv tool install %s"},
	"go":     {toolchain: "go", cmd: "go install %s"},
	"cargo":  {toolchain: "rust", cmd: "cargo install --locked %s"},
	"rustup": {toolchain: "rust", cmd: "rustup component add %s"},
	"dotnet": {toolchain: "dotnet", cmd: "dotnet tool install --global %s"},
}

// ToolInstallers lists the supported installers.
func ToolInstallers() []string {
	var out []string
	for k := range toolInstallers {
		if k == "apt" {
			continue // legacy: accepted in specs, not offered
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	toolNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}$`)
	toolPackageRe = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9@/._:+~=-]{0,199}$`)
)

// ValidateTool checks a tool entry.
func ValidateTool(t Tool) error {
	if !toolNameRe.MatchString(t.Name) {
		return fmt.Errorf("invalid tool name %q", t.Name)
	}
	if _, ok := toolInstallers[t.Via]; !ok {
		return fmt.Errorf("tool %s: unknown installer %q (one of %s)", t.Name, t.Via, strings.Join(ToolInstallers(), ", "))
	}
	if !toolPackageRe.MatchString(t.Package) {
		return fmt.Errorf("tool %s: invalid package %q", t.Name, t.Package)
	}
	return nil
}

// normalizeTools drops invalid entries and duplicates (first wins).
func normalizeTools(in []Tool) []Tool {
	seen := map[string]bool{}
	var out []Tool
	for _, t := range in {
		if ValidateTool(t) != nil || seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		out = append(out, t)
	}
	return out
}

// toolToolchains are the toolchains tools need, for Normalize to add.
func toolToolchains(ts []Tool) []string {
	var out []string
	for _, t := range ts {
		if tc := toolInstallers[t.Via].toolchain; tc != "" && !slices.Contains(out, tc) {
			out = append(out, tc)
		}
	}
	return out
}

// toolCommands are the setup-user.sh commands installing tools that use a
// package manager (nix and apt tools are in the flake).
func toolCommands(ts []Tool) []toolCmd {
	var out []toolCmd
	for _, t := range ts {
		in := toolInstallers[t.Via]
		if in.cmd == "" {
			continue
		}
		pkg := t.Package
		if t.Via == "go" && !strings.Contains(pkg, "@") {
			pkg += "@latest"
		}
		out = append(out, toolCmd{Cmd: fmt.Sprintf(in.cmd, shellQuote(pkg)),
			Warning: shellQuote(fmt.Sprintf("could not install tool %s (%s %s)", t.Name, t.Via, pkg))})
	}
	return out
}
