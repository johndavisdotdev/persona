package cmd

import (
	"github.com/spf13/cobra"
)

func newJoinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "join [container|instance|persona]",
		Short: "Attach to a running persona's Claude Code session",
		Long: `Attaches your terminal to the tmux session running Claude Code inside a
spawned container. Detach with Ctrl-b d; the session keeps running.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeContainers,
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			c, err := pickContainer(cmd.Context(), ref, true)
			if err != nil {
				return err
			}
			return attachTo(cmd.Context(), c.Name, cmd.OutOrStdout())
		},
	}
}
