package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/ui"
)

const fwHelp = `Port forwards expose a spawn's port on the host: on localhost, or on one of
the host's own IPv4 addresses (reachable from your network, if your host's
firewall allows it). Or, to other spawns in any namespace, on the spawn's own
address in 100.64.0.0/10 (carrier-grade NAT space): give "cgnat" as the host.
A spawn at 10.200.a.b is 100.64.a.b there, and other spawns can also reach it
by name as <spawn>.<namespace>.persona. Each forward gets a rule id, shown by list-rules and used
by rm-rule. Forwards last until removed, or until their spawn is killed; a
stopped spawn keeps them. They need the firecracker runtime.`

func newFWCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "fw",
		Aliases: []string{"firewall"},
		Short:   "Forward host ports to spawns",
		Long:    fwHelp,
	}
	cmd.AddCommand(fwAllowCmd(), fwListCmd(), fwRemoveCmd())
	return cmd
}

func fwAllowCmd() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "allow-from <localhost|host-ip|cgnat>:<port>[:tcp|:udp] --to <spawn>:<port>",
		Short: "Forward a host address and port to a spawn's port",
		Long: fwHelp + `

The protocol defaults to tcp. The spawn must be running.`,
		Example: `  persona fw allow-from localhost:8080 --to jill:8080
  persona fw allow-from 172.16.1.100:80:tcp --to jill:8080
  persona fw allow-from localhost:5353:udp --to jill:53
  persona fw allow-from cgnat:80 --to jill:8080          # other spawns: 100.64.a.b:80`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			hostIP, hostPort, proto, err := parseFrom(args[0])
			if err != nil {
				return err
			}
			spawn, port, err := parseTo(to)
			if err != nil {
				return err
			}
			f, err := forwarder(ctx)
			if err != nil {
				return err
			}
			r, err := f.AddForward(ctx, spawn, proto, hostIP, hostPort, port)
			if err != nil {
				return err
			}
			ui.Success(out, fmt.Sprintf("Rule %s: %s → %s (namespace %s)", ui.Bold.Render(strconv.Itoa(r.ID)),
				fromString(r), toString(r), r.Namespace))
			fmt.Fprintf(out, "Remove it with %s\n", ui.Bold.Render(fmt.Sprintf("persona fw rm-rule %d", r.ID)))
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the spawn and its port, as <spawn>:<port> (required)")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func fwListCmd() *cobra.Command {
	var namespace string
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list-rules [spawn]",
		Aliases: []string{"ls", "list"},
		Short:   "List port forwards (of one spawn, of one namespace, or all of yours)",
		Long:    fwHelp,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			f, err := forwarder(ctx)
			if err != nil {
				return err
			}
			if err := checkNamespaceFlag(ctx, instance.Current(), namespace); err != nil {
				return err
			}
			rules, err := f.Forwards(ctx)
			if err != nil {
				return err
			}
			var shown []instance.PortForward
			for _, r := range rules {
				if len(args) == 1 && r.Spawn != args[0] {
					continue
				}
				if namespace != "" && r.Namespace != namespace {
					continue
				}
				shown = append(shown, r)
			}
			if asJSON {
				if shown == nil {
					shown = []instance.PortForward{}
				}
				return json.NewEncoder(out).Encode(shown)
			}
			if len(shown) == 0 {
				fmt.Fprintln(out, ui.Dim.Render("No port forwards. Add one with `persona fw allow-from <host>:<port> --to <spawn>:<port>`."))
				return nil
			}
			t := newTable("ID", "FROM", "TO", "NAMESPACE")
			for _, r := range shown {
				t.Row(ui.Bold.Render(strconv.Itoa(r.ID)), fromString(r), toString(r), r.Namespace)
			}
			fmt.Fprintln(out, t)
			return nil
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", "", "only forwards to spawns in this namespace")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	_ = cmd.RegisterFlagCompletionFunc("namespace", completeNamespaceFlag)
	return cmd
}

func fwRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm-rule <rule-id>...",
		Aliases: []string{"rm", "remove"},
		Short:   "Remove port forwards by rule id",
		Long:    fwHelp,
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			var ids []int
			for _, a := range args {
				id, err := strconv.Atoi(a)
				if err != nil || id < 1 {
					return fmt.Errorf("invalid rule id %q", a)
				}
				ids = append(ids, id)
			}
			f, err := forwarder(ctx)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := f.RemoveForward(ctx, id); err != nil {
					return err
				}
				ui.Success(out, fmt.Sprintf("Removed rule %d", id))
			}
			return nil
		},
	}
}

func forwarder(ctx context.Context) (instance.Forwarder, error) {
	rt := instance.Current()
	f, ok := rt.(instance.Forwarder)
	if !ok {
		return nil, errors.New("port forwards need the firecracker runtime (runtime: firecracker, Linux only)")
	}
	if err := rt.Available(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

// parseFrom parses <localhost|ipv4>:<port>[:tcp|:udp].
func parseFrom(s string) (ip string, port int, proto string, err error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return "", 0, "", fmt.Errorf("invalid address %q: want <localhost|host-ip>:<port>[:tcp|:udp]", s)
	}
	proto = "tcp"
	if len(parts) == 3 {
		if proto = strings.ToLower(parts[2]); proto != "tcp" && proto != "udp" {
			return "", 0, "", fmt.Errorf("invalid protocol %q: tcp or udp", parts[2])
		}
	}
	switch host := parts[0]; {
	case host == "localhost":
		ip = "127.0.0.1"
	case host == "cgnat":
		ip = "cgnat" // the spawn's own address in 100.64.0.0/10, picked by the host
	case net.ParseIP(host) != nil && net.ParseIP(host).To4() != nil:
		ip = net.ParseIP(host).To4().String()
	default:
		return "", 0, "", fmt.Errorf("invalid host %q: localhost or an IPv4 address of this host", host)
	}
	if port, err = parsePort(parts[1]); err != nil {
		return "", 0, "", err
	}
	return ip, port, proto, nil
}

// parseTo parses <spawn>:<port>.
func parseTo(s string) (spawn string, port int, err error) {
	spawn, p, ok := strings.Cut(s, ":")
	if !ok || spawn == "" {
		return "", 0, fmt.Errorf("invalid --to %q: want <spawn>:<port>", s)
	}
	port, err = parsePort(p)
	return spawn, port, err
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return p, nil
}

func fromString(r instance.PortForward) string {
	host := r.HostIP
	if host == "127.0.0.1" {
		host = "localhost"
	}
	s := fmt.Sprintf("%s:%d/%s", host, r.HostPort, r.Proto)
	if ip := net.ParseIP(r.HostIP); ip != nil && cgnatRange.Contains(ip) {
		s += " (spawns)"
	}
	return s
}

var cgnatRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

func toString(r instance.PortForward) string { return fmt.Sprintf("%s:%d", r.Spawn, r.Port) }
