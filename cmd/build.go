package cmd

import (
	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/persona"
)

func newBuildCmd() *cobra.Command {
	var refresh bool
	cmd := &cobra.Command{
		Use:   "build <name>...",
		Short: "(Re)build persona images from their flakes and setup scripts",
		Long: `Builds each persona's image in two stages: Nix builds flake.nix into
persona/NAME:base (with the host's Nix, or in a container when there is none),
then setup-root.sh and setup-user.sh run in a container from it, which is
committed as persona/NAME:latest.

--refresh first updates flake.lock to the newest nixpkgs.`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completePersonas,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, n := range args {
				p, err := persona.Load(n)
				if err != nil {
					return err
				}
				if p.Frozen() {
					return persona.ErrFrozen(p)
				}
				if err := buildImage(cmd.Context(), n, refresh, cmd.OutOrStdout()); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "update flake.lock to the newest nixpkgs first")
	return cmd
}
