package ai

import (
	"slices"
	"strings"
	"testing"

	"github.com/johndavisdev/persona/internal/persona"
)

func toolchainSet(p *persona.Persona) string {
	var s []string
	for _, t := range p.Toolchains {
		s = append(s, t.String())
	}
	slices.Sort(s)
	return strings.Join(s, " ")
}

func TestEnsureMentioned(t *testing.T) {
	cases := []struct {
		desc string
		have []persona.Toolchain
		want string // toolchains after, sorted
		apt  []string
	}{
		{"A senior .NET 8 expert who maintains ASP.NET Core services", nil, "dotnet@8", nil},
		{"Rust systems programmer", nil, "rust", nil},
		{"Backend Go developer for our microservices", nil, "go", nil},
		{"Expert in Go and Rust.", nil, "go rust", nil},
		{"Writes services in Go 1.22", nil, "go@1.22", nil},
		{"Frontend React engineer building our website", nil, "playwright", nil},
		{"Data scientist on Python 3.11 with pandas", []persona.Toolchain{{Name: "python"}}, "python@3.11", nil},
		{"Kotlin Android dev with Gradle", nil, "java kotlin", nil},
		{"Phoenix LiveView developer", []persona.Toolchain{{Name: "elixir", Nix: "elixir", Source: "https://elixir-lang.org", Install: "x"}}, "elixir", nil},
		{"Maintains our Haskell compiler and some Zig tools", nil, "haskell zig", nil},
		{"Embedded C++ developer using CMake", nil, "", []string{"gcc", "cmake", "gdb", "clang-tools", "pkg-config"}},
		// Not language mentions.
		{"My go-to reviewer for documents and contracts", nil, "", nil},
		{"Go ahead and triage incidents; react to alerts quickly; respect guardrails and OTP rules", nil, "", nil},
		{"Plans the Go-live of our CRM and writes swift responses to customers", nil, "", nil},
		{"Logistics analyst tracking cargo shipments; searches the web for rates", nil, "", nil},
	}
	for _, c := range cases {
		p := &persona.Persona{Toolchains: append([]persona.Toolchain{}, c.have...)}
		EnsureMentioned(c.desc, p)
		if got := toolchainSet(p); got != c.want {
			t.Errorf("%q: toolchains %q, want %q", c.desc, got, c.want)
		}
		if !slices.Equal(p.Packages, c.apt) {
			t.Errorf("%q: apt %v, want %v", c.desc, p.Packages, c.apt)
		}
		for _, tc := range p.Toolchains {
			if err := persona.ValidateToolchain(tc); err != nil {
				t.Errorf("%q: invalid toolchain added: %v", c.desc, err)
			}
		}
	}
}

func TestMissingToolchains(t *testing.T) {
	got := MissingToolchains("Frontend React engineer on a Rails app", []persona.Toolchain{{Name: "playwright"}})
	if len(got) != 1 || !strings.Contains(got[0], "toolchain ruby") {
		t.Errorf("MissingToolchains = %v", got)
	}
}

func TestDesignerPromptListsToolchains(t *testing.T) {
	for _, n := range persona.ToolchainNames() {
		if !strings.Contains(designerSystem, "  - "+n+": ") {
			t.Errorf("designer prompt lacks toolchain %s", n)
		}
	}
	if strings.Contains(designerSystem, "@TOOLCHAINS@") {
		t.Error("placeholder not replaced")
	}
}

func TestMentionedHarness(t *testing.T) {
	for desc, want := range map[string]string{
		"A reviewer that runs on Claude Code with the pr-review plugin": persona.HarnessClaude,
		"Use the pi coding agent with OpenRouter":                       persona.HarnessPi,
		"A data engineer for dbt and Snowflake":                         "",
		"Computes pi to many digits":                                    "",
	} {
		if got := MentionedHarness(desc); got != want {
			t.Errorf("%q: %q, want %q", desc, got, want)
		}
	}
	if !strings.Contains(designerSystem, "npm:pi-code") {
		t.Error("prompt lacks default pi packages")
	}
}
