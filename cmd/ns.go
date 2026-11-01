package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/ui"
)

const namespacesHelp = `A namespace is a separate network for microVM spawns: spawns in one reach
each other (and hear each other's beacon multicast), and nothing in any
other namespace. Each is a Linux network namespace with its own bridge and
NAT to the internet. The "default" namespace always exists and can't be
removed. Namespaces need the firecracker runtime.`

// newNSCmd is "persona ns", with create, remove and list.
func newNSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ns",
		Aliases: []string{"namespace", "namespaces"},
		Short:   "Manage network namespaces for spawns",
		Long:    namespacesHelp,
	}
	cmd.AddCommand(nsCreateCmd("create"), nsRemoveCmd("remove", "rm"), nsEnumCmd("list", "ls"))
	return cmd
}

func nsCreateCmd(use string, aliases ...string) *cobra.Command {
	return &cobra.Command{
		Use:     use + " <name>",
		Aliases: aliases,
		Short:   "Create a network namespace for spawns",
		Long:    namespacesHelp,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := namespacer(cmd.Context())
			if err != nil {
				return err
			}
			if err := n.CreateNamespace(cmd.Context(), args[0]); err != nil {
				return err
			}
			ui.Success(cmd.OutOrStdout(), fmt.Sprintf("Created namespace %s; spawn into it with %s", ui.Bold.Render(args[0]),
				ui.Bold.Render("persona spawn <persona> --namespace "+args[0])))
			return nil
		},
	}
}

func nsRemoveCmd(use string, aliases ...string) *cobra.Command {
	return &cobra.Command{
		Use:               use + " <name>",
		Aliases:           aliases,
		Short:             "Remove a network namespace (fails while any spawn is in it)",
		Long:              namespacesHelp + "\n\nA namespace with spawns in it, running or stopped, can't be removed: kill them first.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeNamespaces,
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] == instance.DefaultNamespace {
				return fmt.Errorf("the %s namespace can't be removed: persona always needs one", instance.DefaultNamespace)
			}
			n, err := namespacer(cmd.Context())
			if err != nil {
				return err
			}
			if err := n.RemoveNamespace(cmd.Context(), args[0]); err != nil {
				return err
			}
			ui.Success(cmd.OutOrStdout(), "Removed namespace "+args[0])
			return nil
		},
	}
}

func nsEnumCmd(use string, aliases ...string) *cobra.Command {
	return &cobra.Command{
		Use:               use + " [name]",
		Aliases:           aliases,
		Short:             "List namespaces and the spawns in each (or in one)",
		Long:              namespacesHelp,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeNamespaces,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			rt := instance.Current()
			if err := rt.Available(ctx); err != nil {
				return err
			}
			names, err := instance.Namespaces(ctx, rt)
			if err != nil {
				return err
			}
			filter := ""
			if len(args) == 1 {
				if filter = args[0]; !slices.Contains(names, filter) {
					return unknownNamespace(filter, names)
				}
			}
			cs, err := rt.List(ctx, "")
			if err != nil {
				return err
			}
			renderSpawnGroups(out, cs, names, filter, true)
			return nil
		},
	}
}

// namespacer is the current runtime's namespace support.
func namespacer(ctx context.Context) (instance.Namespacer, error) {
	rt := instance.Current()
	n, ok := rt.(instance.Namespacer)
	if !ok {
		return nil, errors.New("namespaces need the firecracker runtime (runtime: firecracker, Linux only)")
	}
	if err := rt.Available(ctx); err != nil {
		return nil, err
	}
	return n, nil
}

// checkNamespaceFlag validates a --namespace value against the runtime's
// namespaces ("" passes).
func checkNamespaceFlag(ctx context.Context, rt instance.Runtime, ns string) error {
	if ns == "" {
		return nil
	}
	names, err := instance.Namespaces(ctx, rt)
	if err != nil {
		return err
	}
	if !slices.Contains(names, ns) {
		return unknownNamespace(ns, names)
	}
	return nil
}

func unknownNamespace(ns string, names []string) error {
	return fmt.Errorf("no namespace %q (namespaces: %s; create one with `persona ns create %s`)", ns, strings.Join(names, ", "), ns)
}

// renderSpawnGroups prints spawns as one table per namespace. With filter,
// only that namespace. showEmpty prints namespaces without spawns too.
func renderSpawnGroups(out io.Writer, cs []instance.Container, names []string, filter string, showEmpty bool) {
	by := map[string][]instance.Container{}
	for _, c := range cs {
		ns := instance.NamespaceOf(c)
		by[ns] = append(by[ns], c)
		if !slices.Contains(names, ns) {
			names = append(names, ns) // a spawn in a namespace since removed
		}
	}
	first := true
	for _, ns := range names {
		if filter != "" && ns != filter {
			continue
		}
		group := by[ns]
		if len(group) == 0 && !showEmpty {
			continue
		}
		if !first {
			fmt.Fprintln(out)
		}
		first = false
		fmt.Fprintf(out, "%s %s\n", ui.Heading.Render("namespace "+ns), ui.Dim.Render(fmt.Sprintf("(%d)", len(group))))
		if len(group) == 0 {
			fmt.Fprintln(out, ui.Dim.Render("  no spawns"))
			continue
		}
		t := newTable("NAME", "PERSONA", "STATE", "STATUS", "CREATED")
		for _, c := range group {
			state := c.State
			if c.Running() {
				state = ui.OK.Render(state)
			}
			t.Row(ui.Bold.Render(c.Instance), c.Persona, state, c.Status, ago(c.Created))
		}
		fmt.Fprintln(out, t)
	}
}

func completeNamespaces(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	if err := config.Init(cfgFile, ""); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names, err := instance.Namespaces(cmd.Context(), instance.Current())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

func completeNamespaceFlag(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return completeNamespaces(cmd, nil, "")
}
