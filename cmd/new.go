package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"

	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

func newNewCmd() *cobra.Command {
	var (
		name, description, descFile, fromSpec string
		yes, ask, noBuild, noExa, noFix       bool
	)
	cmd := &cobra.Command{
		Use:   "new [name]",
		Short: "Design a new persona with AI (guided wizard)",
		Long: `Describe a persona in plain language. Claude interprets it, searches the
built-in plugin marketplaces (and, with EXA_API_KEY set, the web for niche
third-party marketplaces and MCP servers), and generates the image
definition: a Nix flake with the toolchains and packages, plus setup scripts
that install the harness and the chosen plugins, skills and MCP servers. Every language the description
mentions is installed. The image is then built so the persona can be spawned.

Only the name and description are asked for (when not given as arguments);
the design is accepted as-is and built. Pass --ask to choose whether to search
the web, review the selected components, and confirm before building.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				name = args[0]
			}
			if descFile != "" {
				b, err := os.ReadFile(descFile)
				if err != nil {
					return err
				}
				description = string(b)
			}
			if fromSpec != "" {
				return importSpec(cmd, fromSpec, name, noBuild, noFix)
			}
			useExa := !noExa && viper.GetString(config.KeyExaAPIKey) != ""
			interactive := ui.Interactive() && !yes
			ask = ask && interactive

			if interactive && (ask || name == "" || strings.TrimSpace(description) == "") {
				fmt.Fprintln(out, ui.Title.Render("✦ New persona"))
				if err := wizard(&name, &description, &useExa, ask && useExa); err != nil {
					return err
				}
			}
			name = strings.TrimSpace(name)
			description = strings.TrimSpace(description)
			if err := validateNewName(name); err != nil {
				return err
			}
			if description == "" {
				return errors.New("a persona description is required (--description or --description-file)")
			}
			if viper.GetString(config.KeyAnthropicAPIKey) == "" {
				return errors.New("no Anthropic API key: the persona designer needs one (run `persona configure`, or set ANTHROPIC_API_KEY)")
			}
			if !useExa && !noExa {
				ui.Warning(out, "no Exa API key: searching built-in marketplaces only (add one with `persona configure`)")
			}

			fmt.Fprintln(out, ui.Heading.Render("Designing "+name))
			p, err := newDesigner(useExa, out).Design(ctx, name, description, nil)
			if err != nil {
				return err
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, ui.RenderPersona(p))
			if err := reviewCustomInstalls(p, out, ask); err != nil {
				return err
			}

			if ask {
				if err := reviewComponents(p); err != nil {
					return err
				}
			}
			if err := p.Save(); err != nil {
				return err
			}
			if _, err := p.Generate(true); err != nil {
				return err
			}
			ui.Success(out, "Saved persona to "+persona.Dir(name))

			build := !noBuild
			if ask && build {
				if build, err = confirm("Build the image now?", true); err != nil {
					return err
				}
			}
			if build {
				if err := buildWithRepair(ctx, name, !noFix, out); err != nil {
					return fmt.Errorf("%w\nfix flake.nix or the setup scripts with `persona edit %s`, then run `persona build %s`", err, name, name)
				}
				fmt.Fprintf(out, "\nSpawn it with %s\n", ui.Bold.Render("persona spawn "+name+" --attach"))
			} else {
				fmt.Fprintf(out, "\nBuild later with %s\n", ui.Bold.Render("persona build "+name))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&name, "name", "n", "", "persona name")
	f.StringVarP(&description, "description", "d", "", "persona description")
	f.StringVar(&descFile, "description-file", "", "read the persona description from a file")
	f.BoolVar(&ask, "ask", false, "ask whether to search the web, review the selected components, and confirm before building")
	f.BoolVarP(&yes, "yes", "y", false, "never prompt (fail if the name or description is missing)")
	f.BoolVar(&noBuild, "no-build", false, "write the image definition but do not build it")
	f.BoolVar(&noFix, "no-fix", false, "if the build fails, don't let Claude fix flake.nix and rebuild")
	f.BoolVar(&noExa, "no-exa", false, "disable Exa web search for third-party marketplaces")
	f.StringVar(&fromSpec, "from-spec", "", "create the persona from an existing persona.yaml (no AI); the name argument overrides the file's name")
	return cmd
}

func validateNewName(name string) error {
	if err := persona.ValidateName(name); err != nil {
		return err
	}
	if persona.Exists(name) {
		return fmt.Errorf("persona %q already exists (use `persona update %s`)", name, name)
	}
	return nil
}

func wizard(name, description *string, useExa *bool, exaAvailable bool) error {
	fields := []huh.Field{
		huh.NewInput().Title("Name").Description("Lowercase letters, digits, and dashes, e.g. data-engineer").
			Value(name).Validate(validateNewName),
		huh.NewText().Title("Describe the persona").
			Description("Role, domain, tools, and typical tasks. The more specific, the better the plugin selection.").
			Placeholder("A senior data engineer who works with dbt, Snowflake and Airflow, reviews SQL for performance...").
			Lines(6).Value(description).
			Validate(func(s string) error {
				if len(strings.Fields(s)) < 4 {
					return errors.New("give at least a sentence")
				}
				return nil
			}),
	}
	if exaAvailable {
		fields = append(fields, huh.NewConfirm().Title("Search the web (Exa) for niche third-party marketplaces?").Value(useExa))
	}
	err := huh.NewForm(huh.NewGroup(fields...)).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return context.Canceled
	}
	return err
}

// reviewComponents lets the user deselect proposed components.
func reviewComponents(p *persona.Persona) error {
	var opts []huh.Option[string]
	for _, x := range p.Plugins {
		opts = append(opts, huh.NewOption("plugin  "+x.ID(), "p:"+x.ID()).Selected(true))
	}
	for _, x := range p.MCPServers {
		opts = append(opts, huh.NewOption("mcp     "+x.Name, "m:"+x.Name).Selected(true))
	}
	for _, x := range p.Skills {
		opts = append(opts, huh.NewOption("skill   "+x.Name, "s:"+x.Name).Selected(true))
	}
	if len(opts) == 0 {
		return nil
	}
	var keep []string
	err := huh.NewMultiSelect[string]().
		Title("Keep these components?").
		Description("space to toggle, enter to confirm").
		Options(opts...).Value(&keep).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return context.Canceled
	}
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	var pl []persona.Plugin
	for _, x := range p.Plugins {
		if kept["p:"+x.ID()] {
			pl = append(pl, x)
		}
	}
	var mc []persona.MCPServer
	for _, x := range p.MCPServers {
		if kept["m:"+x.Name] {
			mc = append(mc, x)
		}
	}
	var sk []persona.Skill
	for _, x := range p.Skills {
		if kept["s:"+x.Name] {
			sk = append(sk, x)
		}
	}
	p.Plugins, p.MCPServers, p.Skills = pl, mc, sk
	pruneMarketplaces(p)
	return nil
}

// pruneMarketplaces drops marketplaces no remaining plugin uses.
func pruneMarketplaces(p *persona.Persona) {
	used := map[string]bool{}
	for _, x := range p.Plugins {
		used[x.Marketplace] = true
	}
	var mps []persona.Marketplace
	for _, m := range p.Marketplaces {
		if used[m.Name] {
			mps = append(mps, m)
		}
	}
	p.Marketplaces = mps
}

// importSpec creates a persona from a YAML definition without the AI designer.
func importSpec(cmd *cobra.Command, path, name string, noBuild, noFix bool) error {
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var p persona.Persona
	if err := yaml.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if name != "" {
		p.Name = name
	}
	if err := validateNewName(p.Name); err != nil {
		return err
	}
	p.CreatedAt, p.FileHashes, p.DockerfileHash, p.SettingsHash = time.Time{}, nil, "", ""
	if err := p.Save(); err != nil {
		return err
	}
	if _, err := p.Generate(true); err != nil {
		return err
	}
	fmt.Fprintln(out, ui.RenderPersona(&p))
	ui.Success(out, "Imported persona to "+persona.Dir(p.Name))
	if noBuild {
		return nil
	}
	return buildWithRepair(ctx, p.Name, !noFix, out)
}
