package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/beaconnet"
	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/ui"
)

func newMessageCmd() *cobra.Command {
	var names []string
	var personaName, namespace string
	var discover bool
	var wait time.Duration
	cmd := &cobra.Command{
		Use:     "message <text>...",
		Aliases: []string{"msg"},
		Short:   "Send a message to running spawns' Claude Code sessions",
		Long: `Sends a message to running microVM spawns through persona-beacon, which
delivers it into each spawn's Claude Code session as a channel message.

Without --name or --persona it goes to every running spawn: one multicast on
each namespace's bridge (only --namespace's, if given). With --name (one or
more spawns) and/or --persona (every spawn of that persona) it goes only to
the spawns matching all the filters given, --namespace included.

--discover instead lists the agents that answer a discovery request, in every
namespace or in --namespace.

The sockets are created inside each namespace by the persona-vm wrapper and
handed back, so this needs the persona package and the firecracker runtime.`,
		Example: `  persona message "Pull main and rerun the tests"
  persona message --name jill "Review PR 42"
  persona message --persona reviewer --namespace lab "Stop and summarize"
  persona message --discover`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			rt := instance.Current()
			if _, ok := rt.(instance.Namespacer); !ok {
				return errors.New("messages need the firecracker runtime (runtime: firecracker, Linux only)")
			}
			if err := rt.Available(ctx); err != nil {
				return err
			}
			if err := checkNamespaceFlag(ctx, rt, namespace); err != nil {
				return err
			}
			if discover {
				if len(args) > 0 || len(names) > 0 || personaName != "" {
					return errors.New("--discover takes no message, --name or --persona")
				}
				return discoverAgents(ctx, rt, namespace, wait, out)
			}
			text := strings.TrimSpace(strings.Join(args, " "))
			if text == "" {
				return errors.New("no message: pass the text to send")
			}
			cs, err := rt.List(ctx, "")
			if err != nil {
				return err
			}
			targets, err := messageTargets(cs, names, personaName, namespace)
			if err != nil {
				return err
			}
			broadcast := len(names) == 0 && personaName == ""
			byNS := map[string][]instance.Container{}
			var order []string
			for _, c := range targets {
				ns := instance.NamespaceOf(c)
				if byNS[ns] == nil {
					order = append(order, ns)
				}
				byNS[ns] = append(byNS[ns], c)
			}
			for _, ns := range order {
				conn, err := instance.BridgeSocket(ctx, ns)
				if err != nil {
					return fmt.Errorf("namespace %s: %w", ns, err)
				}
				if broadcast {
					err = beaconnet.Command(conn, text)
				} else {
					for _, c := range byNS[ns] {
						if err = beaconnet.CommandTo(conn, net.ParseIP(c.IP), text); err != nil {
							break
						}
					}
				}
				conn.Close()
				if err != nil {
					return fmt.Errorf("namespace %s: %w", ns, err)
				}
				var who []string
				for _, c := range byNS[ns] {
					who = append(who, c.Instance)
				}
				how := "to"
				if broadcast {
					how = "to everyone in"
				}
				ui.Success(out, fmt.Sprintf("Sent %s namespace %s: %s", how, ns, strings.Join(who, ", ")))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&names, "name", nil, "spawn(s) to message (repeatable, or comma-separated)")
	f.StringVar(&personaName, "persona", "", "message every spawn of this persona")
	f.StringVar(&namespace, "namespace", "", "only spawns in this namespace")
	f.BoolVar(&discover, "discover", false, "list the agents that answer a discovery request instead")
	f.DurationVar(&wait, "wait", 2*time.Second, "with --discover, how long to collect answers")
	_ = cmd.RegisterFlagCompletionFunc("namespace", completeNamespaceFlag)
	_ = cmd.RegisterFlagCompletionFunc("persona", completePersonas)
	return cmd
}

// messageTargets picks the running spawns matching every filter given.
func messageTargets(cs []instance.Container, names []string, personaName, namespace string) ([]instance.Container, error) {
	var out []instance.Container
	found := map[string]bool{}
	for _, c := range cs {
		if !c.Running() || c.IP == "" {
			continue
		}
		if len(names) > 0 && !slices.Contains(names, c.Instance) {
			continue
		}
		found[c.Instance] = true
		if personaName != "" && c.Persona != personaName {
			continue
		}
		if namespace != "" && instance.NamespaceOf(c) != namespace {
			continue
		}
		out = append(out, c)
	}
	for _, n := range names {
		if !found[n] {
			return nil, fmt.Errorf("no running spawn %q", n)
		}
	}
	if len(out) == 0 {
		var f []string
		if len(names) > 0 {
			f = append(f, "--name "+strings.Join(names, ","))
		}
		if personaName != "" {
			f = append(f, "--persona "+personaName)
		}
		if namespace != "" {
			f = append(f, "--namespace "+namespace)
		}
		if len(f) == 0 {
			return nil, errors.New("no running spawns to message")
		}
		return nil, fmt.Errorf("no running spawn matches %s", strings.Join(f, " "))
	}
	return out, nil
}

func discoverAgents(ctx context.Context, rt instance.Runtime, namespace string, wait time.Duration, out io.Writer) error {
	names, err := instance.Namespaces(ctx, rt)
	if err != nil {
		return err
	}
	for _, ns := range names {
		if namespace != "" && ns != namespace {
			continue
		}
		conn, err := instance.BridgeSocket(ctx, ns)
		if err != nil {
			return fmt.Errorf("namespace %s: %w", ns, err)
		}
		agents, err := beaconnet.Discover(ctx, conn, wait)
		conn.Close()
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %s\n", ui.Heading.Render("namespace "+ns), ui.Dim.Render(fmt.Sprintf("(%d)", len(agents))))
		for _, a := range agents {
			fmt.Fprintf(out, "  %-32s %-20s %s\n", a.GetName(), a.GetPersona(), beaconnet.IP(a.GetAddress()))
		}
	}
	return nil
}
