package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/tui"
)

func newControlCenterCmd() *cobra.Command {
	var skipPerms bool
	cmd := &cobra.Command{
		Use:     "control-center",
		Aliases: []string{"cc", "ui"},
		Short:   "TUI to multiplex spawned personas in split panes and tabs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := docker.Available(ctx); err != nil {
				return err
			}
			return tui.Run(ctx, func(ctx context.Context, name string, cols, rows int) (string, error) {
				return Spawn(ctx, name, SpawnOptions{SkipPermissions: skipPerms, Cols: cols, Rows: rows})
			})
		},
	}
	cmd.Flags().BoolVar(&skipPerms, "skip-permissions", false, "spawn Claude Code with --dangerously-skip-permissions")
	return cmd
}
