package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

func newUpdateCmd() *cobra.Command {
	var (
		description                                      string
		edits                                            componentEdits
		redesign, noExa, force, noBuild, refresh, dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update a persona definition and rebuild its image",
		Long: `Update a persona's definition. Changing --description (or passing --redesign)
asks Claude to revise the persona; the other flags make precise edits without AI.
The image definition (flake.nix, the setup scripts, settings.json) is
regenerated and the image rebuilt. A hand-edited file is never overwritten
unless --force is given. Customized and saved personas can't be updated.`,
		Example: `  persona update data-eng --description "now also owns the Kafka pipelines"
  persona update data-eng --add-plugin commit-commands@claude-plugins-official
  persona update data-eng --add-marketplace someone/their-marketplace --add-plugin tool@their-marketplace
  persona update data-eng --remove-mcp github --model sonnet --add-env GITHUB_TOKEN`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completePersonas,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			p, err := persona.Load(args[0])
			if err != nil {
				return err
			}
			if p.Frozen() {
				return persona.ErrFrozen(p)
			}
			if !force {
				edited, err := p.EditedFiles()
				if err != nil {
					return err
				}
				if len(edited) > 0 {
					return fmt.Errorf("%s for %q was hand-edited; updating would overwrite it. Use `persona edit %s`, or pass --force", strings.Join(edited, " and "), p.Name, p.Name)
				}
			}

			if cmd.Flags().Changed("description") || redesign {
				desc := p.Description
				if cmd.Flags().Changed("description") {
					desc = strings.TrimSpace(description)
				}
				useExa := !noExa && viper.GetString(config.KeyExaAPIKey) != ""
				fmt.Fprintln(out, ui.Heading.Render("Revising "+p.Name))
				np, err := newDesigner(useExa, out).Design(ctx, p.Name, desc, p)
				if err != nil {
					return err
				}
				if err := reviewCustomInstalls(np, out, false); err != nil {
					return err
				}
				p = np
			}

			edits.any(cmd.Flags())
			if err := edits.apply(ctx, p, out); err != nil {
				return err
			}
			p.Normalize()

			if dryRun {
				def, err := persona.Render(p)
				if err != nil {
					return err
				}
				fmt.Fprintln(out, ui.RenderPersona(p))
				for _, f := range persona.EditableFiles {
					fmt.Fprintln(out, ui.Heading.Render(f))
					fmt.Fprintln(out, def.Files[f])
				}
				return nil
			}
			if _, err := p.Generate(true); err != nil {
				return err
			}
			fmt.Fprintln(out, ui.RenderPersona(p))
			ui.Success(out, "Updated "+p.Name)
			if noBuild {
				return nil
			}
			return buildImage(ctx, p.Name, refresh, out)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&description, "description", "d", "", "new description; Claude revises the persona to match")
	f.BoolVar(&redesign, "redesign", false, "ask Claude to redesign the persona from its current description")
	f.BoolVar(&noExa, "no-exa", false, "disable Exa web search during redesign")
	edits.register(f)
	f.BoolVar(&force, "force", false, "overwrite hand-edited flake.nix, setup scripts or settings.json")
	f.BoolVar(&noBuild, "no-build", false, "do not rebuild the image")
	f.BoolVar(&refresh, "refresh", false, "update flake.lock to the newest nixpkgs when rebuilding")
	f.BoolVar(&dryRun, "dry-run", false, "print the resulting definition and generated files without saving")
	return cmd
}
