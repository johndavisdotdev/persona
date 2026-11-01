package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

// Commands that manage spawns: running instances of a persona.

// resolveSpawns finds spawns by exact name (or the runtime's full name), never
// by persona name, so `kill reviewer` can't take out every reviewer spawn.
func resolveSpawns(ctx context.Context, rt instance.Runtime, refs []string, all bool) ([]instance.Container, error) {
	cs, err := rt.List(ctx, "")
	if err != nil {
		return nil, err
	}
	if all {
		return cs, nil
	}
	if len(refs) == 0 {
		return nil, errors.New("name a spawn (see `persona ps -a`), or pass --all")
	}
	var out []instance.Container
	for _, ref := range refs {
		found := false
		for _, c := range cs {
			if c.Instance == ref || c.Name == ref {
				out, found = append(out, c), true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("no spawn named %q (see `persona ps -a`)", ref)
		}
	}
	return out, nil
}

func completeSpawns(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	cs, _ := instance.Current().List(cmd.Context(), "")
	var out []string
	for _, c := range cs {
		out = append(out, c.Instance+"\t"+c.Persona+" ("+c.State+")")
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func newPsCmd() *cobra.Command {
	var all, asJSON bool
	var namespace string
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List running spawns by namespace (-a: all, including stopped)",
		Long: `Lists running spawns, grouped by network namespace (see persona ns).
-a includes stopped spawns; --namespace shows only one namespace.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			rt := instance.Current()
			if err := rt.Available(ctx); err != nil {
				return err
			}
			if err := checkNamespaceFlag(ctx, rt, namespace); err != nil {
				return err
			}
			all0, err := rt.List(ctx, "")
			if err != nil {
				return err
			}
			var cs, shown []instance.Container
			for _, c := range all0 {
				if namespace != "" && instance.NamespaceOf(c) != namespace {
					continue
				}
				cs = append(cs, c)
				if all || c.Running() {
					shown = append(shown, c)
				}
			}
			if asJSON {
				if shown == nil {
					shown = []instance.Container{}
				}
				return json.NewEncoder(out).Encode(shown)
			}
			if len(shown) == 0 {
				where := ""
				if namespace != "" {
					where = " in namespace " + namespace
				}
				msg := "No running spawns" + where + ". Start one with `persona spawn <persona>`."
				if !all && len(cs) > 0 {
					msg = fmt.Sprintf("No running spawns%s (%d stopped; `persona ps -a` lists them).", where, len(cs))
				}
				fmt.Fprintln(out, ui.Dim.Render(msg))
				return nil
			}
			names, err := instance.Namespaces(ctx, rt)
			if err != nil {
				return err
			}
			renderSpawnGroups(out, shown, names, namespace, false)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include stopped spawns")
	cmd.Flags().StringVar(&namespace, "namespace", "", "only spawns in this namespace")
	_ = cmd.RegisterFlagCompletionFunc("namespace", completeNamespaceFlag)
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

// ago renders a creation time (RFC 3339 or Docker's format) as "3 hours ago".
func ago(created string) string {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05 -0700 MST"} {
		if t, err := time.Parse(layout, created); err == nil {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%d min ago", int(d.Minutes()))
			case d < 48*time.Hour:
				return fmt.Sprintf("%d hours ago", int(d.Hours()))
			}
			return fmt.Sprintf("%d days ago", int(d.Hours()/24))
		}
	}
	return created
}

func newAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "attach [spawn]",
		Aliases: []string{"join"},
		Short:   "Attach to a running spawn's Claude Code session",
		Long: `Attaches your terminal to the tmux session running Claude Code in a spawn.
Detach with Ctrl-b d; the session keeps running. Without a name, you pick
from the running spawns.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeSpawns,
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

func newStopCmd() *cobra.Command {
	var all, force bool
	cmd := &cobra.Command{
		Use:   "stop <spawn>...",
		Short: "Stop spawns, keeping them to start again later",
		Long: `Stops spawns without removing them; ` + "`persona start`" + ` resumes one with its
files intact, and ` + "`persona kill`" + ` removes it.

A microVM spawn saves its work first: commits land on its branch, and
uncommitted changes are copied into its worktree. --force skips that, for a
spawn that no longer responds.`,
		ValidArgsFunction: completeSpawns,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			rt := instance.Current()
			if err := rt.Available(ctx); err != nil {
				return err
			}
			if force {
				ctx = instance.WithForce(ctx)
			}
			targets, err := resolveSpawns(ctx, rt, args, all)
			if err != nil {
				return err
			}
			n := 0
			for _, c := range targets {
				if !c.Running() {
					if !all {
						ui.Step(out, c.Instance+" is already stopped")
					}
					continue
				}
				if err := rt.Stop(ctx, c.Name); err != nil {
					return err
				}
				ui.Success(out, "Stopped "+c.Instance)
				n++
			}
			if n > 0 {
				fmt.Fprintln(out, ui.Dim.Render("Start again with `persona start <spawn>`; remove with `persona kill <spawn>`."))
			} else if all {
				ui.Step(out, "nothing running")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop every running spawn")
	cmd.Flags().BoolVar(&force, "force", false, "stop without saving work (for spawns that no longer respond)")
	return cmd
}

func newStartCmd() *cobra.Command {
	var attach bool
	cmd := &cobra.Command{
		Use:   "start <spawn>...",
		Short: "Start stopped spawns again",
		Long: `Resumes stopped spawns with their files, branch and address intact, and a
fresh Claude Code session using your current credentials.`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completeSpawns,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			rt := instance.Current()
			if err := rt.Available(ctx); err != nil {
				return err
			}
			targets, err := resolveSpawns(ctx, rt, args, false)
			if err != nil {
				return err
			}
			if attach && len(targets) != 1 {
				return errors.New("--attach needs exactly one spawn")
			}
			for _, c := range targets {
				if c.Running() {
					ui.Step(out, c.Instance+" is already running")
					continue
				}
				var personaEnv []string
				harness := ""
				if p, err := persona.Load(c.Persona); err == nil {
					personaEnv, harness = p.Env, harnessOf(p)
				}
				env, err := instanceEnv(personaEnv, nil, harness, out)
				if err != nil {
					return err
				}
				if err := rt.Start(ctx, c.Name, env); err != nil {
					return err
				}
				ui.Success(out, "Started "+c.Instance)
			}
			if attach {
				return attachTo(ctx, targets[0].Name, out)
			}
			fmt.Fprintf(out, "Attach with %s\n", ui.Bold.Render("persona attach "+targets[0].Instance))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&attach, "attach", "a", false, "attach to the session once started")
	return cmd
}

func newKillCmd() *cobra.Command {
	var all, force bool
	cmd := &cobra.Command{
		Use:   "kill <spawn>...",
		Short: "Stop and remove spawns",
		Long: `Stops spawns and removes them for good. A microVM spawn saves its work
first (commits on its branch, uncommitted changes into its worktree); its
worktree is then deleted unless it has uncommitted changes, and its branch is
always kept. --force skips saving, for a spawn that no longer responds.`,
		ValidArgsFunction: completeSpawns,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			rt := instance.Current()
			if err := rt.Available(ctx); err != nil {
				return err
			}
			if force {
				ctx = instance.WithForce(ctx)
			}
			targets, err := resolveSpawns(ctx, rt, args, all)
			if err != nil {
				return err
			}
			for _, c := range targets {
				if err := rt.Remove(ctx, c.Name); instance.IsKept(err) {
					ui.Warning(out, err.Error())
				} else if err != nil {
					return err
				}
				ui.Success(out, "Killed "+c.Instance)
			}
			if len(targets) == 0 {
				ui.Step(out, "no spawns")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "kill every spawn")
	cmd.Flags().BoolVar(&force, "force", false, "kill without saving work (for spawns that no longer respond)")
	return cmd
}

func newSaveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "save <spawn> <new-persona>",
		Short: "Save a running spawn's current state as a new persona",
		Long: `Captures a running spawn's filesystem (everything you or Claude installed or
configured, but not /workspace, which every spawn fills from its own
directory) as a new persona. The spawn keeps running.

A saved persona has no definition to rebuild from: ` + "`persona update`" + ` and ` + "`persona edit`" + `
don't apply. Change it with ` + "`persona customize`" + `, or by spawning it, making the
changes there, and saving again.`,
		Example:           `  persona save keen-yak-89 dotnet-dev-tuned`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeSpawns,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			spawnRef, name := args[0], args[1]
			if err := validateNewName(name); err != nil {
				return err
			}
			rt := instance.Current()
			if err := rt.Available(ctx); err != nil {
				return err
			}
			targets, err := resolveSpawns(ctx, rt, []string{spawnRef}, false)
			if err != nil {
				return err
			}
			c := targets[0]
			if !c.Running() {
				return fmt.Errorf("%s is stopped; start it first (`persona start %s`)", c.Instance, c.Instance)
			}
			fmt.Fprintln(out, ui.Heading.Render("Saving "+c.Instance+" as "+name))
			start := time.Now()
			if err := rt.Save(ctx, c.Name, name); err != nil {
				return err
			}
			p := savedPersona(name, c)
			if err := p.Save(); err != nil {
				return err
			}
			ui.Success(out, fmt.Sprintf("Saved %s as persona %s in %s", c.Instance, ui.Bold.Render(name), time.Since(start).Round(100*time.Millisecond)))
			fmt.Fprintf(out, "Spawn it with %s\n", ui.Bold.Render("persona spawn "+name))
			return nil
		},
	}
}

// savedPersona describes a persona saved from spawn c. What it contains is
// its image; the rest is carried over from the source persona for reference,
// plus the env vars its spawns forward.
func savedPersona(name string, c instance.Container) *persona.Persona {
	p := &persona.Persona{Name: name,
		Description: fmt.Sprintf("Saved from spawn %s of persona %s.", c.Instance, c.Persona),
		SavedFrom:   &persona.SavedFrom{Persona: c.Persona, Spawn: c.Instance, At: time.Now().UTC()}}
	if src, err := persona.Load(c.Persona); err == nil {
		p.Summary, p.Model, p.Env, p.Harness = src.Summary, src.Model, src.Env, harnessOf(src)
		p.Plugins, p.MCPServers, p.Skills, p.Toolchains = src.Plugins, src.MCPServers, src.Skills, src.Toolchains
		p.Description = src.Description + "\n\n" + p.Description
	}
	return p
}
