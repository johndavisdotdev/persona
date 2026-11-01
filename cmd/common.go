package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/ai"
	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

func newDesigner(useExa bool, out io.Writer) *ai.Designer {
	d := &ai.Designer{
		Client: &ai.Client{
			APIKey:  viper.GetString(config.KeyAnthropicAPIKey),
			BaseURL: viper.GetString(config.KeyAnthropicURL),
			Model:   viper.GetString(config.KeyModel),
		},
		Catalog:      ai.NewCatalog(),
		Marketplaces: designerMarketplaces(),
		Preinstalled: preinstalledPlugins(),
		Progress:     func(s string) { ui.Step(out, s) },
	}
	if useExa {
		d.Exa = &ai.Exa{APIKey: viper.GetString(config.KeyExaAPIKey)}
	}
	return d
}

// buildImage builds a persona image, streaming docker output dimmed.
func buildImage(ctx context.Context, name string, noCache bool, out io.Writer) error {
	if err := docker.Available(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, ui.Heading.Render("Building image "+persona.ImageFor(name)))
	start := time.Now()
	if err := docker.Build(ctx, name, noCache, &dimWriter{w: out}); err != nil {
		return err
	}
	ui.Success(out, fmt.Sprintf("Built %s in %s", persona.ImageFor(name), time.Since(start).Round(time.Second)))
	return nil
}

// dimWriter renders streamed build output in a muted color, line by line.
type dimWriter struct {
	w   io.Writer
	buf []byte
}

func (d *dimWriter) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	for {
		i := strings.IndexByte(string(d.buf), '\n')
		if i < 0 {
			break
		}
		fmt.Fprintln(d.w, ui.Dim.Render("  │ "+string(d.buf[:i])))
		d.buf = d.buf[i+1:]
	}
	return len(p), nil
}

func confirm(title string, def bool) (bool, error) {
	if !ui.Interactive() {
		return def, nil
	}
	v := def
	err := huh.NewConfirm().Title(title).Value(&v).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return false, context.Canceled
	}
	return v, err
}

// completePersonas provides shell completion for persona names.
func completePersonas(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if err := config.Init(cfgFile, ""); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ps, _ := persona.List()
	var out []string
	for _, p := range ps {
		out = append(out, p.Name+"\t"+p.Summary)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completeContainers provides shell completion for instances and personas.
func completeContainers(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	if err := config.Init(cfgFile, ""); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	cs, _ := docker.List(cmd.Context(), "")
	var out []string
	for _, c := range cs {
		out = append(out, c.Name+"\t"+c.Persona+" ("+c.State+")")
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// pickContainer resolves a reference to exactly one container, prompting if needed.
func pickContainer(ctx context.Context, ref string, runningOnly bool) (docker.Container, error) {
	var cs []docker.Container
	var err error
	if ref == "" {
		cs, err = docker.List(ctx, "")
	} else {
		cs, err = docker.Resolve(ctx, ref)
	}
	if err != nil {
		return docker.Container{}, err
	}
	if runningOnly {
		var r []docker.Container
		for _, c := range cs {
			if c.Running() {
				r = append(r, c)
			}
		}
		cs = r
	}
	switch {
	case len(cs) == 0:
		return docker.Container{}, fmt.Errorf("%w: nothing running (start one with `persona spawn <persona>`)", docker.ErrNoMatch)
	case len(cs) == 1:
		return cs[0], nil
	case !ui.Interactive():
		var names []string
		for _, c := range cs {
			names = append(names, c.Name)
		}
		return docker.Container{}, fmt.Errorf("%q matches several containers, pick one: %s", ref, strings.Join(names, ", "))
	}
	var opts []huh.Option[string]
	for _, c := range cs {
		opts = append(opts, huh.NewOption(fmt.Sprintf("%s  (%s)", c.Name, c.Status), c.Name))
	}
	var chosen string
	if err := huh.NewSelect[string]().Title("Which container?").Options(opts...).Value(&chosen).Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return docker.Container{}, context.Canceled
		}
		return docker.Container{}, err
	}
	for _, c := range cs {
		if c.Name == chosen {
			return c, nil
		}
	}
	return docker.Container{}, docker.ErrNoMatch
}

// waitForSession waits until Claude Code's tmux session exists in the container.
func waitForSession(ctx context.Context, container string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := docker.Exec(ctx, container, "tmux", "has-session", "-t", persona.SessionName); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for the Claude Code session in %s (check `docker logs %s`)", container, container)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// designerMarketplaces are the configured built-ins plus every marketplace the
// base settings and base requirements declare.
func designerMarketplaces() []string {
	srcs := viper.GetStringSlice(config.KeyMarketplaces)
	if bs, err := persona.LoadBaseSettings(); err == nil {
		_, mps := persona.SettingsPlugins(bs)
		srcs = append(srcs, mps...)
	}
	if b, err := persona.LoadBase(); err == nil {
		for _, m := range b.Marketplaces {
			srcs = append(srcs, m.Source)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range srcs {
		if n := ai.NormalizeSource(s); !seen[n] {
			seen[n] = true
			out = append(out, s)
		}
	}
	return out
}

func preinstalledPlugins() []string {
	var ids []string
	if bs, err := persona.LoadBaseSettings(); err == nil {
		ids, _ = persona.SettingsPlugins(bs)
	}
	if b, err := persona.LoadBase(); err == nil {
		for _, p := range b.Plugins {
			ids = append(ids, p.ID())
		}
	}
	return ids
}
