package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/server"
	"github.com/johndavisdev/persona/internal/status"
	"github.com/johndavisdev/persona/internal/ui"
)

func newServerCmd() *cobra.Command {
	var addr, certFile, keyFile, webDir string
	var allowOrigins []string
	var newToken, forceTLS bool
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Web UI: spawns, namespaces, port forwards, new personas and terminals in the browser",
		Long: `Serves persona's web UI over HTTP/3, talking to the page over WebTransport:
an overview of every spawn, drawn inside its network namespace; creating
personas with their build output live; spawning, stopping and killing;
namespaces and port forwards; and a terminal (xterm.js) on any spawn's
Claude Code session.

It is a remote control for every spawn, so it listens on 127.0.0.1 by default
and every session needs a token, kept in $PERSONA_HOME/server/token (0600) and
printed with the URL. --new-token replaces it.

TLS: with --cert and --key, your certificate. Otherwise a self-signed ECDSA
certificate, renewed every 13 days, which the page trusts for WebTransport by
its hash. On a loopback address the page itself is served over plain HTTP
(the browser treats localhost as secure); elsewhere over HTTPS with the
self-signed certificate (the browser will warn once per certificate), unless
--cert is given. WebTransport needs a recent Chrome, Edge or Firefox.

The page is the web app (web/), built into the binary with make web; --web
serves another build of it from disk instead (e.g. web/dist while working on
it). It connects by fetching /config.json for the WebTransport URL (and
certificate hash), then opening a session at that URL with ?token=. --allow-origin lets a page served from
elsewhere (a Vite dev server) connect too.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return fmt.Errorf("--addr: %w", err)
			}
			ip := net.ParseIP(host)
			loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
			dir := filepath.Join(config.Home(), "server")
			token, err := serverToken(dir, newToken)
			if err != nil {
				return err
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			s := &server.Server{
				Addr: addr, Token: token, Actions: &serverActions{self: self},
				CertFile: certFile, KeyFile: keyFile, CertDir: dir, WebDir: webDir, AllowOrigins: allowOrigins,
				PlainHTTP: loopback && certFile == "" && !forceTLS,
				Log:       log.New(os.Stderr, "persona server: ", log.LstdFlags),
			}
			if webDir != "" {
				if fi, err := os.Stat(filepath.Join(webDir, "index.html")); err != nil || fi.IsDir() {
					return fmt.Errorf("--web %s has no index.html (build the web app first)", webDir)
				}
			}
			if !loopback {
				ui.Warning(out, "listening on "+addr+": anyone who can reach it and has the token controls every spawn")
			}
			ui.Success(out, "persona server on "+addr+" (HTTP/3 + WebTransport over UDP, the page over TCP)")
			fmt.Fprintf(out, "Open %s\n", ui.Bold.Render(s.URL("")))
			return s.Run(ctx)
		},
	}
	f := cmd.Flags()
	f.StringVar(&addr, "addr", "127.0.0.1:7443", "address to listen on (TCP for the page, UDP for HTTP/3)")
	f.StringVar(&certFile, "cert", "", "TLS certificate (PEM) instead of a self-signed one")
	f.StringVar(&keyFile, "key", "", "TLS key (PEM) for --cert")
	f.BoolVar(&newToken, "new-token", false, "replace the access token")
	f.BoolVar(&forceTLS, "tls", false, "serve the page over HTTPS even on loopback")
	f.StringVar(&webDir, "web", "", "serve this directory (a build of the web app, e.g. web/dist) instead of the one in the binary")
	f.StringSliceVar(&allowOrigins, "allow-origin", nil, "also accept sessions from pages at this origin (e.g. http://172.16.1.100:5173 for a Vite dev server)")
	return cmd
}

// serverToken reads the access token, or makes a new one.
func serverToken(dir string, fresh bool) (string, error) {
	path := filepath.Join(dir, "token")
	if !fresh {
		if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
			return strings.TrimSpace(string(b)), nil
		}
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

// serverActions is what the web UI can do, on the current runtime.
type serverActions struct{ self string }

func (a *serverActions) State(ctx context.Context) server.State {
	rt := instance.Current()
	st := server.State{Runtime: rt.Name()}
	_, st.Features.Namespaces = rt.(instance.Namespacer)
	_, st.Features.Firewall = rt.(instance.Forwarder)
	fail := func(what string, err error) { st.Errors = append(st.Errors, what+": "+err.Error()) }
	st.HostAddresses = hostAddresses()

	if ns, err := instance.Namespaces(ctx, rt); err != nil {
		fail("namespaces", err)
		st.Namespaces = []string{instance.DefaultNamespace}
	} else {
		st.Namespaces = ns
	}
	if cs, err := rt.List(ctx, ""); err != nil {
		fail("spawns", err)
	} else {
		for _, c := range cs {
			sp := server.Spawn{Name: c.Instance, Container: c.Name, Persona: c.Persona, Namespace: instance.NamespaceOf(c),
				State: c.State, Status: c.Status, IP: c.IP, Created: c.Created}
			if ip := net.ParseIP(c.IP).To4(); ip != nil && ip[0] == 10 && ip[1] == 200 {
				sp.CGNAT = net.IPv4(100, 64, ip[2], ip[3]).String()
			}
			st.Spawns = append(st.Spawns, sp)
		}
	}
	ps, errs := persona.List()
	for _, err := range errs {
		fail("personas", err)
	}
	{
		dockerOK := docker.Available(ctx) == nil
		for _, p := range ps {
			st.Personas = append(st.Personas, server.PersonaInfo{Name: p.Name, Summary: p.Summary,
				Harness: p.Harness, Built: dockerOK && docker.ImageExists(ctx, p.Image())})
		}
	}
	if f, ok := rt.(instance.Forwarder); ok {
		if rules, err := f.Forwards(ctx); err != nil {
			fail("port forwards", err)
		} else {
			for _, r := range rules {
				st.Forwards = append(st.Forwards, server.Forward{ID: r.ID, Spawn: r.Spawn, Namespace: r.Namespace,
					Proto: r.Proto, HostIP: r.HostIP, HostPort: r.HostPort, Port: r.Port})
			}
		}
	}
	return st
}

// hostAddresses are the host's IPv4 addresses a port forward can listen on:
// not loopback (that's "localhost"), link-local, or persona's own networks.
func hostAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	own := []*net.IPNet{
		{IP: net.IPv4(10, 199, 0, 0), Mask: net.CIDRMask(16, 32)},
		{IP: net.IPv4(10, 200, 0, 0), Mask: net.CIDRMask(16, 32)},
		{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)},
	}
	var out []string
next:
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := n.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		for _, o := range own {
			if o.Contains(ip) {
				continue next
			}
		}
		out = append(out, ip.String())
	}
	return out
}

var spawnNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

func (a *serverActions) Do(ctx context.Context, op string, raw json.RawMessage) (any, error) {
	var args struct {
		Name      string   `json:"name"`
		Spawn     string   `json:"spawn"`
		ID        int      `json:"id"`
		From      string   `json:"from"` // fw.add: <localhost|ip|cgnat>:<port>[:udp]
		Port      int      `json:"port"`
		Text      string   `json:"text"`
		Names     []string `json:"names"`
		Persona   string   `json:"persona"`
		Namespace string   `json:"namespace"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
	}
	switch op {
	case "ns.create", "ns.remove":
		n, err := namespacer(ctx)
		if err != nil {
			return nil, err
		}
		if op == "ns.create" {
			return nil, n.CreateNamespace(ctx, args.Name)
		}
		return nil, n.RemoveNamespace(ctx, args.Name)
	case "fw.add":
		f, err := forwarder(ctx)
		if err != nil {
			return nil, err
		}
		ip, hostPort, proto, err := parseFrom(args.From)
		if err != nil {
			return nil, err
		}
		if args.Port < 1 || args.Port > 65535 {
			return nil, fmt.Errorf("invalid port %d", args.Port)
		}
		return f.AddForward(ctx, args.Spawn, proto, ip, hostPort, args.Port)
	case "fw.remove":
		f, err := forwarder(ctx)
		if err != nil {
			return nil, err
		}
		return nil, f.RemoveForward(ctx, args.ID)
	case "spawn.stop", "spawn.start", "spawn.kill":
		if !spawnNameRE.MatchString(args.Spawn) {
			return nil, fmt.Errorf("invalid spawn %q", args.Spawn)
		}
		verb := strings.TrimPrefix(op, "spawn.")
		out, err := exec.CommandContext(ctx, a.self, verb, "--", args.Spawn).CombinedOutput()
		if err != nil {
			return nil, errors.New(lastLines(ansiRE.ReplaceAllString(string(out), ""), 3))
		}
		return nil, nil
	case "message":
		cmdArgs := []string{"message"}
		for _, n := range args.Names {
			cmdArgs = append(cmdArgs, "--name", n)
		}
		if args.Persona != "" {
			cmdArgs = append(cmdArgs, "--persona", args.Persona)
		}
		if args.Namespace != "" {
			cmdArgs = append(cmdArgs, "--namespace", args.Namespace)
		}
		if strings.TrimSpace(args.Text) == "" {
			return nil, errors.New("empty message")
		}
		out, err := exec.CommandContext(ctx, a.self, append(cmdArgs, "--", args.Text)...).CombinedOutput()
		clean := strings.TrimSpace(ansiRE.ReplaceAllString(string(out), ""))
		if err != nil {
			return nil, errors.New(lastLines(clean, 3))
		}
		return clean, nil
	}
	return nil, fmt.Errorf("unknown request %q", op)
}

// Notices watches every spawn's status stream for what its beacon reports to
// the host (persona-agent: report_work_finished, request_input) and delivers
// each as it is appended. History from before the server started is skipped.
func (a *serverActions) Notices(ctx context.Context) <-chan server.Notice {
	out := make(chan server.Notice, 32)
	go func() {
		defer close(out)
		offsets := map[string]int64{}
		first := true
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			cs, err := instance.Current().List(ctx, "")
			if err == nil {
				for _, c := range cs {
					off, seen := offsets[c.Instance]
					evs, next, err := status.Read(c.Instance, off)
					if err != nil {
						continue
					}
					offsets[c.Instance] = next
					if first && !seen {
						continue // already there when the server started
					}
					for _, ev := range evs {
						if n, ok := beaconNotice(c, ev); ok {
							select {
							case out <- n:
							case <-ctx.Done():
								return
							}
						}
					}
				}
				first = false
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return out
}

// beaconNotice is the notice for a status event a beacon sent, if any.
func beaconNotice(c instance.Container, ev status.Event) (server.Notice, bool) {
	if ev.Harness != "beacon" {
		return server.Notice{}, false
	}
	n := server.Notice{Spawn: c.Instance, Persona: c.Persona, Namespace: instance.NamespaceOf(c), TS: ev.TS}
	switch ev.Event {
	case status.EventTaskDone:
		n.Kind = "finished"
	case status.EventQuestion:
		n.Kind = "input"
		if len(ev.Questions) > 0 {
			n.Question = ev.Questions[0].Question
			for _, o := range ev.Questions[0].Options {
				n.Options = append(n.Options, o.Label)
			}
		}
	default:
		return server.Notice{}, false
	}
	return n, true
}

// Attach is persona attach for a spawn, run under the server's pty.
func (a *serverActions) Attach(ctx context.Context, spawn string) (*exec.Cmd, error) {
	rt := instance.Current()
	c, err := runningSpawn(ctx, rt, spawn)
	if err != nil {
		return nil, err
	}
	cmd := rt.AttachCommand(ctx, c.Name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil // the pty's
	return cmd, nil
}

// Job is a persona CLI command whose output the page shows live.
func (a *serverActions) Job(ctx context.Context, kind string, raw json.RawMessage) (*exec.Cmd, error) {
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Persona     string `json:"persona"`
		Namespace   string `json:"namespace"`
		Workdir     string `json:"workdir"`
		Prompt      string `json:"prompt"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
	}
	// Values go as --flag=value, so none can be taken for a flag.
	var argv []string
	switch kind {
	case "new":
		if strings.TrimSpace(args.Description) == "" {
			return nil, errors.New("a persona needs a description")
		}
		if err := persona.ValidateName(args.Name); err != nil {
			return nil, err
		}
		argv = []string{"new", "--yes", "--name=" + args.Name, "--description=" + args.Description}
	case "build":
		if err := persona.ValidateName(args.Name); err != nil {
			return nil, err
		}
		argv = []string{"build", args.Name}
	case "spawn":
		if err := persona.ValidateName(args.Persona); err != nil {
			return nil, err
		}
		argv = []string{"spawn", args.Persona}
		if args.Name != "" {
			if !spawnNameRE.MatchString(args.Name) {
				return nil, fmt.Errorf("invalid spawn name %q", args.Name)
			}
			argv = append(argv, "--name="+args.Name)
		}
		if args.Namespace != "" {
			argv = append(argv, "--namespace="+args.Namespace)
		}
		if args.Prompt != "" {
			argv = append(argv, "--prompt="+args.Prompt)
		}
		if args.Workdir != "" {
			argv = append(argv, "--workdir="+args.Workdir)
		} else {
			argv = append(argv, "--no-mount")
		}
	default:
		return nil, fmt.Errorf("unknown job %q", kind)
	}
	cmd := exec.CommandContext(ctx, a.self, argv...)
	cmd.Env = append(os.Environ(), "PERSONA_HOME="+config.Home())
	return cmd, nil
}

func runningSpawn(ctx context.Context, rt instance.Runtime, ref string) (instance.Container, error) {
	cs, err := instance.Resolve(ctx, rt, ref)
	if err != nil {
		return instance.Container{}, err
	}
	if len(cs) != 1 {
		return instance.Container{}, fmt.Errorf("%q matches %d spawns", ref, len(cs))
	}
	if !cs[0].Running() {
		return instance.Container{}, fmt.Errorf("%s is not running", cs[0].Instance)
	}
	return cs[0], nil
}
