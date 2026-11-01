package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/persona"
)

// A persona image is built in two stages:
//
//  1. Nix builds flake.nix into persona/NAME:base (dockerTools.buildLayeredImage),
//     with the host's Nix if it has one, else in a container (KeyNixImage)
//     whose store persists in the persona-nix volume.
//  2. A setup container from that image runs setup-root.sh and setup-user.sh
//     (the harness, plugins, tools Nix can't express) and is committed as
//     persona/NAME:latest.

// BaseImageFor is the Nix-built stage of a persona's image.
func BaseImageFor(name string) string { return "persona/" + name + ":base" }

// nixVolume keeps the container builder's /nix between builds.
const nixVolume = "persona-nix"

// BuildOptions configures Build.
type BuildOptions struct {
	Name string
	// Refresh updates flake.lock to the newest nixpkgs before building.
	Refresh bool
	Out     io.Writer // build output
}

// Build builds a persona's image from its flake and setup scripts.
func Build(ctx context.Context, o BuildOptions) error {
	p, err := persona.Load(o.Name)
	if err != nil {
		return err
	}
	if p.Frozen() {
		return persona.ErrFrozen(p)
	}
	if _, err := os.Stat(persona.FlakePath(p.Name)); errors.Is(err, os.ErrNotExist) {
		// Defined before images were built with Nix: generate its flake and setup scripts.
		fmt.Fprintln(o.Out, "== generating flake.nix and setup scripts (first Nix build of this persona)")
		if _, err := p.Generate(false); err != nil {
			return err
		}
	}
	fmt.Fprintln(o.Out, "== nix: building the base image from flake.nix")
	if err := nixBuild(ctx, o); err != nil {
		return err
	}
	fmt.Fprintln(o.Out, "== setup: setup-root.sh and setup-user.sh")
	return setup(ctx, p, o.Out)
}

// HostNix returns the host's nix command, or "".
func HostNix() string {
	if p, err := exec.LookPath("nix"); err == nil {
		return p
	}
	for _, p := range []string{"/nix/var/nix/profiles/default/bin/nix", "/run/current-system/sw/bin/nix"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

var nixFlags = []string{"--extra-experimental-features", "nix-command flakes", "--print-build-logs"}

func nixBuild(ctx context.Context, o BuildOptions) error {
	dir := persona.Dir(o.Name)
	flake := "path:" + dir + "#default"
	builder := viper.GetString(config.KeyNixBuilder)
	nix := HostNix()
	if builder == "container" || (builder != "host" && nix == "") {
		return nixBuildInContainer(ctx, o)
	}
	if nix == "" {
		return errors.New("nix_builder is host, but there is no nix command; install Nix or set nix_builder to auto")
	}
	if o.Refresh {
		cmd := exec.CommandContext(ctx, nix, append(nixFlags, "flake", "update", "--flake", "path:"+dir)...)
		cmd.Stdout, cmd.Stderr = o.Out, o.Out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("nix flake update: %w", err)
		}
	}
	link := filepath.Join(dir, ".image")
	cmd := exec.CommandContext(ctx, nix, append(nixFlags, "build", flake, "--out-link", link)...)
	cmd.Stdout, cmd.Stderr = o.Out, o.Out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nix build failed for %s: %w", o.Name, err)
	}
	f, err := os.Open(link)
	if err != nil {
		return err
	}
	defer f.Close()
	return load(ctx, f, o.Out)
}

// nixBuildInContainer builds with Nix in a container and streams the image
// tarball straight into docker load.
func nixBuildInContainer(ctx context.Context, o BuildOptions) error {
	dir := persona.Dir(o.Name)
	script := `set -e
nix() { command nix --extra-experimental-features 'nix-command flakes' --option sandbox false "$@"; }
` + optionalStr(o.Refresh, "nix flake update --flake path:/src >&2\n") + `out=$(nix build --print-build-logs --no-link --print-out-paths path:/src#default)
chown "$OWNER" /src/flake.lock 2>/dev/null || true
cat "$out"`
	fmt.Fprintf(o.Out, "building with Nix in %s (store kept in the %s volume)\n", viper.GetString(config.KeyNixImage), nixVolume)
	build := Command(ctx, "run", "--rm", "-i", "-v", nixVolume+":/nix", "-v", dir+":/src",
		"-e", fmt.Sprintf("OWNER=%d:%d", os.Getuid(), os.Getgid()),
		viper.GetString(config.KeyNixImage), "sh", "-c", script)
	build.Stderr = o.Out
	stdout, err := build.StdoutPipe()
	if err != nil {
		return err
	}
	if err := build.Start(); err != nil {
		return err
	}
	loadErr := load(ctx, stdout, o.Out)
	if err := build.Wait(); err != nil {
		return fmt.Errorf("nix build (in a container) failed for %s: %w", o.Name, err)
	}
	return loadErr
}

func load(ctx context.Context, r io.Reader, out io.Writer) error {
	cmd := Command(ctx, "load")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r, out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker load: %w", err)
	}
	return nil
}

// FinalChanges restores a persona image's runtime config on a commit (the
// setup and customize containers run with another user and entrypoint).
func FinalChanges(name string) []string {
	return []string{
		"USER " + persona.ImageUser,
		"WORKDIR " + persona.ImageWorkdir,
		`ENTRYPOINT ["` + persona.ImageEntrypoint + `"]`,
		"CMD []",
		"LABEL " + persona.LabelManaged + "=true " + persona.LabelName + "=" + name,
	}
}

func setup(ctx context.Context, p *persona.Persona, out io.Writer) error {
	ctr := "persona-setup-" + p.Name + "-" + randHex(3)
	if _, err := output(ctx, "run", "-d", "--name", ctr, "--user", "root", "--entrypoint", "sleep",
		BaseImageFor(p.Name), "infinity"); err != nil {
		return err
	}
	defer Command(context.WithoutCancel(ctx), "rm", "-f", ctr).Run()

	if _, err := output(ctx, "cp", persona.Dir(p.Name)+"/.", ctr+":"+persona.SetupDir); err != nil {
		return err
	}
	if mp := viper.GetString(config.KeyBeaconMarketplace); dirExists(mp) {
		if _, err := output(ctx, "cp", mp+"/.", ctr+":"+persona.SetupDir+"/marketplace"); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "warning: no persona marketplace at %s (set %s; `make persona-beacon` publishes one to bin/beacon/marketplace)\n",
			mp, config.KeyBeaconMarketplace)
	}
	if ms := viper.GetString(config.KeyManagedSettings); fileExists(ms) {
		if _, err := output(ctx, "cp", ms, ctr+":"+persona.SetupDir+"/managed-settings.json"); err != nil {
			return err
		}
	}

	run := func(user string, script string, env ...string) error {
		args := []string{"exec", "-u", user, "-w", "/"}
		for _, e := range env {
			args = append(args, "-e", e)
		}
		args = append(args, ctr, "bash", persona.SetupDir+"/"+script)
		cmd := Command(ctx, args...)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s failed: %w", script, err)
		}
		return nil
	}
	if err := run("root", persona.FileSetupRoot); err != nil {
		return err
	}
	// The token reaches the setup run by name, never on a command line or in a layer.
	env := []string{"HOME=" + persona.ImageHome, "USER=" + persona.ImageUser}
	token := githubToken(ctx)
	if token != "" {
		env = append(env, "GITHUB_TOKEN")
	}
	cmdEnv := os.Environ()
	if token != "" {
		cmdEnv = append(cmdEnv, "GITHUB_TOKEN="+token)
	}
	userRun := func() error {
		args := []string{"exec", "-u", persona.ImageUser, "-w", persona.ImageHome}
		for _, e := range env {
			args = append(args, "-e", e)
		}
		cmd := Command(ctx, append(args, ctr, "bash", persona.SetupDir+"/"+persona.FileSetupUser)...)
		cmd.Env = append(cmdEnv, "DOCKER_BUILDKIT=1")
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s failed: %w", persona.FileSetupUser, err)
		}
		return nil
	}
	if err := userRun(); err != nil {
		return err
	}
	if _, err := output(ctx, "exec", "-u", "root", ctr, "rm", "-rf", persona.SetupDir); err != nil {
		return err
	}
	return Commit(ctx, ctr, persona.ImageFor(p.Name), FinalChanges(p.Name))
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return p != "" && err == nil && fi.IsDir()
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return p != "" && err == nil && fi.Mode().IsRegular()
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func optionalStr(ok bool, s string) string {
	if ok {
		return s
	}
	return ""
}
