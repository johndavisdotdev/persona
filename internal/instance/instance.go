// Package instance runs persona instances on a pluggable runtime: Docker
// containers, or Firecracker microVMs (see VM). The CLI and the control
// center only talk to the Runtime interface.
package instance

import (
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
}

// Runtime is where instances run.
type Runtime interface {
	Name() string
	Available(ctx context.Context) error
	// Run starts a detached instance and returns its name.
	Run(ctx context.Context, o RunOptions) (string, error)
	List(ctx context.Context, personaName string) ([]Container, error)
	Stop(ctx context.Context, name string) error
	// Remove deletes a stopped instance and what it leaves behind.
	Remove(ctx context.Context, name string) error
	// Exec runs a command as the image's user and returns its stdout.
	Exec(ctx context.Context, name string, args ...string) ([]byte, error)
	// AttachCommand attaches the terminal to Claude Code's tmux session.
	AttachCommand(ctx context.Context, name string) *exec.Cmd
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
	return docker.List(ctx, p)
}
func (Docker) Stop(ctx context.Context, name string) error   { return docker.Stop(ctx, name) }
func (Docker) Remove(ctx context.Context, name string) error { return docker.Remove(ctx, name) }
func (Docker) Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return docker.Exec(ctx, name, args...)
}
func (Docker) AttachCommand(ctx context.Context, name string) *exec.Cmd {
	return docker.AttachCommand(ctx, name)
}
func (Docker) Run(ctx context.Context, o RunOptions) (string, error) {
	return docker.Run(ctx, docker.RunOptions{Persona: o.Persona, Instance: o.Instance, Workdir: o.Workdir,
		Env: o.Env, ClaudeArgs: o.ClaudeArgs, Prompt: o.Prompt, Cols: o.Cols, Rows: o.Rows})
}
