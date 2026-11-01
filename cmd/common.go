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
	"github.com/johndavisdev/persona/internal/instance"
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
	if b, err := persona.LoadBase(); err == nil {
		d.Harness = persona.EffectiveHarness(&persona.Persona{}, b)
	}
	if useExa {
		d.Exa = &ai.Exa{APIKey: viper.GetString(config.KeyExaAPIKey)}
	}
	return d
}

// exportError is a microVM disk export that failed after the image itself
// built: the build is fine, the persona just can't be spawned as a microVM
// until the export works.
type exportError struct{ err error }

func (e *exportError) Error() string {
	return "exporting the image to a microVM disk failed: " + e.err.Error()
}
func (e *exportError) Unwrap() error { return e.err }

// exportDisk exports a persona's image to its microVM disk, so spawns start
// from it without waiting (runtimes without disks do nothing). Every build,
// update and customize ends with it; a failure fails the command.
func exportDisk(ctx context.Context, name, image string, out io.Writer) error {
	p, ok := instance.Current().(instance.Preparer)
	if !ok {
		return nil
	}
	start := time.Now()
	if err := p.Prepare(ctx, name, image); err != nil {
		return &exportError{err}
	}
	ui.Success(out, fmt.Sprintf("Exported the microVM disk in %s", time.Since(start).Round(100*time.Millisecond)))
	return nil
}

// buildImage builds a persona image, streaming build output dimmed, and
// exports it to its microVM disk.
func buildImage(ctx context.Context, name string, refresh bool, out io.Writer) error {
	if err := docker.Available(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, ui.Heading.Render("Building image "+persona.ImageFor(name)))
	start := time.Now()
	if err := docker.Build(ctx, docker.BuildOptions{Name: name, Refresh: refresh, Out: &dimWriter{w: out}}); err != nil {
		return err
	}
	ui.Success(out, fmt.Sprintf("Built %s in %s", persona.ImageFor(name), time.Since(start).Round(time.Second)))
	return exportDisk(ctx, name, persona.ImageFor(name), out)
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
	cs, _ := instance.Current().List(cmd.Context(), "")
	var out []string
	for _, c := range cs {
		out = append(out, c.Name+"\t"+c.Persona+" ("+c.State+")")
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// pickContainer resolves a reference to exactly one instance, prompting if needed.
func pickContainer(ctx context.Context, ref string, runningOnly bool) (instance.Container, error) {
	rt := instance.Current()
	var cs []instance.Container
	var err error
	if ref == "" {
		cs, err = rt.List(ctx, "")
	} else {
		cs, err = instance.Resolve(ctx, rt, ref)
	}
	if err != nil {
		return instance.Container{}, err
	}
	if runningOnly {
		var r []instance.Container
		for _, c := range cs {
			if c.Running() {
				r = append(r, c)
			}
		}
		cs = r
	}
	switch {
	case len(cs) == 0:
		return instance.Container{}, fmt.Errorf("%w: nothing running (start one with `persona spawn <persona>`)", instance.ErrNoMatch)
	case len(cs) == 1:
		return cs[0], nil
	case !ui.Interactive():
		var names []string
		for _, c := range cs {
			names = append(names, c.Name)
		}
		return instance.Container{}, fmt.Errorf("%q matches several instances, pick one: %s", ref, strings.Join(names, ", "))
	}
	var opts []huh.Option[string]
	for _, c := range cs {
		opts = append(opts, huh.NewOption(fmt.Sprintf("%s  (%s)", c.Name, c.Status), c.Name))
	}
	var chosen string
	if err := huh.NewSelect[string]().Title("Which instance?").Options(opts...).Value(&chosen).Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return instance.Container{}, context.Canceled
		}
		return instance.Container{}, err
	}
	for _, c := range cs {
		if c.Name == chosen {
			return c, nil
		}
	}
	return instance.Container{}, instance.ErrNoMatch
}

// waitForSession waits until Claude Code's tmux session exists in the instance.
func waitForSession(ctx context.Context, container string, timeout time.Duration) error {
	rt := instance.Current()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := rt.Exec(ctx, container, "tmux", "has-session", "-t", persona.SessionName); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			hint := "check `docker logs " + container + "`"
			if rt.Name() != "docker" {
				hint = "the VM console log is under /srv/jailer/persona-users"
			}
			return fmt.Errorf("timed out waiting for the Claude Code session in %s (%s)", container, hint)
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

// reviewCustomInstalls shows install commands the designer wrote from a web
// page (custom toolchains). With ask, it confirms before keeping them (they
// run as root inside the image build); declined ones are dropped.
func reviewCustomInstalls(p *persona.Persona, out io.Writer, ask bool) error {
	custom := persona.CustomInstalls(p.Toolchains)
	if len(custom) == 0 {
		return nil
	}
	for _, t := range custom {
		fmt.Fprintln(out, ui.Heading.Render("Install commands for "+t.Name)+ui.Dim.Render("  (from "+t.Source+", run as root at build time)"))
		for _, line := range strings.Split(strings.TrimSpace(t.Install), "\n") {
			fmt.Fprintln(out, ui.Dim.Render("  │ ")+line)
		}
	}
	if !ask || !ui.Interactive() {
		return nil
	}
	ok, err := confirm("Use these install commands?", true)
	if err != nil || ok {
		return err
	}
	var kept []persona.Toolchain
	for _, t := range p.Toolchains {
		if t.Install == "" {
			kept = append(kept, t)
			continue
		}
		ui.Warning(out, "dropped toolchain "+t.Name+"; add it later with `persona edit "+p.Name+" --spec` (for example with nix: <attribute>)")
	}
	p.Toolchains = kept
	return nil
}
