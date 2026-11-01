package ai

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/johndavisdev/persona/internal/persona"
)

// A language mention in a persona description and what it requires. Every
// mention is installed (EnsureMentioned), whatever the designer chose.
type languageHint struct {
	toolchain string         // toolchain name
	re        *regexp.Regexp // the mention
	version   *regexp.Regexp // optional: captures a named version
	nix       string         // non-built-in languages: nixpkgs attribute
	pkgs      []string       // languages served by plain nixpkgs packages instead
	also      []string       // other toolchains the language needs
}

func re(s string) *regexp.Regexp { return regexp.MustCompile(s) }

var languageHints = []languageHint{
	// Built-in toolchains.
	{toolchain: "dotnet", re: re(`(?i)(\.net\b|\bc#|\bf#|\bdotnet\b|asp\.net|\bblazor\b|\bnuget\b|\bmaui\b|entity framework)`),
		version: re(`(?i)(?:\.net|dotnet)\s*(?:core\s*)?v?(\d{1,2}(?:\.\d)?)\b`)},
	{toolchain: "rust", re: re(`(?i)(\brust\b|rustacean)`), version: re(`(?i)\brust\s*v?(1\.\d{2,3})\b`)},
	{toolchain: "go", re: re(`(?i)(\bgolang\b|\bgo (developer|engineer|expert|programmer|backend|services?|modules?|code)\b|\b(written|writing|coding|program(ming)?|in) go\b)`),
		version: re(`(?i)\b(?:go|golang)\s*v?(1\.\d{1,2})\b`)},
	{toolchain: "java", re: re(`(?i)(\bjava\b|\bjdk\b|\bjvm\b|\bspring (boot|framework|developer|engineer)\b|\bgradle\b|\bmaven\b)`), version: re(`(?i)\b(?:java|jdk)\s*v?(\d{1,2})\b`)},
	{toolchain: "python", re: re(`(?i)(\bpython\b|\bdjango\b|\bfastapi\b|\bflask\b|\bpandas\b|\bjupyter\b|\bpytorch\b|\bnumpy\b)`),
		version: re(`(?i)\bpython\s*v?(3\.\d{1,2})\b`)},
	{toolchain: "node", re: re(`(?i)(\bnode(\.?js)?\b|\btypescript\b|\bjavascript\b|\bnpm\b|\bpnpm\b)`), version: re(`(?i)\bnode(?:\.?js)?\s*v?(\d{2})\b`)},
	{toolchain: "bun", re: re(`(?i)\bbun\b`)},
	{toolchain: "deno", re: re(`(?i)\bdeno\b`)},
	{toolchain: "playwright", re: re(`(\bReact\b|(?i)(\bweb ?(apps?|sites?|development|developers?|engineers?|ui|frontends?|pages?|services?)\b|\bfront[- ]?end\b|\bfull[- ]?stack\b|\breact(\.?js| native| developers?| engineers?| apps?| components?| hooks)\b|\bnext\.?js\b|\bvue(\.?js)?\b|\bnuxt\b|\bangular\b|\bsvelte(kit)?\b|\bhtml\b|\bcss\b|\btailwind\b|\bbrowsers?\b|\be2e\b|end[- ]to[- ]end|\bplaywright\b|\bui (developer|engineer)\b))`)},
	// Languages without a built-in installer come from nixpkgs.
	{toolchain: "kotlin", re: re(`(?i)\bkotlin\b`), nix: "kotlin", also: []string{"java"}},
	{toolchain: "scala", re: re(`(?i)\bscala\b`), nix: "scala-cli", also: []string{"java"}},
	{toolchain: "clojure", re: re(`(?i)\bclojure\b`), nix: "clojure", also: []string{"java"}},
	{toolchain: "zig", re: re(`(?i)\bzig\b`), nix: "zig"},
	{toolchain: "elixir", re: re(`(?i)(\belixir\b|\bphoenix\b|\bliveview\b)`), nix: "beamPackages.elixir"},
	{toolchain: "erlang", re: re(`(?i)\berlang\b`), nix: "beamPackages.erlang"},
	{toolchain: "gleam", re: re(`(?i)\bgleam\b`), nix: "gleam"},
	{toolchain: "haskell", re: re(`(?i)\bhaskell\b`), nix: "ghc"},
	{toolchain: "ocaml", re: re(`(?i)\bocaml\b`), nix: "ocaml"},
	{toolchain: "julia", re: re(`(?i)\bjulia\b`), nix: "julia"},
	{toolchain: "ruby", re: re(`(?i)(\bruby\b|\brails (app|apps|developer|engineer|codebase)\b)`), nix: "ruby"},
	{toolchain: "php", re: re(`(?i)(\bphp\b|\blaravel\b|\bsymfony\b)`), nix: "php"},
	{toolchain: "perl", re: re(`(?i)\bperl\b`), nix: "perl"},
	{toolchain: "lua", re: re(`(?i)\blua\b`), nix: "lua"},
	{toolchain: "dart", re: re(`(?i)\bdart\b`), nix: "dart"},
	{toolchain: "flutter", re: re(`(?i)\bflutter\b`), nix: "flutter"},
	{toolchain: "swift", re: re(`(\bSwiftUI\b|\b(in|with) Swift\b|\bSwift (developer|programmer|engineer|code|packages?|apps?|language)\b)`), nix: "swift"},
	{toolchain: "crystal", re: re(`(?i)\bcrystal lang|\bcrystal (developer|programmer|code)`), nix: "crystal"},
	{toolchain: "nim", re: re(`(?i)\bnim(lang)?\b`), nix: "nim"},
	{toolchain: "r", re: re(`(\bR (language|programmer|developer|scripts?|packages?)\b|\bRStudio\b|\btidyverse\b|\bggplot2?\b|\bCRAN\b)`), nix: "R"},
	// Plain nixpkgs packages (make is in every image).
	{toolchain: "c/c++", re: re(`(?i)(\bc\+\+|\bcpp\b|\bc programm|\bin c\b|\bcmake\b|\bclang\b|\bgcc\b|\bembedded c\b)`),
		pkgs: []string{"gcc", "cmake", "gdb", "clang-tools", "pkg-config"}},
}

// goCapital catches "Go" written as a proper noun ("expert in Go and Rust"),
// which the case-insensitive pattern leaves alone to avoid "go-to" and "go ahead".
var goCapital = re(`\bGo(\s+(\w+)|[.,;:!?)]|$)`)

func mentioned(h languageHint, text string) string {
	if m := h.re.FindString(text); m != "" {
		return m
	}
	if h.toolchain == "go" {
		for _, m := range goCapital.FindAllStringSubmatch(text, -1) {
			switch strings.ToLower(m[2]) {
			case "to", "ahead", "back", "through", "over", "live", "out", "for", "with", "on":
				continue
			}
			return m[0]
		}
	}
	return ""
}

// MissingToolchains lists languages the description mentions that ts lacks,
// each with the phrase that mentioned it.
func MissingToolchains(description string, ts []persona.Toolchain) []string {
	have := map[string]bool{}
	for _, t := range ts {
		have[t.Name] = true
	}
	if have["playwright"] {
		have["node"] = true // playwright brings Node.js
	}
	var out []string
	for _, h := range languageHints {
		if have[h.toolchain] || len(h.pkgs) > 0 {
			continue
		}
		if m := mentioned(h, description); m != "" {
			out = append(out, fmt.Sprintf("%q (toolchain %s)", m, h.toolchain))
		}
	}
	sort.Strings(out)
	return out
}

// EnsureMentioned adds every language the description mentions that p lacks:
// built-in toolchains (with the version named, if any), nixpkgs for the rest,
// nixpkgs packages for C/C++. It returns what it added.
func EnsureMentioned(description string, p *persona.Persona) []string {
	var added []string
	index := func(name string) int {
		return slices.IndexFunc(p.Toolchains, func(t persona.Toolchain) bool { return t.Name == name })
	}
	add := func(t persona.Toolchain) {
		if i := index(t.Name); i >= 0 {
			if t.Version != "" && p.Toolchains[i].Version == "" && !p.Toolchains[i].Custom() {
				p.Toolchains[i].Version = t.Version
				added = append(added, t.String())
			}
			return
		}
		if t.Name == "node" && index("playwright") >= 0 && t.Version == "" {
			return // playwright brings Node.js
		}
		p.Toolchains = append(p.Toolchains, t)
		added = append(added, t.String())
	}
	for _, h := range languageHints {
		m := mentioned(h, description)
		if m == "" {
			continue
		}
		reason := fmt.Sprintf("the description mentions %q", m)
		if len(h.pkgs) > 0 {
			before := len(p.Packages)
			for _, pkg := range h.pkgs {
				if !slices.Contains(p.Packages, pkg) {
					p.Packages = append(p.Packages, pkg)
				}
			}
			if len(p.Packages) > before {
				added = append(added, h.toolchain+" ("+strings.Join(h.pkgs, ", ")+")")
			}
			continue
		}
		t := persona.Toolchain{Name: h.toolchain, Reason: reason, Nix: h.nix}
		if h.version != nil {
			if v := h.version.FindStringSubmatch(description); v != nil {
				t.Version = v[1]
			}
		}
		add(t)
		for _, dep := range h.also {
			add(persona.Toolchain{Name: dep, Reason: "needed by " + h.toolchain})
		}
	}
	return added
}

var (
	claudeCodeRe = re(`(?i)\bclaude[ -]code\b`)
	piHarnessRe  = re(`(?i)(\bpi (harness|coding agent|agent)\b|\bpi\.dev\b|\bpi-coding-agent\b)`)
)

// MentionedHarness is the harness the description asks for, if any.
func MentionedHarness(description string) string {
	switch {
	case piHarnessRe.MatchString(description):
		return persona.HarnessPi
	case claudeCodeRe.MatchString(description):
		return persona.HarnessClaude
	}
	return ""
}
