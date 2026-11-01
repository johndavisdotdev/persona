package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/auth"
	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/instance"
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
	Namespace       string // network namespace ("" is default)
	Cols, Rows      int
	Log             io.Writer
}

// Spawn builds the image if needed and starts a spawn. It returns the
// runtime's name for it and the spawn's own (random unless given) name.
func Spawn(ctx context.Context, name string, o SpawnOptions) (string, string, error) {
	c, inst, err := spawn(ctx, name, o)
	return c, inst, err
}

func spawn(ctx context.Context, name string, o SpawnOptions) (string, string, error) {
	p, err := persona.Load(name)
	if err != nil {
		return "", "", err
	}
	rt := instance.Current()
	if err := rt.Available(ctx); err != nil {
		return "", "", err
	}
	log := o.Log
	if log == nil {
		log = io.Discard
	}
	if !docker.ImageExists(ctx, p.Image()) {
		if err := buildImage(ctx, p.Name, false, log); err != nil {
			return "", "", err
		}
	}
	inst, err := spawnName(ctx, rt, o.Instance)
	if err != nil {
		return "", "", err
	}

	workdir := ""
	if !o.NoMount {
		workdir = o.Workdir
		if workdir == "" {
			if workdir, err = os.Getwd(); err != nil {
				return "", "", err
			}
		}
		if workdir, err = filepath.Abs(workdir); err != nil {
			return "", "", err
		}
		if st, err := os.Stat(workdir); err != nil || !st.IsDir() {
			return "", "", fmt.Errorf("workdir %q is not a directory", workdir)
		}
	}

	env, err := instanceEnv(p.Env, o.Env, harnessOf(p), log)
	if err != nil {
		return "", "", err
	}

	claudeArgs := ""
	if o.SkipPermissions {
		claudeArgs = "--dangerously-skip-permissions"
	}
	c, err := rt.Run(ctx, instance.RunOptions{
		Persona: p.Name, Instance: inst, Workdir: workdir, Env: env,
		ClaudeArgs: claudeArgs, Prompt: o.Prompt, Cols: o.Cols, Rows: o.Rows, Log: log, Namespace: o.Namespace,
	})
	return c, inst, err
}

// spawnName validates a requested spawn name, or picks a random one, unique
// across every persona's spawns.
func spawnName(ctx context.Context, rt instance.Runtime, want string) (string, error) {
	cs, err := rt.List(ctx, "")
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, c := range cs {
		taken[c.Instance] = true
	}
	if want != "" {
		if err := persona.ValidateName(want); err != nil {
			return "", fmt.Errorf("spawn name: %w", err)
		}
		if taken[want] {
			return "", fmt.Errorf("a spawn named %s already exists", want)
		}
		return want, nil
	}
	for i := 0; i < 100; i++ {
		if n := naming.Random(); !taken[n] {
			return n, nil
		}
	}
	return "", errors.New("could not find a free spawn name; pass --name")
}

// harnessOf is the harness p's spawns run (its own, the base's, or the default).
func harnessOf(p *persona.Persona) string {
	b, _ := persona.LoadBase()
	return persona.EffectiveHarness(p, b)
}

// instanceEnv is what a spawn receives: the configured and persona-declared
// env vars, extra -e entries, and credentials for its harness: Claude Code
// gets the Claude subscription token (see auth.ForInstance); pi gets the
// OpenRouter key and never the Claude token, which only Anthropic's own apps
// may use. Values are forwarded by name from this process's environment,
// never put on a command line, so stored secrets are set in it here.
func instanceEnv(personaEnv, extra []string, harness string, log io.Writer) ([]string, error) {
	stored, err := auth.Load()
	if err != nil {
		return nil, err
	}
	if harness == persona.HarnessPi {
		stored = nil
	}
	plan := auth.ForInstance(os.LookupEnv, append(viper.GetStringSlice(config.KeyForwardEnv), personaEnv...), extra, stored)
	if harness == persona.HarnessPi {
		plan.Notes = nil
		plan.Forward = slices.DeleteFunc(plan.Forward, func(k string) bool { return k == auth.TokenEnv })
	}
	for _, n := range plan.Notes {
		ui.Step(log, n)
	}
	env := plan.Forward
	// A stored OpenRouter key (persona configure) reaches spawns like an
	// exported one: by name, from this process's environment.
	if _, ok := os.LookupEnv("OPENROUTER_API_KEY"); !ok {
		if k := viper.GetString(config.KeyOpenRouterAPIKey); k != "" && slices.Contains(viper.GetStringSlice(config.KeyForwardEnv), "OPENROUTER_API_KEY") {
			plan.Inject = append(plan.Inject, "OPENROUTER_API_KEY="+k)
		}
	}
	for _, kv := range plan.Inject {
		k, v, _ := strings.Cut(kv, "=")
		if err := os.Setenv(k, v); err != nil {
			return nil, err
		}
		env = append(env, k)
	}
	env = append(env, extra...)
	switch {
	case harness == persona.HarnessPi:
		if !slices.Contains(env, "OPENROUTER_API_KEY") {
			ui.Warning(log, "no OpenRouter API key for the spawn; pi can't reach a model until you add one (run `persona configure`)")
		}
	case !plan.HasCredentials() && !slices.Contains(extra, auth.APIKeyEnv) && !slices.Contains(extra, auth.TokenEnv):
		ui.Warning(log, "no Claude credentials for the spawn; Claude Code will ask you to log in (run `persona auth login` to use your Claude subscription)")
	}
	return env, nil
}

func newSpawnCmd() *cobra.Command {
	var o SpawnOptions
	var attach bool
	cmd := &cobra.Command{
		Use:   "spawn <persona>",
		Short: "Spawn a new instance of a persona (a microVM on Linux) running Claude Code",
		Long: `Starts a detached instance from the persona's image (building it first if
needed). Claude Code runs inside a tmux session; attach with --attach,
` + "`persona attach`" + `, or the control center. The persona's declared env vars
are forwarded.

Claude Code signs in with your subscription token: CLAUDE_CODE_OAUTH_TOKEN if
it is exported, otherwise the one stored by ` + "`persona auth login`" + `. With a
token, ANTHROPIC_API_KEY is not forwarded unless you pass -e ANTHROPIC_API_KEY;
without one, ANTHROPIC_API_KEY is used.

On Linux the default runtime is firecracker, and the instance is a microVM.
Inside a git repository it gets its own branch, persona/<instance>, with a
worktree under $PERSONA_HOME/worktrees and a copy in the VM. Claude Code
pushes and opens pull requests through a broker on the host, so no
credentials enter the VM. Stopping it saves its work to the worktree. Outside
a repository nothing is copied: /workspace starts empty.

With the docker runtime (the default off Linux, or runtime: docker), the
current directory (or --workdir) is mounted at /workspace instead.`,
		Example: `  persona spawn reviewer -a
  persona spawn data-eng --workdir ~/src/warehouse --prompt "audit the dbt models"
  persona spawn researcher --no-mount --name scratch`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completePersonas,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			o.Log = out
			name, inst, err := Spawn(ctx, args[0], o)
			if err != nil {
				return err
			}
			ui.Success(out, "Spawned "+ui.Bold.Render(inst)+ui.Dim.Render(" ("+args[0]+")"))
			if !attach {
				fmt.Fprintf(out, "Attach with %s · stop with %s\n", ui.Bold.Render("persona attach "+inst), ui.Bold.Render("persona stop "+inst))
				return nil
			}
			return attachTo(ctx, name, out)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Instance, "name", "", "spawn name (default: random, e.g. keen-yak-89)")
	f.StringVarP(&o.Workdir, "workdir", "w", "", "host directory mounted at /workspace (default: current directory)")
	f.BoolVar(&o.NoMount, "no-mount", false, "do not mount any host directory")
	f.StringArrayVarP(&o.Env, "env", "e", nil, "extra env (NAME to forward from host, or NAME=VALUE)")
	f.StringVarP(&o.Prompt, "prompt", "p", "", "initial prompt for Claude Code")
	f.BoolVar(&o.SkipPermissions, "skip-permissions", false, "run Claude Code with --dangerously-skip-permissions (the container is the sandbox)")
	f.BoolVarP(&attach, "attach", "a", false, "attach to the session immediately")
	f.StringVar(&o.Namespace, "namespace", "", "network namespace to spawn into (default: default; see persona ns)")
	_ = cmd.RegisterFlagCompletionFunc("namespace", completeNamespaceFlag)
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
	err := instance.Current().AttachCommand(ctx, container).Run()
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		return nil // tmux exits non-zero on some detaches; not a user-facing error
	}
	return err
}
