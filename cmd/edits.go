package cmd

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/ai"
	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

// componentEdits are precise, AI-free edits shared by `update` and `base`.
type componentEdits struct {
	addPlugins, rmPlugins, addMarketplaces, rmMCP []string
	addEnv, rmEnv, addApt, rmApt                  []string
	addToolchains, rmToolchains                   []string
	addHarnessPkgs, rmHarnessPkgs                 []string
	model, harness                                string
	modelSet, harnessSet                          bool
	// base is set when editing the base requirements, whose package list is
	// absolute rather than relative to it.
	base bool
}

func (e *componentEdits) register(f *pflag.FlagSet) {
	f.StringSliceVar(&e.addPlugins, "add-plugin", nil, "add plugin (name@marketplace)")
	f.StringSliceVar(&e.rmPlugins, "remove-plugin", nil, "remove plugin (name or name@marketplace)")
	f.StringSliceVar(&e.addMarketplaces, "add-marketplace", nil, "make a marketplace (owner/repo or URL) available to --add-plugin")
	f.StringSliceVar(&e.rmMCP, "remove-mcp", nil, "remove MCP server by name")
	f.StringSliceVar(&e.addEnv, "add-env", nil, "forward a host env var into spawned containers")
	f.StringSliceVar(&e.rmEnv, "remove-env", nil, "stop forwarding a host env var")
	f.StringSliceVar(&e.addApt, "add-package", nil, "add a nixpkgs package (attribute name, e.g. ffmpeg)")
	f.StringSliceVar(&e.rmApt, "remove-package", nil, "remove a nixpkgs package (or a legacy apt package)")
	f.StringSliceVar(&e.addToolchains, "add-toolchain", nil, "add a language toolchain (name or name@version; one of "+strings.Join(persona.ToolchainNames(), ", ")+")")
	f.StringSliceVar(&e.rmToolchains, "remove-toolchain", nil, "remove a language toolchain by name")
	f.StringVar(&e.model, "model", "", "harness model: opus, sonnet or haiku for Claude Code; an OpenRouter model id (e.g. anthropic/claude-sonnet-4.5) for pi; empty for default")
	f.StringVar(&e.harness, "harness", "", "coding agent for spawns: "+strings.Join(persona.Harnesses, " or ")+" (empty: the default)")
	f.StringSliceVar(&e.addHarnessPkgs, "add-harness-package", nil, "add a pi package (npm:<name>)")
	f.StringSliceVar(&e.rmHarnessPkgs, "remove-harness-package", nil, "remove a pi package, including a default one (npm:<name>)")
}

func (e *componentEdits) any(f *pflag.FlagSet) bool {
	e.modelSet = f.Changed("model")
	e.harnessSet = f.Changed("harness")
	return e.modelSet || e.harnessSet || len(e.addHarnessPkgs)+len(e.rmHarnessPkgs) > 0 || len(e.addPlugins)+len(e.rmPlugins)+len(e.addMarketplaces)+len(e.rmMCP)+
		len(e.addEnv)+len(e.rmEnv)+len(e.addApt)+len(e.rmApt)+len(e.addToolchains)+len(e.rmToolchains) > 0
}

func (e *componentEdits) apply(ctx context.Context, p *persona.Persona, out io.Writer) error {
	if len(e.addPlugins) > 0 || len(e.addMarketplaces) > 0 {
		cat := ai.NewCatalog()
		sources := viper.GetStringSlice(config.KeyMarketplaces)
		for _, m := range p.Marketplaces {
			sources = append(sources, m.Source)
		}
		sources = append(sources, e.addMarketplaces...)
		for _, err := range cat.LoadAll(ctx, sources) {
			ui.Warning(out, err.Error())
		}
		for _, id := range e.addPlugins {
			n, mp, ok := strings.Cut(id, "@")
			if !ok {
				return fmt.Errorf("--add-plugin %q must be name@marketplace", id)
			}
			if !cat.Has(n, mp) {
				return fmt.Errorf("plugin %q not found; add its marketplace with --add-marketplace owner/repo", id)
			}
			m, _ := cat.Get(mp)
			p.Marketplaces = append(p.Marketplaces, persona.Marketplace{Name: m.Name, Source: m.Source})
			p.Plugins = append(p.Plugins, persona.Plugin{Name: n, Marketplace: mp})
		}
	}
	for _, id := range e.rmPlugins {
		before := len(p.Plugins)
		p.Plugins = slices.DeleteFunc(p.Plugins, func(x persona.Plugin) bool { return x.ID() == id || x.Name == id })
		if len(p.Plugins) == before {
			return fmt.Errorf("%s has no plugin %q", p.Name, id)
		}
	}
	pruneMarketplaces(p)
	for _, n := range e.rmMCP {
		before := len(p.MCPServers)
		p.MCPServers = slices.DeleteFunc(p.MCPServers, func(x persona.MCPServer) bool { return x.Name == n })
		if len(p.MCPServers) == before {
			return fmt.Errorf("%s has no MCP server %q", p.Name, n)
		}
	}
	p.Env = append(p.Env, e.addEnv...)
	p.Env = slices.DeleteFunc(p.Env, func(s string) bool { return slices.Contains(e.rmEnv, s) })
	for _, pk := range e.addApt {
		if err := persona.ValidateNixPackage(pk); err != nil {
			return err
		}
	}
	p.Packages = append(p.Packages, e.addApt...)
	p.Packages = slices.DeleteFunc(p.Packages, func(s string) bool { return slices.Contains(e.rmApt, s) })
	p.AptPackages = slices.DeleteFunc(p.AptPackages, func(s string) bool { return slices.Contains(e.rmApt, s) })
	if e.harnessSet {
		if err := persona.ValidateHarness(e.harness); err != nil {
			return err
		}
		p.Harness = e.harness
	}
	for _, pk := range append(append([]string{}, e.addHarnessPkgs...), e.rmHarnessPkgs...) {
		if err := persona.ValidateHarnessPackage(pk); err != nil || strings.HasPrefix(pk, "-") {
			return fmt.Errorf("invalid pi package %q (use npm:<name>)", pk)
		}
	}
	for _, pk := range e.addHarnessPkgs {
		p.HarnessPackages = slices.DeleteFunc(p.HarnessPackages, func(x string) bool { return x == "-"+pk })
		if !slices.Contains(p.HarnessPackages, pk) {
			p.HarnessPackages = append(p.HarnessPackages, pk)
		}
	}
	for _, pk := range e.rmHarnessPkgs {
		had := slices.Contains(p.HarnessPackages, pk)
		p.HarnessPackages = slices.DeleteFunc(p.HarnessPackages, func(x string) bool { return x == pk })
		if !had && !e.base && !slices.Contains(p.HarnessPackages, "-"+pk) {
			p.HarnessPackages = append(p.HarnessPackages, "-"+pk) // drop the base's
		}
	}
	p.Toolchains = slices.DeleteFunc(p.Toolchains, func(t persona.Toolchain) bool { return slices.Contains(e.rmToolchains, t.Name) })
	for _, s := range e.addToolchains {
		t, err := persona.ParseToolchain(s)
		if err != nil {
			return err
		}
		if t.Custom() {
			return fmt.Errorf("toolchain %q isn't built in; describe it with `persona update --description` or add it to persona.yaml with `persona edit --spec`", t.Name)
		}
		// Replace an existing entry so --add-toolchain dotnet@8 changes the version.
		p.Toolchains = slices.DeleteFunc(p.Toolchains, func(x persona.Toolchain) bool { return x.Name == t.Name })
		p.Toolchains = append(p.Toolchains, t)
	}
	if e.modelSet {
		p.Model = e.model
	}
	p.Normalize()
	return nil
}
