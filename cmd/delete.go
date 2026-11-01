package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

func newDeleteCmd() *cobra.Command {
	var force, keepImage bool
	cmd := &cobra.Command{
		Use:               "delete <name>...",
		Aliases:           []string{"rm"},
		Short:             "Delete personas (definition, Dockerfile, and image)",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completePersonas,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			for _, name := range args {
				if _, err := persona.Load(name); err != nil {
					return err
				}
			}
			dockerOK := docker.Available(ctx) == nil
			rt := instance.Current()
			rtOK := rt.Available(ctx) == nil
			for _, name := range args {
				var cs []instance.Container
				if rtOK {
					var err error
					if cs, err = rt.List(ctx, name); err != nil {
						return err
					}
				}
				if len(cs) > 0 && !force {
					return fmt.Errorf("persona %q has %d instance(s); stop them first or pass --force", name, len(cs))
				}
				if !force {
					ok, err := confirm(fmt.Sprintf("Delete persona %q? This removes its Dockerfile and image.", name), false)
					if err != nil {
						return err
					}
					if !ok {
						if !ui.Interactive() {
							return fmt.Errorf("refusing to delete %q without --force in non-interactive mode", name)
						}
						fmt.Fprintln(out, ui.Dim.Render("skipped "+name))
						continue
					}
				}
				for _, c := range cs {
					if err := rt.Remove(ctx, c.Name); instance.IsKept(err) {
						ui.Warning(out, err.Error())
					} else if err != nil {
						return err
					}
					ui.Step(out, "removed instance "+c.Name)
				}
				if dockerOK && !keepImage {
					if err := docker.RemoveImage(ctx, persona.ImageFor(name)); err != nil {
						ui.Warning(out, err.Error())
					}
				}
				if err := persona.Delete(name); err != nil {
					return err
				}
				ui.Success(out, "Deleted "+name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "skip confirmation and remove running instances")
	cmd.Flags().BoolVar(&keepImage, "keep-image", false, "keep the docker image")
	return cmd
}
