package cmd

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/ui"
)

func newStopCmd() *cobra.Command {
	var all, keep bool
	cmd := &cobra.Command{
		Use:   "stop <container|instance|persona>...",
		Short: "Stop spawned containers",
		Long: `Stops (and by default removes) persona containers. A persona name stops all
of its instances; --all stops every persona container.`,
		ValidArgsFunction: completeContainers,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			if !all && len(args) == 0 {
				return errors.New("name a container, instance, or persona (or pass --all)")
			}
			if err := docker.Available(ctx); err != nil {
				return err
			}
			var targets []docker.Container
			if all {
				cs, err := docker.List(ctx, "")
				if err != nil {
					return err
				}
				targets = cs
			}
			for _, ref := range args {
				cs, err := docker.Resolve(ctx, ref)
				if err != nil {
					return err
				}
				targets = append(targets, cs...)
			}
			seen := map[string]bool{}
			for _, c := range targets {
				if seen[c.Name] {
					continue
				}
				seen[c.Name] = true
				if c.Running() {
					if err := docker.Stop(ctx, c.Name); err != nil {
						return err
					}
				}
				if !keep {
					if err := docker.Remove(ctx, c.Name); err != nil {
						return err
					}
				}
				ui.Success(out, "Stopped "+c.Name)
			}
			if len(seen) == 0 {
				ui.Step(out, "nothing to stop")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop all persona containers")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep stopped containers instead of removing them")
	return cmd
}
