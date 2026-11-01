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
	model                                         string
	modelSet                                      bool
}

func (e *componentEdits) register(f *pflag.FlagSet) {
	f.StringSliceVar(&e.addPlugins, "add-plugin", nil, "add plugin (name@marketplace)")
	f.StringSliceVar(&e.rmPlugins, "remove-plugin", nil, "remove plugin (name or name@marketplace)")
	f.StringSliceVar(&e.addMarketplaces, "add-marketplace", nil, "make a marketplace (owner/repo or URL) available to --add-plugin")
	f.StringSliceVar(&e.rmMCP, "remove-mcp", nil, "remove MCP server by name")
	f.StringSliceVar(&e.addEnv, "add-env", nil, "forward a host env var into spawned containers")
	f.StringSliceVar(&e.rmEnv, "remove-env", nil, "stop forwarding a host env var")
	f.StringSliceVar(&e.addApt, "add-apt", nil, "add a Debian package")
	f.StringSliceVar(&e.rmApt, "remove-apt", nil, "remove a Debian package")
	f.StringVar(&e.model, "model", "", "Claude Code model inside the container (opus, sonnet, haiku, or a model id; empty for default)")
}

func (e *componentEdits) any(f *pflag.FlagSet) bool {
	e.modelSet = f.Changed("model")
	return e.modelSet || len(e.addPlugins)+len(e.rmPlugins)+len(e.addMarketplaces)+len(e.rmMCP)+
		len(e.addEnv)+len(e.rmEnv)+len(e.addApt)+len(e.rmApt) > 0
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
	p.AptPackages = append(p.AptPackages, e.addApt...)
	p.AptPackages = slices.DeleteFunc(p.AptPackages, func(s string) bool { return slices.Contains(e.rmApt, s) })
	if e.modelSet {
		p.Model = e.model
	}
	p.Normalize()
	return nil
}
