package cmd

import (
	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/persona"
)

func newBuildCmd() *cobra.Command {
	var noCache bool
	cmd := &cobra.Command{
		Use:               "build <name>...",
		Short:             "(Re)build persona images from their Dockerfiles",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completePersonas,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, n := range args {
				if _, err := persona.Load(n); err != nil {
					return err
				}
				if err := buildImage(cmd.Context(), n, noCache, cmd.OutOrStdout()); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "build without the docker layer cache")
	return cmd
}
