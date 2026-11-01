// Package instance runs persona instances on a pluggable runtime: Docker
// containers, or Firecracker microVMs (see VM). The CLI and the control
// center only talk to the Runtime interface.
package instance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"

	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/status"
)

// Container is one running (or stopped) instance, whatever the runtime.
type Container = docker.Container

// ErrNoMatch is returned when no instance matches a reference.
var ErrNoMatch = docker.ErrNoMatch

// RunOptions configures a new instance.
type RunOptions struct {
	Persona    string
	Instance   string
	Image      string   // image to run; default persona.ImageFor(Persona)
	Workdir    string   // host directory for /workspace; empty for none
	Env        []string // NAME (forwarded from the host) or NAME=VALUE
	ClaudeArgs string
	Prompt     string
	Cols, Rows int
	Log        io.Writer // progress messages
	// Namespace is the persona network namespace to run in ("" is
	// DefaultNamespace). Only the firecracker runtime has others.
	Namespace string
}

// DefaultNamespace always exists; it is the only one Docker has.
const DefaultNamespace = "default"

// Namespacer is a runtime with persona network namespaces: separate
// networks whose spawns reach each other and nothing in the others.
type Namespacer interface {
	// Namespaces lists every namespace, DefaultNamespace first.
	Namespaces(ctx context.Context) ([]string, error)
	CreateNamespace(ctx context.Context, name string) error
	// RemoveNamespace fails while any spawn, running or stopped, is in it.
	RemoveNamespace(ctx context.Context, name string) error
}

// Namespaces lists rt's namespaces: just DefaultNamespace without support.
func Namespaces(ctx context.Context, rt Runtime) ([]string, error) {
	if n, ok := rt.(Namespacer); ok {
		return n.Namespaces(ctx)
	}
	return []string{DefaultNamespace}, nil
}

// ExecResult is what a command run in a spawn produced.
type ExecResult struct {
	Stdout, Stderr []byte
	Exit           int
}

// ErrNotStarted wraps the reason a command couldn't be started at all (not
// found, not executable); persona exec reports it as exit status 127.
type ErrNotStarted struct{ Msg string }

func (e *ErrNotStarted) Error() string { return e.Msg }

// Commander is a runtime that runs one-off commands in a spawn, beside its
// Claude Code session.
type Commander interface {
	// RunCommand runs argv in the running spawn ref as the session user, in
	// its working directory and with its environment, and returns its output
	// and exit status (128+N if killed by signal N).
	RunCommand(ctx context.Context, ref string, argv []string, stdin []byte) (ExecResult, error)
}

// PortForward exposes a spawn's port on a host address.
type PortForward struct {
	ID        int    `json:"id"`
	Spawn     string `json:"spawn"` // the spawn's name, or its VM id if it is gone
	Persona   string `json:"persona,omitempty"`
	Namespace string `json:"namespace"`
	Proto     string `json:"proto"` // tcp or udp
	HostIP    string `json:"host_ip"`
	HostPort  int    `json:"host_port"`
	Port      int    `json:"port"` // the spawn's port
}

// Forwarder is a runtime that can forward host ports to spawns.
type Forwarder interface {
	// AddForward forwards hostIP:hostPort (tcp or udp) to port of the running
	// spawn ref, and returns the new rule.
	AddForward(ctx context.Context, ref, proto, hostIP string, hostPort, port int) (PortForward, error)
	// Forwards lists this user's forwards.
	Forwards(ctx context.Context) ([]PortForward, error)
	RemoveForward(ctx context.Context, id int) error
}

// NamespaceOf is c's namespace, DefaultNamespace when unset.
func NamespaceOf(c Container) string {
	if c.Namespace == "" {
		return DefaultNamespace
	}
	return c.Namespace
}

// Runtime is where instances run.
type Runtime interface {
	Name() string
	Available(ctx context.Context) error
	// Run starts a detached instance and returns its name.
	Run(ctx context.Context, o RunOptions) (string, error)
	List(ctx context.Context, personaName string) ([]Container, error)
	// Stop stops a spawn, keeping it so Start can resume it.
	Stop(ctx context.Context, name string) error
	// Start resumes a stopped spawn. env is what Run's Env would be now
	// (forwarded names and credentials); runtimes that fix env at creation
	// ignore it.
	Start(ctx context.Context, name string, env []string) error
	// Remove kills a spawn, running or not, and deletes what it leaves behind.
	Remove(ctx context.Context, name string) error
	// Save captures a running spawn's filesystem (not /workspace) as the image
	// of persona newPersona. The spawn keeps running.
	Save(ctx context.Context, name, newPersona string) error
	// Exec runs a command as the image's user and returns its stdout.
	Exec(ctx context.Context, name string, args ...string) ([]byte, error)
	// AttachCommand attaches the terminal to Claude Code's tmux session.
	AttachCommand(ctx context.Context, name string) *exec.Cmd
}

// Preparer is a runtime that does per-image work ahead of time (the
// firecracker runtime exports the image to a microVM disk), so spawning
// right after a build doesn't wait for it.
type Preparer interface {
	Prepare(ctx context.Context, personaName, image string) error
}

// ImageForgetter is a runtime that keeps per-image artifacts (the firecracker
// runtime's microVM disks) to delete along with a persona.
type ImageForgetter interface {
	ForgetImages(personaName string) error
}

// Current returns the configured runtime.
func Current() Runtime {
	if viper.GetString(config.KeyRuntime) == "firecracker" {
		return firecrackerRuntime()
	}
	return Docker{}
}

// Resolve finds instances by name, id, instance id, or persona name.
func Resolve(ctx context.Context, rt Runtime, ref string) ([]Container, error) {
	all, err := rt.List(ctx, "")
	if err != nil {
		return nil, err
	}
	var exact, byPersona []Container
	for _, c := range all {
		switch {
		case c.Name == ref || c.ID == ref || strings.HasPrefix(c.ID, ref) && len(ref) >= 6 || c.Instance == ref:
			exact = append(exact, c)
		case c.Persona == ref:
			byPersona = append(byPersona, c)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	if len(byPersona) > 0 {
		return byPersona, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrNoMatch, ref)
}

type forceKey struct{}

// WithForce marks ctx so Stop skips saving the session's work (for VMs that
// no longer respond). Their work is lost.
func WithForce(ctx context.Context) context.Context { return context.WithValue(ctx, forceKey{}, true) }

func forced(ctx context.Context) bool { b, _ := ctx.Value(forceKey{}).(bool); return b }

// KeptError reports that an instance was removed but left work behind on
// purpose, such as a worktree with uncommitted changes.
type KeptError struct{ Msg string }

func (e *KeptError) Error() string { return e.Msg }

// IsKept reports whether err is a KeptError.
func IsKept(err error) bool {
	var k *KeptError
	return errors.As(err, &k)
}

func sortContainers(cs []Container) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
}

// Docker runs instances as Docker containers.
type Docker struct{}

func (Docker) Name() string                        { return "docker" }
func (Docker) Available(ctx context.Context) error { return docker.Available(ctx) }
func (Docker) List(ctx context.Context, p string) ([]Container, error) {
	cs, err := docker.List(ctx, p)
	for i := range cs {
		cs[i].Namespace = DefaultNamespace
	}
	return cs, err
}
func (Docker) Stop(ctx context.Context, name string) error { return docker.Stop(ctx, name) }
func (Docker) Start(ctx context.Context, name string, _ []string) error {
	return docker.Start(ctx, name)
}
func (Docker) Remove(ctx context.Context, name string) error {
	spawn := ""
	if cs, err := docker.List(ctx, ""); err == nil {
		for _, c := range cs {
			if c.Name == name {
				spawn = c.Instance
			}
		}
	}
	if err := docker.Remove(ctx, name); err != nil {
		return err
	}
	if spawn != "" {
		_ = status.Remove(spawn)
	}
	return nil
}

// Save commits the container; its /workspace is a bind mount, so it isn't
// part of the image.
func (Docker) Save(ctx context.Context, name, newPersona string) error {
	return docker.Commit(ctx, name, persona.ImageFor(newPersona),
		[]string{fmt.Sprintf("LABEL %s=%q %s=%q", persona.LabelManaged, "true", persona.LabelName, newPersona)})
}
func (Docker) Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return docker.Exec(ctx, name, args...)
}

func (d Docker) RunCommand(ctx context.Context, ref string, argv []string, stdin []byte) (ExecResult, error) {
	c, err := resolveRunning(ctx, d, ref)
	if err != nil {
		return ExecResult{}, err
	}
	cmd := docker.Command(ctx, append([]string{"exec", "-i", c.Name}, argv...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(stdin), &stdout, &stderr
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
	case errors.As(err, &ee):
		// docker exec exits 126/127 itself when the command can't be run.
		return ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Exit: ee.ExitCode()}, nil
	}
	return ExecResult{}, err
}

// resolveRunning finds exactly one running spawn by reference.
func resolveRunning(ctx context.Context, rt Runtime, ref string) (Container, error) {
	cs, err := Resolve(ctx, rt, ref)
	if err != nil {
		return Container{}, err
	}
	if len(cs) != 1 {
		return Container{}, fmt.Errorf("%q matches %d spawns; use a spawn's name", ref, len(cs))
	}
	if !cs[0].Running() {
		return Container{}, fmt.Errorf("%s is not running (persona start %s)", cs[0].Instance, cs[0].Instance)
	}
	return cs[0], nil
}
func (Docker) AttachCommand(ctx context.Context, name string) *exec.Cmd {
	return docker.AttachCommand(ctx, name)
}
func (Docker) Run(ctx context.Context, o RunOptions) (string, error) {
	if o.Namespace != "" && o.Namespace != DefaultNamespace {
		return "", fmt.Errorf("namespaces need the firecracker runtime; Docker spawns are all in %q", DefaultNamespace)
	}
	dir, err := status.Prepare(o.Instance)
	if err != nil {
		return "", err
	}
	return docker.Run(ctx, docker.RunOptions{Persona: o.Persona, Instance: o.Instance, Workdir: o.Workdir, StatusDir: dir,
		Env: o.Env, ClaudeArgs: o.ClaudeArgs, Prompt: o.Prompt, Cols: o.Cols, Rows: o.Rows})
}
