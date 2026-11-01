// Package docker wraps the docker CLI. Shelling out keeps the binary small and
// works identically with Docker Desktop, OrbStack, Colima, and Podman's docker shim.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/persona"
)

func bin() string { return viper.GetString(config.KeyDockerBin) }

// Command builds an *exec.Cmd for the docker CLI.
func Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin(), args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	return cmd
}

func output(ctx context.Context, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := Command(ctx, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return out, nil
}

// Available verifies that the docker CLI exists and the daemon is reachable.
func Available(ctx context.Context) error {
	if _, err := exec.LookPath(bin()); err != nil {
		return fmt.Errorf("docker CLI %q not found in PATH: install Docker Desktop, OrbStack, or Colima", bin())
	}
	if _, err := output(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker daemon is not reachable: %w", err)
	}
	return nil
}

// Build builds the persona image, streaming progress to w.
func Build(ctx context.Context, name string, noCache bool, w io.Writer) error {
	args := []string{"build", "--progress=plain", "-t", persona.ImageFor(name), "-f", persona.DockerfilePath(name)}
	if noCache {
		args = append(args, "--no-cache")
	}
	token := githubToken(ctx)
	if token != "" {
		args = append(args, "--secret", "id=github_token,env=PERSONA_GITHUB_TOKEN")
	}
	args = append(args, persona.Dir(name))
	cmd := Command(ctx, args...)
	if token != "" {
		cmd.Env = append(cmd.Env, "PERSONA_GITHUB_TOKEN="+token)
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build failed for %s: %w", name, err)
	}
	return nil
}

// ImageExists reports whether the image tag exists locally.
func ImageExists(ctx context.Context, tag string) bool {
	return Command(ctx, "image", "inspect", tag).Run() == nil
}

// RemoveImage removes an image, ignoring "no such image".
func RemoveImage(ctx context.Context, tag string) error {
	if !ImageExists(ctx, tag) {
		return nil
	}
	_, err := output(ctx, "image", "rm", "-f", tag)
	return err
}

// RunOptions configures a new persona container.
type RunOptions struct {
	Persona    string
	Instance   string
	Workdir    string   // host directory mounted at /workspace; empty for none
	Env        []string // NAME (forwarded from host) or NAME=VALUE
	ClaudeArgs string
	Prompt     string
	Cols, Rows int
}

// ContainerName is the docker container name for an instance.
func ContainerName(personaName, instance string) string {
	return "persona-" + personaName + "-" + instance
}

// Run starts a detached persona container and returns its name.
func Run(ctx context.Context, o RunOptions) (string, error) {
	name := ContainerName(o.Persona, o.Instance)
	args := []string{"run", "-d", "-t", "--init",
		"--name", name,
		"--hostname", o.Persona,
		"--label", persona.LabelManaged + "=true",
		"--label", persona.LabelName + "=" + o.Persona,
		"--label", persona.LabelInstance + "=" + o.Instance,
	}
	if o.Workdir != "" {
		args = append(args, "--mount", "type=bind,source="+o.Workdir+",target=/workspace")
	}
	for _, e := range o.Env {
		args = append(args, "-e", e)
	}
	if o.ClaudeArgs != "" {
		args = append(args, "-e", "PERSONA_CLAUDE_ARGS="+o.ClaudeArgs)
	}
	if o.Prompt != "" {
		args = append(args, "-e", "PERSONA_PROMPT="+o.Prompt)
	}
	if o.Cols > 0 && o.Rows > 0 {
		args = append(args, "-e", fmt.Sprintf("PERSONA_COLS=%d", o.Cols), "-e", fmt.Sprintf("PERSONA_ROWS=%d", o.Rows))
	}
	args = append(args, persona.ImageFor(o.Persona))
	if _, err := output(ctx, args...); err != nil {
		return "", err
	}
	return name, nil
}

// Container is a persona container as reported by `docker ps`.
type Container struct {
	ID       string
	Name     string
	Persona  string
	Instance string
	State    string
	Status   string
	Image    string
	Created  string
}

// Running reports whether the container is running.
func (c Container) Running() bool { return c.State == "running" }

type psRow struct {
	ID, Names, Image, State, Status, Labels, CreatedAt string
}

// List returns all persona-managed containers, optionally filtered by persona.
func List(ctx context.Context, personaName string) ([]Container, error) {
	args := []string{"ps", "-a", "--no-trunc", "--filter", "label=" + persona.LabelManaged + "=true", "--format", "{{json .}}"}
	if personaName != "" {
		args = append(args, "--filter", "label="+persona.LabelName+"="+personaName)
	}
	out, err := output(ctx, args...)
	if err != nil {
		return nil, err
	}
	var cs []Container
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r psRow
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("parse docker ps output: %w", err)
		}
		labels := parseLabels(r.Labels)
		cs = append(cs, Container{
			ID: r.ID, Name: r.Names, Image: r.Image, State: r.State, Status: r.Status, Created: r.CreatedAt,
			Persona: labels[persona.LabelName], Instance: labels[persona.LabelInstance],
		})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	return cs, sc.Err()
}

func parseLabels(s string) map[string]string {
	m := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// ErrNoMatch is returned when no container matches a reference.
var ErrNoMatch = errors.New("no matching persona container")

// Resolve finds containers by container name, instance id, or persona name.
func Resolve(ctx context.Context, ref string) ([]Container, error) {
	all, err := List(ctx, "")
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

// Stop stops a container.
func Stop(ctx context.Context, name string) error {
	_, err := output(ctx, "stop", "-t", "5", name)
	return err
}

// Remove force-removes a container.
func Remove(ctx context.Context, name string) error {
	_, err := output(ctx, "rm", "-f", name)
	return err
}

// Exec runs a non-interactive command in a container and returns stdout.
func Exec(ctx context.Context, container string, args ...string) ([]byte, error) {
	return output(ctx, append([]string{"exec", container}, args...)...)
}

// AttachCommand returns the command that attaches a TTY to the container's
// Claude Code tmux session.
func AttachCommand(ctx context.Context, container string) *exec.Cmd {
	cmd := Command(ctx, "exec", "-it", "-e", "TERM=xterm-256color", container, "tmux", "-u", "attach-session", "-t", persona.SessionName)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

// githubToken finds a GitHub token for cloning private marketplaces and skills
// at build time: $GITHUB_TOKEN, $GH_TOKEN, then `gh auth token`.
func githubToken(ctx context.Context) string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
