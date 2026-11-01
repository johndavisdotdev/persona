// Package cmd implements the persona CLI.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/ui"
)

// Version is set at build time with -ldflags "-X github.com/johndavisdev/persona/cmd.Version=..."
var Version = "dev"

var cfgFile string

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "persona",
		Short: "Build, spawn, and multiplex containerized Claude Code personas",
		Long: `Build, spawn, and multiplex containerized Claude Code personas: minimal Nix-built
images with the plugins, skills, and MCP servers a role needs already installed.`,
		Example: `  # design a persona with AI (interactive wizard)
  persona new
  # start a container and attach to Claude Code
  persona spawn reviewer -a
  # multiplex running personas in split panes and tabs
  persona control-center
  # give every persona a plugin
  persona base "All personas should have the Linear plugin installed"`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			h, _ := cmd.Flags().GetString("home")
			if h != "" {
				// Helpers persona starts (the broker, __vm-attach) inherit it.
				os.Setenv("PERSONA_HOME", h)
			}
			return config.Init(cfgFile, h)
		},
	}
	root.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default $PERSONA_HOME/config.yaml)")
	root.PersistentFlags().String("home", "", "persona state directory (default: OS config dir/persona)")
	root.PersistentFlags().Bool("no-color", false, "disable colors and styling (also honors NO_COLOR=1)")

	root.AddCommand(
		newListCmd(),
		newNewCmd(),
		newDeleteCmd(),
		newUpdateCmd(),
		newEditCmd(),
		newCustomizeCmd(),
		newSpawnCmd(),
		newPsCmd(),
		newAttachCmd(),
		newStopCmd(),
		newStartCmd(),
		newSaveCmd(),
		newKillCmd(),
		newControlCenterCmd(),
		newManageCmd(),
		newBuildCmd(),
		newBaseCmd(),
		newEditBaseCmd(),
		newConfigureCmd(),
		newAuthCmd(),
		newBrokerCmd(),
		newMessageCmd(),
		newExecCmd(),
		newServerCmd(),
		newVMAttachCmd(),
		newVMRshCmd(),
		newNSCmd(),
		newFWCmd(),
	)
	styleHelp(root)
	return root
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	noColor := colorDisabled(os.Args[1:])
	if noColor {
		// Bubble Tea and huh read NO_COLOR themselves.
		_ = os.Setenv("NO_COLOR", "1")
	}
	stdout, stderr := styledOutput(os.Stdout, noColor), styledOutput(os.Stderr, noColor)
	root := newRoot()
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		var code exitCode
		if errors.As(err, &code) { // persona exec: the command's own status
			return int(code)
		}
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, ui.Dim.Render("cancelled"))
			return 130
		}
		fmt.Fprintln(stderr, ui.ErrS.Render("error: ")+err.Error())
		return 1
	}
	return 0
}
