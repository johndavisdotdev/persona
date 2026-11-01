package cmd

import (
	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/manage"
)

func newManageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "manage",
		Short: "Dashboard of every spawn's status: what it's doing, and what it needs from you",
		Long: `Shows every spawn and what its agent is doing, from the status stream each
harness reports (prompt submitted, turn finished, notifications, questions,
tasks completed), without attaching to their terminals.

Spawns that need you (a question, a permission prompt) are highlighted and
ring the terminal bell. Select one and press enter to type a reply into its
session; for a multiple-choice question, reply with the option's number.
Press a to attach to its session in full.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rt := instance.Current()
			if err := rt.Available(cmd.Context()); err != nil {
				return err
			}
			return manage.Run(cmd.Context(), rt)
		},
	}
}
