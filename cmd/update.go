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
		description, baseImage                           string
		edits                                            componentEdits
		redesign, noExa, force, noBuild, noCache, dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update a persona definition and rebuild its image",
		Long: `Update a persona's definition. Changing --description (or passing --redesign)
asks Claude to revise the persona; the other flags make precise edits without AI.
The Dockerfile is regenerated and the image rebuilt. A hand-edited Dockerfile is
never overwritten unless --force is given.`,
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
				p = np
			}

			edits.any(cmd.Flags())
			if err := edits.apply(ctx, p, out); err != nil {
				return err
			}
			if cmd.Flags().Changed("base-image") {
				p.BaseImage = baseImage
			}
			p.Normalize()

			if dryRun {
				df, err := persona.RenderDockerfile(p)
				if err != nil {
					return err
				}
				st, err := persona.RenderSettings(p)
				if err != nil {
					return err
				}
				fmt.Fprintln(out, ui.RenderPersona(p))
				fmt.Fprintln(out, ui.Heading.Render("settings.json"))
				fmt.Fprintln(out, st)
				fmt.Fprintln(out, ui.Heading.Render("Dockerfile"))
				fmt.Fprintln(out, df)
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
			return buildImage(ctx, p.Name, noCache, out)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&description, "description", "d", "", "new description; Claude revises the persona to match")
	f.BoolVar(&redesign, "redesign", false, "ask Claude to redesign the persona from its current description")
	f.BoolVar(&noExa, "no-exa", false, "disable Exa web search during redesign")
	edits.register(f)
	f.StringVar(&baseImage, "base-image", "", "override the base image")
	f.BoolVar(&force, "force", false, "overwrite a hand-edited Dockerfile")
	f.BoolVar(&noBuild, "no-build", false, "do not rebuild the image")
	f.BoolVar(&noCache, "no-cache", false, "rebuild without the docker layer cache")
	f.BoolVar(&dryRun, "dry-run", false, "print the resulting definition and Dockerfile without saving")
	return cmd
}
