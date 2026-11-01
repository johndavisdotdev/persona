package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/tui"
)

func newControlCenterCmd() *cobra.Command {
	var skipPerms bool
	var namespace string
	cmd := &cobra.Command{
		Use:     "control-center",
		Aliases: []string{"cc", "ui"},
		Short:   "TUI to multiplex spawned personas in split panes and tabs",
		Long: `A TUI to multiplex spawned personas in split panes and tabs. The sidebar
groups spawns by network namespace (see persona ns); with --namespace it
shows, and spawns into, only that namespace.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			rt := instance.Current()
			if err := rt.Available(ctx); err != nil {
				return err
			}
			if err := checkNamespaceFlag(ctx, rt, namespace); err != nil {
				return err
			}
			return tui.Run(ctx, func(ctx context.Context, name, ns string, cols, rows int) (string, error) {
				c, _, err := Spawn(ctx, name, SpawnOptions{SkipPermissions: skipPerms, Namespace: ns, Cols: cols, Rows: rows})
				return c, err
			}, namespace)
		},
	}
	cmd.Flags().BoolVar(&skipPerms, "skip-permissions", false, "spawn Claude Code with --dangerously-skip-permissions")
	cmd.Flags().StringVar(&namespace, "namespace", "", "show and spawn into only this namespace")
	_ = cmd.RegisterFlagCompletionFunc("namespace", completeNamespaceFlag)
	return cmd
}
