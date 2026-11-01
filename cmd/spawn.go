package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/persona/naming"
	"github.com/johndavisdev/persona/internal/ui"
)

// SpawnOptions are shared by the CLI and the control center.
type SpawnOptions struct {
	Instance        string
	Workdir         string
	NoMount         bool
	Env             []string
	Prompt          string
	SkipPermissions bool
	Cols, Rows      int
	Log             io.Writer
}

// Spawn builds the image if needed and starts a container. It returns the container name.
func Spawn(ctx context.Context, name string, o SpawnOptions) (string, error) {
	p, err := persona.Load(name)
	if err != nil {
		return "", err
	}
	if err := docker.Available(ctx); err != nil {
		return "", err
	}
	log := o.Log
	if log == nil {
		log = io.Discard
	}
	if !docker.ImageExists(ctx, p.Image()) {
		if err := buildImage(ctx, p.Name, false, log); err != nil {
			return "", err
		}
	}
	inst := o.Instance
	if inst == "" {
		inst = naming.Random()
	} else if err := persona.ValidateName(inst); err != nil {
		return "", fmt.Errorf("instance name: %w", err)
	}

	workdir := ""
	if !o.NoMount {
		workdir = o.Workdir
		if workdir == "" {
			if workdir, err = os.Getwd(); err != nil {
				return "", err
			}
		}
		if workdir, err = filepath.Abs(workdir); err != nil {
			return "", err
		}
		if st, err := os.Stat(workdir); err != nil || !st.IsDir() {
			return "", fmt.Errorf("workdir %q is not a directory", workdir)
		}
	}

	var env []string
	for _, k := range append(viper.GetStringSlice(config.KeyForwardEnv), p.Env...) {
		if _, ok := os.LookupEnv(k); ok {
			env = append(env, k) // value forwarded from host by docker, never on the command line
		}
	}
	env = append(env, o.Env...)
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		ui.Warning(log, "neither ANTHROPIC_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN is set; Claude Code will ask you to log in (run `claude setup-token` to create a token)")
	}

	claudeArgs := ""
	if o.SkipPermissions {
		claudeArgs = "--dangerously-skip-permissions"
	}
	return docker.Run(ctx, docker.RunOptions{
		Persona: p.Name, Instance: inst, Workdir: workdir, Env: env,
		ClaudeArgs: claudeArgs, Prompt: o.Prompt, Cols: o.Cols, Rows: o.Rows,
	})
}

func newSpawnCmd() *cobra.Command {
	var o SpawnOptions
	var attach bool
	cmd := &cobra.Command{
		Use:   "spawn <persona>",
		Short: "Spawn a container running Claude Code as the given persona",
		Long: `Starts a detached container from the persona's image (building it first if
needed). Claude Code runs inside a tmux session; attach with --attach,
` + "`persona join`" + `, or the control center. The current directory is mounted at
/workspace unless --workdir or --no-mount is given. ANTHROPIC_API_KEY,
CLAUDE_CODE_OAUTH_TOKEN, and the persona's declared env vars are forwarded.`,
		Example: `  persona spawn reviewer -a
  persona spawn data-eng --workdir ~/src/warehouse --prompt "audit the dbt models"
  persona spawn researcher --no-mount --name scratch`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completePersonas,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			o.Log = out
			name, err := Spawn(ctx, args[0], o)
			if err != nil {
				return err
			}
			ui.Success(out, "Spawned "+ui.Bold.Render(name))
			if !attach {
				fmt.Fprintf(out, "Join with %s · stop with %s\n", ui.Bold.Render("persona join "+name), ui.Bold.Render("persona stop "+name))
				return nil
			}
			return attachTo(ctx, name, out)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Instance, "name", "", "instance name (default: random)")
	f.StringVarP(&o.Workdir, "workdir", "w", "", "host directory mounted at /workspace (default: current directory)")
	f.BoolVar(&o.NoMount, "no-mount", false, "do not mount any host directory")
	f.StringArrayVarP(&o.Env, "env", "e", nil, "extra env (NAME to forward from host, or NAME=VALUE)")
	f.StringVarP(&o.Prompt, "prompt", "p", "", "initial prompt for Claude Code")
	f.BoolVar(&o.SkipPermissions, "skip-permissions", false, "run Claude Code with --dangerously-skip-permissions (the container is the sandbox)")
	f.BoolVarP(&attach, "attach", "a", false, "attach to the session immediately")
	return cmd
}

func attachTo(ctx context.Context, container string, out io.Writer) error {
	if !ui.Interactive() {
		return errors.New("attaching requires an interactive terminal")
	}
	if err := waitForSession(ctx, container, 30*time.Second); err != nil {
		return err
	}
	fmt.Fprintln(out, ui.Dim.Render("Attaching to "+container+" — detach with Ctrl-b d"))
	err := docker.AttachCommand(ctx, container).Run()
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		return nil // tmux exits non-zero on some detaches; not a user-facing error
	}
	return err
}
