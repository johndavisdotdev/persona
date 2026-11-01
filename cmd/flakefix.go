package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/ai"
	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

// logTail is how much of a failed build's output the repair agent sees.
const logTail = 16 << 10

// buildWithRepair builds a persona's image; if that fails and an Anthropic API
// key is configured, a Claude agent reads the log, edits flake.nix and
// rebuilds, at most ai.MaxFlakeRebuilds times. If it can't fix the build,
// the original flake.nix is put back (its last attempt is kept as
// flake.nix.attempt) and the original error is returned.
func buildWithRepair(ctx context.Context, name string, repair bool, out io.Writer) error {
	tail := &tailWriter{max: logTail}
	err := buildImage(ctx, name, false, io.MultiWriter(out, tail))
	var exportErr *exportError
	if err == nil || !repair || ctx.Err() != nil || errors.As(err, &exportErr) {
		return err // (an export failure isn't the flake's)
	}
	apiKey := viper.GetString(config.KeyAnthropicAPIKey)
	if apiKey == "" {
		return err
	}
	dir := persona.Dir(name)
	flakePath := filepath.Join(dir, persona.FileFlake)
	orig, rerr := os.ReadFile(flakePath)
	if rerr != nil {
		return err
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Heading.Render(fmt.Sprintf("Build failed: asking Claude to fix flake.nix (up to %d rebuilds)", ai.MaxFlakeRebuilds)))
	fixer := &ai.FlakeFixer{
		Client: &ai.Client{
			APIKey:  apiKey,
			BaseURL: viper.GetString(config.KeyAnthropicURL),
			Model:   viper.GetString(config.KeyModel),
		},
		Progress: func(s string) { ui.Step(out, s) },
	}
	readable := []string{persona.FileSetupRoot, persona.FileSetupUser, "persona.yaml"}
	var exportFailed error
	in := ai.FlakeFixInput{
		Name:  name,
		Flake: string(orig),
		Log:   tail.String() + "\nerror: " + err.Error(),
		Rebuild: func(ctx context.Context, flake string) (string, error) {
			if err := os.WriteFile(flakePath, []byte(flake), 0o644); err != nil {
				return "", err
			}
			t := &tailWriter{max: logTail}
			berr := buildImage(ctx, name, false, io.MultiWriter(out, t))
			if errors.As(berr, &exportErr) {
				// The flake built; only the export failed. Report that after.
				exportFailed = berr
				return "", nil
			}
			if berr != nil {
				return t.String() + "\nerror: " + berr.Error(), berr
			}
			return "", nil
		},
		ReadableFiles: readable,
		ReadFile: func(file string) (string, error) {
			for _, r := range readable {
				if r == file {
					b, err := os.ReadFile(filepath.Join(dir, file))
					return string(b), err
				}
			}
			return "", fmt.Errorf("can't read %q", file)
		},
	}
	if docker.HostNix() != "" {
		in.CheckAttrs = func(ctx context.Context, attrs []string) (string, error) { return checkNixAttrs(ctx, dir, attrs) }
	}

	res, ferr := fixer.Fix(ctx, in)
	if res.Fixed {
		ui.Success(out, fmt.Sprintf("Claude fixed the build after %d rebuild(s): %s", res.Rebuilds, res.Summary))
		ui.Step(out, "flake.nix now differs from the generated one; `persona update` regenerates it")
		return exportFailed
	}
	// Not fixed: restore what was generated, keep the last attempt for reference.
	if cur, _ := os.ReadFile(flakePath); !bytes.Equal(cur, orig) {
		_ = os.WriteFile(flakePath+".attempt", cur, 0o644)
		_ = os.WriteFile(flakePath, orig, 0o644)
		ui.Step(out, "Restored the original flake.nix; Claude's last attempt is in flake.nix.attempt")
	}
	switch {
	case ferr != nil:
		ui.Warning(out, "the repair agent failed: "+ferr.Error())
	case res.Summary != "":
		ui.Warning(out, fmt.Sprintf("Claude could not fix the build (%d rebuild(s)): %s", res.Rebuilds, res.Summary))
	default:
		ui.Warning(out, fmt.Sprintf("Claude could not fix the build (%d rebuild(s))", res.Rebuilds))
	}
	return err
}

var nixAttrRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_'+-]*(\.[A-Za-z_][A-Za-z0-9_'+-]*)*$`)

// checkNixExpr reports each attribute path in PERSONA_ATTRS (newline
// separated) against the nixpkgs locked by the flake in PERSONA_FLAKE.
const checkNixExpr = `
let
  flake = builtins.getFlake ("path:" + builtins.getEnv "PERSONA_FLAKE");
  pkgs = flake.inputs.nixpkgs.legacyPackages.${builtins.currentSystem};
  lib = pkgs.lib;
  attrs = lib.filter (a: a != "") (lib.splitString "\n" (builtins.getEnv "PERSONA_ATTRS"));
  check = p:
    let r = builtins.tryEval (lib.attrByPath (lib.splitString "." p) null pkgs);
    in if !r.success then "error: evaluating it throws"
    else if r.value == null then "missing"
    else if !(lib.isDerivation r.value) then "exists (not a package: " + builtins.typeOf r.value + ")"
    else
      let
        d = builtins.tryEval r.value.drvPath;
        v = builtins.tryEval (r.value.version or r.value.name or "?");
      in (if d.success then "ok" else "unusable (broken, insecure, unfree or unsupported)")
        + (if v.success then " " + toString v.value else "");
in lib.genAttrs attrs check
`

// checkNixAttrs evaluates attribute paths in the persona's locked nixpkgs.
func checkNixAttrs(ctx context.Context, dir string, attrs []string) (string, error) {
	var ok []string
	var b strings.Builder
	for _, a := range attrs {
		if nixAttrRE.MatchString(a) {
			ok = append(ok, a)
		} else {
			fmt.Fprintf(&b, "%s: not an attribute path\n", a)
		}
	}
	if len(ok) == 0 {
		return b.String(), nil
	}
	cmd := exec.CommandContext(ctx, docker.HostNix(), "--extra-experimental-features", "nix-command flakes",
		"eval", "--impure", "--json", "--expr", checkNixExpr)
	cmd.Env = append(os.Environ(), "PERSONA_FLAKE="+dir, "PERSONA_ATTRS="+strings.Join(ok, "\n"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	res, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix eval: %v: %s", err, lastLines(stderr.String(), 20))
	}
	b.Write(res)
	return b.String(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

// String is the tail without terminal styling.
func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return ansiRE.ReplaceAllString(string(t.buf), "")
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
