package persona

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Toolchain is a language SDK or runtime. Images are built with Nix, so
// toolchains come from nixpkgs: the catalog (ToolchainCatalog) maps each to
// its newest stable nixpkgs package at build time (see toolchainNix).
//
// Any other runtime is custom:
//   - Nix: its nixpkgs attribute (preferred), or
//   - Install: root shell commands from the runtime's primary source
//     (Source), run during image setup, for runtimes nixpkgs lacks.
type Toolchain struct {
	Name    string   `yaml:"name" json:"name"`
	Version string   `yaml:"version,omitempty" json:"version,omitempty"` // empty: see ToolchainCatalog
	Reason  string   `yaml:"reason,omitempty" json:"reason,omitempty"`
	Source  string   `yaml:"source,omitempty" json:"source,omitempty"`   // primary source URL (custom installs)
	Install string   `yaml:"install,omitempty" json:"install,omitempty"` // custom root install commands
	Nix     string   `yaml:"nix,omitempty" json:"nix,omitempty"`         // nixpkgs attribute, e.g. "zig"
	Path    []string `yaml:"path,omitempty" json:"path,omitempty"`       // dirs a custom install adds to PATH
}

// Custom reports whether t isn't a catalog toolchain.
func (t Toolchain) Custom() bool { _, ok := toolchainInfo(t.Name); return !ok }

// ToolchainInfo describes a supported toolchain.
type ToolchainInfo struct {
	Name, Summary, Default string
	Bins                   []string // commands that must be on PATH once installed
	Requires               []string // other toolchains it needs
	Plugins                []Plugin // Claude Code plugins it implies
}

// PlaywrightPlugin is Microsoft's Playwright plugin in the official marketplace.
var PlaywrightPlugin = Plugin{Name: "playwright", Marketplace: "claude-plugins-official",
	Reason: "browser automation and end-to-end testing (implied by the playwright toolchain)"}

// ToolchainCatalog lists the toolchains persona knows how to install.
var ToolchainCatalog = []ToolchainInfo{
	{Name: "dotnet", Summary: ".NET SDK (C#, F#, ASP.NET, Blazor, MAUI libraries); a version is a major (10) and picks nixpkgs dotnet-sdk_10", Default: "newest GA SDK in nixpkgs", Bins: []string{"dotnet"}},
	{Name: "go", Summary: "Go toolchain", Default: "newest Go in nixpkgs", Bins: []string{"go", "gofmt"}},
	{Name: "rust", Summary: "Rust via rustup (rustc, cargo, clippy, rustfmt; add more with rustup); a version is a rustup toolchain", Default: "stable", Bins: []string{"rustc", "cargo", "rustup"}},
	{Name: "node", Summary: "Node.js and npm; a version is a major (24), lts or latest", Default: "newest LTS in nixpkgs", Bins: []string{"node", "npm", "npx"}},
	{Name: "bun", Summary: "Bun JavaScript runtime and package manager", Default: "latest", Bins: []string{"bun"}},
	{Name: "deno", Summary: "Deno JavaScript/TypeScript runtime", Default: "latest", Bins: []string{"deno"}},
	{Name: "java", Summary: "OpenJDK (Java, Kotlin, Scala, Gradle/Maven builds); a version is a major (21), lts or latest", Default: "newest LTS in nixpkgs",
		Bins: []string{"java", "javac"}},
	{Name: "python", Summary: "Python 3 with pip and uv; a version is major.minor (3.13) and picks nixpkgs python313", Default: "nixpkgs python3", Bins: []string{"python3", "pip3", "uv"}},
	{Name: "playwright", Summary: "Web testing suite: @playwright/cli with Chromium and its system libraries, the Playwright CLI skills, and the playwright plugin", Default: "latest",
		Requires: []string{"node"}, Plugins: []Plugin{PlaywrightPlugin}, Bins: []string{"playwright-cli"}},
}

func toolchainInfo(name string) (ToolchainInfo, bool) {
	for _, t := range ToolchainCatalog {
		if t.Name == name {
			return t, true
		}
	}
	return ToolchainInfo{}, false
}

// ToolchainNames lists the supported toolchain names.
func ToolchainNames() []string {
	var out []string
	for _, t := range ToolchainCatalog {
		out = append(out, t.Name)
	}
	return out
}

// Versions are embedded in shell commands, so they're restricted to a safe
// alphabet ("lts", "latest", "10", "1.23", "1.83.0", "nightly", "21").
var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,31}$`)

var (
	toolchainNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+._-]{0,31}$`)
	nixAttrRe       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.+-]{0,63}$`)
	pathRe          = regexp.MustCompile(`^/[A-Za-z0-9._/+-]{0,127}$`)
)

// ValidateToolchain checks a toolchain entry.
func ValidateToolchain(t Toolchain) error {
	if !toolchainNameRe.MatchString(t.Name) {
		return fmt.Errorf("invalid toolchain name %q", t.Name)
	}
	if t.Version != "" && !versionRe.MatchString(t.Version) {
		return fmt.Errorf("toolchain %s: invalid version %q", t.Name, t.Version)
	}
	for _, p := range t.Path {
		if !pathRe.MatchString(p) {
			return fmt.Errorf("toolchain %s: invalid path %q", t.Name, p)
		}
	}
	if !t.Custom() {
		if t.Install != "" || t.Nix != "" {
			return fmt.Errorf("toolchain %s is built in; drop install/nix", t.Name)
		}
		return nil
	}
	switch {
	case t.Install == "" && t.Nix == "":
		return fmt.Errorf("custom toolchain %s needs its nixpkgs attribute (nix), or install commands from its primary source (supported built-ins: %s)",
			t.Name, strings.Join(ToolchainNames(), ", "))
	case t.Nix != "" && !nixAttrRe.MatchString(t.Nix):
		return fmt.Errorf("toolchain %s: invalid nixpkgs attribute %q", t.Name, t.Nix)
	case t.Install != "" && !strings.HasPrefix(t.Source, "https://"):
		return fmt.Errorf("toolchain %s: a custom install needs the https:// URL of its primary source", t.Name)
	case strings.Contains(t.Install, heredocEnd):
		return fmt.Errorf("toolchain %s: install must not contain %s", t.Name, heredocEnd)
	}
	return nil
}

const heredocEnd = "PERSONA_TOOLCHAIN_END"

// ParseToolchain parses a built-in "name" or "name@version".
func ParseToolchain(s string) (Toolchain, error) {
	name, version, _ := strings.Cut(strings.TrimSpace(s), "@")
	t := Toolchain{Name: strings.ToLower(name), Version: version}
	return t, ValidateToolchain(t)
}

func (t Toolchain) String() string {
	if t.Version == "" {
		return t.Name
	}
	return t.Name + "@" + t.Version
}

// normalizeToolchains drops invalid entries and duplicates (first wins), adds
// required toolchains, and orders them for installation.
func normalizeToolchains(in []Toolchain) []Toolchain {
	byName := map[string]Toolchain{}
	var order []string
	add := func(t Toolchain) {
		t.Name = strings.ToLower(strings.TrimSpace(t.Name))
		t.Version = strings.TrimSpace(t.Version)
		if ValidateToolchain(t) != nil {
			return
		}
		if _, ok := byName[t.Name]; !ok {
			byName[t.Name] = t
			order = append(order, t.Name)
		}
	}
	for _, t := range in {
		add(t)
	}
	for _, name := range append([]string{}, order...) {
		info, _ := toolchainInfo(name)
		for _, r := range info.Requires {
			add(Toolchain{Name: r, Reason: "required by " + name})
		}
	}
	rank := map[string]int{}
	for i, t := range ToolchainCatalog {
		rank[t.Name] = i
	}
	custom := len(ToolchainCatalog)
	sort.SliceStable(order, func(i, j int) bool {
		ri, ok := rank[order[i]]
		if !ok {
			ri = custom
		}
		rj, ok := rank[order[j]]
		if !ok {
			rj = custom
		}
		return ri < rj
	})
	out := make([]Toolchain, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

// impliedPlugins are plugins the toolchains bring along.
func impliedPlugins(ts []Toolchain) []Plugin {
	var out []Plugin
	for _, t := range ts {
		info, _ := toolchainInfo(t.Name)
		out = append(out, info.Plugins...)
	}
	return out
}

// CustomInstalls lists custom toolchains whose install commands were written
// from a web page, for review before building.
func CustomInstalls(ts []Toolchain) []Toolchain {
	var out []Toolchain
	for _, t := range ts {
		if t.Install != "" {
			out = append(out, t)
		}
	}
	return out
}
