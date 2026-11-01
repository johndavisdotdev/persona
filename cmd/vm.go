package cmd

import (
	"log"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/instance"
)

// Hidden commands used by the firecracker runtime.

func newBrokerCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "broker",
		Short:  "Serve push and pull-request requests from microVM instances",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b := &instance.Broker{Idle: 10 * time.Minute, Log: log.New(os.Stderr, "persona broker: ", log.LstdFlags)}
			return b.Run(cmd.Context())
		},
	}
}

func newVMAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__vm-attach <instance>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return instance.AttachMain(cmd.Context(), args[0])
		},
	}
}

func newVMRshCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "__vm-rsh VSOCK USER HOST COMMAND...",
		Hidden:             true,
		DisableFlagParsing: true, // the arguments are rsync's
		Run: func(_ *cobra.Command, args []string) {
			os.Exit(instance.RshMain(args))
		},
	}
}
