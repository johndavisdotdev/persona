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
		Use:     "delete <name>...",
		Aliases: []string{"rm"},
		Short:   "Delete personas (definition and images)",
		Long: `Deletes personas: their definition and images. Spawns of a
deleted persona are not affected: they keep running, and can be stopped,
started, saved and killed as before.`,
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
			for _, name := range args {
				if !force {
					ok, err := confirm(fmt.Sprintf("Delete persona %q? This removes its definition and image; its spawns keep running.", name), false)
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
				if !keepImage {
					// Spawns keep their own copy: Docker containers hold the
					// image's layers, microVM spawns hard link their disk.
					if dockerOK {
						for _, img := range []string{persona.ImageFor(name), docker.BaseImageFor(name)} {
							if err := docker.RemoveImage(ctx, img); err != nil {
								ui.Warning(out, err.Error())
							}
						}
					}
					if f, ok := instance.Current().(instance.ImageForgetter); ok {
						if err := f.ForgetImages(name); err != nil {
							ui.Warning(out, err.Error())
						}
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
	cmd.Flags().BoolVarP(&force, "force", "f", false, "skip confirmation")
	cmd.Flags().BoolVar(&keepImage, "keep-image", false, "keep the docker image")
	return cmd
}
