package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"

	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

// propagateOpts controls how base changes reach existing personas.
type propagateOpts struct {
	yes, noBuild, build bool
}

func (o *propagateOpts) register(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "apply without confirmation")
	cmd.Flags().BoolVar(&o.build, "build", false, "rebuild affected images without prompting")
	cmd.Flags().BoolVar(&o.noBuild, "no-build", false, "regenerate Dockerfiles but do not rebuild images")
}

func newBaseCmd() *cobra.Command {
	var (
		edits        componentEdits
		prop         propagateOpts
		clear, noExa bool
	)
	cmd := &cobra.Command{
		Use:   `base ["plain-language requirement"]`,
		Short: "Show or change the base requirements shared by all personas",
		Long: `The base is a set of plugins, MCP servers, skills, packages, env vars, and
instructions merged into EVERY persona's image (a persona's own entries win on
name collisions). It lives in $PERSONA_HOME/base.yaml.

With no arguments, shows the current base. With a plain-language requirement,
Claude interprets it against the real marketplaces and updates the base. The
--add-*/--remove-* flags make precise edits without AI. After a change, every
persona's Dockerfile is regenerated (hand-edited ones are skipped with a
warning) and the images are rebuilt.`,
		Example: `  persona base
  persona base "All personas should have the Linear plugin from the default anthropic marketplace installed."
  persona base --add-plugin linear@claude-plugins-official
  persona base --remove-plugin linear --add-apt jq
  persona edit-base`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			base, err := persona.LoadBase()
			if err != nil {
				return err
			}
			requirement := strings.TrimSpace(strings.Join(args, " "))
			hasEdits := edits.any(cmd.Flags())

			if requirement == "" && !hasEdits && !clear {
				showBase(out, base)
				return nil
			}

			next := *base
			switch {
			case clear:
				next = persona.Base{}
			case requirement != "":
				if viper.GetString(config.KeyAnthropicAPIKey) == "" {
					return errors.New("ANTHROPIC_API_KEY is not set; interpreting a requirement needs it (or use --add-plugin etc.)")
				}
				useExa := !noExa && viper.GetString(config.KeyExaAPIKey) != ""
				fmt.Fprintln(out, ui.Heading.Render("Interpreting requirement"))
				nb, change, err := newDesigner(useExa, out).DesignBase(ctx, requirement, base)
				if err != nil {
					return err
				}
				next = *nb
				if change != "" {
					fmt.Fprintln(out, ui.Dim.Render("  → "+change))
				}
			}
			if hasEdits {
				p := next.AsPersona()
				if err := edits.apply(ctx, p, out); err != nil {
					return err
				}
				next.SetFromPersona(p)
			}

			fmt.Fprintln(out)
			showBase(out, &next)
			ok := prop.yes
			if !ok {
				if ok, err = confirm("Apply this base to all personas?", true); err != nil {
					return err
				}
				if !ok && !ui.Interactive() {
					return errors.New("refusing to change the base non-interactively without --yes")
				}
			}
			if !ok {
				fmt.Fprintln(out, ui.Dim.Render("No changes made."))
				return nil
			}
			if err := next.Save(); err != nil {
				return err
			}
			ui.Success(out, "Saved "+persona.BasePath())
			return propagateBase(ctx, out, prop)
		},
	}
	edits.register(cmd.Flags())
	prop.register(cmd)
	cmd.Flags().BoolVar(&clear, "clear", false, "remove all base requirements")
	cmd.Flags().BoolVar(&noExa, "no-exa", false, "disable Exa web search while interpreting")
	return cmd
}

func newEditBaseCmd() *cobra.Command {
	var (
		prop     propagateOpts
		settings bool
	)
	cmd := &cobra.Command{
		Use:   "edit-base",
		Short: "Hand-edit the base requirements (base.yaml) or base settings (--settings)",
		Long: `Opens base.yaml (components merged into every persona) in your editor. With
--settings, opens base-settings.json instead: the ~/.claude/settings.json content
every persona's settings.json starts from (marketplaces, enabled plugins, model,
permissions, theme, …). It is seeded from the built-in default on first edit.
Saving regenerates every persona's files that were not hand-edited.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			path, validate := persona.BasePath(), func() error { return validateBaseFile(persona.BasePath()) }
			if settings {
				if err := persona.SeedBaseSettings(); err != nil {
					return err
				}
				path = persona.BaseSettingsPath()
				validate = func() error {
					if err := validateJSONFile(path); err != nil {
						return err
					}
					_, err := persona.RenderSettings(&persona.Persona{Name: "probe"})
					return err
				}
			} else if _, err := os.Stat(path); os.IsNotExist(err) {
				if err := (&persona.Base{}).Save(); err != nil {
					return err
				}
			}
			changed, err := editLoop(cmd, path, validate)
			if err != nil || !changed {
				return err
			}
			ui.Success(out, "Saved "+path)
			prop.yes = true
			return propagateBase(ctx, out, prop)
		},
	}
	cmd.Flags().BoolVar(&settings, "settings", false, "edit base-settings.json instead of base.yaml")
	cmd.Flags().BoolVar(&prop.build, "build", false, "rebuild affected images without prompting")
	cmd.Flags().BoolVar(&prop.noBuild, "no-build", false, "regenerate persona files but do not rebuild images")
	return cmd
}

func validateBaseFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var base persona.Base
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&base); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid base.yaml: %w", err)
	}
	probe := &persona.Persona{Name: "probe"}
	if _, err := persona.RenderDockerfile(probe); err != nil {
		return err
	}
	_, err = persona.RenderSettings(probe)
	return err
}

func showBase(out io.Writer, b *persona.Base) {
	if bs, err := persona.LoadBaseSettings(); err != nil {
		ui.Warning(out, err.Error())
	} else {
		src := persona.BaseSettingsPath()
		if _, err := os.Stat(src); err != nil {
			src = "built-in default"
		}
		plugins, mps := persona.SettingsPlugins(bs)
		fmt.Fprintln(out, ui.Heading.Render("Base settings.json")+ui.Dim.Render(" ("+src+"; edit with `persona edit-base --settings`)"))
		if m, ok := bs["model"].(string); ok {
			fmt.Fprintln(out, "  model: "+m)
		}
		if len(mps) > 0 {
			fmt.Fprintln(out, "  marketplaces: "+strings.Join(mps, ", "))
		}
		if len(plugins) > 0 {
			fmt.Fprintln(out, "  plugins: "+strings.Join(plugins, ", "))
		}
		fmt.Fprintln(out)
	}
	if b.Empty() {
		fmt.Fprintln(out, ui.Dim.Render("The base is empty: personas get only their own components."))
		fmt.Fprintln(out, ui.Dim.Render(`Add one with: persona base "All personas should have ..."`))
		return
	}
	p := b.AsPersona()
	p.Name = "base · applied to every persona"
	if len(b.Requirements) > 0 {
		p.Summary = "Requirements:\n  - " + strings.Join(b.Requirements, "\n  - ")
	}
	fmt.Fprintln(out, ui.RenderPersona(p))
	if b.Instructions != "" {
		fmt.Fprintln(out, ui.Heading.Render("Instructions (appended to every CLAUDE.md)"))
		fmt.Fprintln(out, b.Instructions)
	}
}

// propagateBase regenerates every persona's Dockerfile and optionally rebuilds.
func propagateBase(ctx context.Context, out io.Writer, o propagateOpts) error {
	ps, errs := persona.List()
	for _, e := range errs {
		ui.Warning(out, e.Error())
	}
	if len(ps) == 0 {
		return nil
	}
	var changed []string
	for _, p := range ps {
		before := readGenerated(p.Name)
		skipped, err := p.Generate(false)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		warnSkipped(out, p.Name, skipped)
		if readGenerated(p.Name) != before {
			changed = append(changed, p.Name)
			ui.Step(out, "regenerated "+p.Name)
		}
	}
	if len(changed) == 0 || o.noBuild {
		if len(changed) > 0 {
			fmt.Fprintf(out, "Rebuild later with %s\n", ui.Bold.Render("persona build "+strings.Join(changed, " ")))
		}
		return nil
	}
	build := o.build
	if !build {
		var err error
		if build, err = confirm(fmt.Sprintf("Rebuild %d persona image(s) now?", len(changed)), true); err != nil {
			return err
		}
	}
	if !build {
		fmt.Fprintf(out, "Rebuild later with %s\n", ui.Bold.Render("persona build "+strings.Join(changed, " ")))
		return nil
	}
	if err := docker.Available(ctx); err != nil {
		return err
	}
	for _, n := range changed {
		if err := buildImage(ctx, n, false, out); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, ui.Dim.Render("Running containers keep their old image; respawn them to pick up the base."))
	return nil
}

func readGenerated(name string) string {
	d, _ := os.ReadFile(persona.DockerfilePath(name))
	st, _ := os.ReadFile(persona.SettingsPath(name))
	return string(d) + "\x00" + string(st)
}
