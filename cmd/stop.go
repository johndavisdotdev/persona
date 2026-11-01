package cmd

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/ui"
)

func newStopCmd() *cobra.Command {
	var all, keep, force bool
	cmd := &cobra.Command{
		Use:   "stop <container|instance|persona>...",
		Short: "Stop spawned instances",
		Long: `Stops (and by default removes) persona instances. A persona name stops all
of its instances; --all stops every instance.

A microVM instance saves its work first: commits land on its branch and
uncommitted changes are copied into its worktree. Removing it deletes the
worktree only if that has no uncommitted changes; the branch is always kept.`,
		ValidArgsFunction: completeContainers,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			if !all && len(args) == 0 {
				return errors.New("name a container, instance, or persona (or pass --all)")
			}
			rt := instance.Current()
			if force {
				ctx = instance.WithForce(ctx)
			}
			if err := rt.Available(ctx); err != nil {
				return err
			}
			var targets []instance.Container
			if all {
				cs, err := rt.List(ctx, "")
				if err != nil {
					return err
				}
				targets = cs
			}
			for _, ref := range args {
				cs, err := instance.Resolve(ctx, rt, ref)
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
					if err := rt.Stop(ctx, c.Name); err != nil {
						return err
					}
				}
				if !keep {
					if err := rt.Remove(ctx, c.Name); instance.IsKept(err) {
						ui.Warning(out, err.Error())
					} else if err != nil {
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
	cmd.Flags().BoolVar(&all, "all", false, "stop all persona instances")
	cmd.Flags().BoolVar(&force, "force", false, "stop microVMs without saving their work (if they no longer respond)")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep stopped instances (and their worktrees) instead of removing them")
	return cmd
}
