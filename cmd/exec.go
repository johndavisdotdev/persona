package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/instance"
)

// maxExecStdin bounds what persona exec forwards from a piped stdin.
const maxExecStdin = 16 << 20

// exitCode makes Execute exit with a command's own status, printing nothing.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func newExecCmd() *cobra.Command {
	var withStdin bool
	cmd := &cobra.Command{
		Use:   "exec [-i] <spawn> <command> [args...]",
		Short: "Run a command in a spawn, outside Claude Code",
		Long: `Runs a command inside a running spawn, beside its Claude Code session: as the
session's user, in its working directory, with the image's environment. The
command's stdout and stderr are printed as they were written to each, once
it finishes, and persona exits with the command's exit status (126/127 if it
couldn't be started, 128+N if a signal killed it). With -i, persona's stdin is
read to its end and passed to the command (up to 16 MiB); otherwise the
command's stdin is empty. There is no terminal: for an interactive shell,
attach to the session instead.

persona's own flags (-i) go before the spawn's name; everything after it is
the command, flags included.`,
		Example: `  persona exec jill git status
  persona exec jill ls -la /workspace
  persona exec jill sh -c 'cd src && go test ./...'
  echo hello | persona exec -i jill cat`,
		Args: cobra.MinimumNArgs(2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, s string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveDefault
			}
			return completeSpawns(cmd, args, s)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			rt := instance.Current()
			c, ok := rt.(instance.Commander)
			if !ok {
				return fmt.Errorf("the %s runtime can't run commands in spawns", rt.Name())
			}
			if err := rt.Available(ctx); err != nil {
				return err
			}
			var stdin []byte
			if withStdin {
				b, err := io.ReadAll(io.LimitReader(os.Stdin, maxExecStdin+1))
				if err != nil {
					return err
				}
				if len(b) > maxExecStdin {
					return errors.New("stdin is larger than 16 MiB")
				}
				stdin = b
			}
			res, err := c.RunCommand(ctx, args[0], args[1:], stdin)
			var ns *instance.ErrNotStarted
			if errors.As(err, &ns) { // as a shell reports it: 127 not found, 126 not runnable
				fmt.Fprintf(os.Stderr, "persona exec: %s\n", ns.Msg)
				if strings.HasSuffix(ns.Msg, "not found") {
					return exitCode(127)
				}
				return exitCode(126)
			}
			if err != nil {
				return err
			}
			os.Stdout.Write(res.Stdout)
			os.Stderr.Write(res.Stderr)
			if res.Exit != 0 {
				return exitCode(res.Exit)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&withStdin, "stdin", "i", false, "pass persona's stdin to the command")
	// Flags after the spawn's name belong to the command.
	cmd.Flags().SetInterspersed(false)
	return cmd
}
