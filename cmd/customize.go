package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

func newCustomizeCmd() *cobra.Command {
	var root, yes bool
	var run string
	cmd := &cobra.Command{
		Use:   "customize <name>",
		Short: "Change a persona's image by hand in a shell, then commit it",
		Long: `Opens a shell in a container from the persona's image: configure the harness
interactively, edit settings, install packages Nix can't provide. When the
shell exits, persona shows what changed and commits the container as the
persona's new image (and prepares its microVM disk).

A customized persona is frozen: rebuilding it from flake.nix would lose the
hand changes, so ` + "`persona update`, `edit` and `build`" + ` refuse it from then on.
Customize it again to make more changes.

  persona customize web-dev                  # shell as claude
  persona customize web-dev --root           # shell as root
  persona customize web-dev --run 'npm i -g some-cli'`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completePersonas,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			p, err := persona.Load(args[0])
			if err != nil {
				return err
			}
			if err := docker.Available(ctx); err != nil {
				return err
			}
			if !docker.ImageExists(ctx, p.Image()) {
				return fmt.Errorf("%s has no image yet; build it with `persona build %s`", p.Name, p.Name)
			}
			if run == "" && !ui.Interactive() {
				return errors.New("customize needs a terminal, or --run")
			}
			return customize(ctx, p, root, run, yes, out)
		},
	}
	cmd.Flags().BoolVar(&root, "root", false, "run the shell (or --run) as root instead of claude")
	cmd.Flags().StringVar(&run, "run", "", "run this command instead of an interactive shell")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "commit the changes without asking")
	return cmd
}

// customizeNoise are paths every shell session touches; they don't count as changes.
var customizeNoise = []string{"/home/claude/.bash_history", "/root/.bash_history", "/tmp", "/run", "/home/claude/.cache", "/root/.cache"}

func customize(ctx context.Context, p *persona.Persona, root bool, run string, yes bool, out io.Writer) error {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	ctr := "persona-customize-" + p.Name + "-" + hex.EncodeToString(b)
	user, home := persona.ImageUser, persona.ImageHome
	if root {
		user, home = "root", "/root"
	}
	args := []string{"run", "--name", ctr, "--hostname", p.Name, "--user", user, "-w", home, "-e", "HOME=" + home,
		"--entrypoint", "bash"}
	if run != "" {
		args = append(args, "-i", p.Image(), "-lc", run)
	} else {
		args = append(args, "-it", p.Image(), "-l")
		fmt.Fprintln(out, ui.Heading.Render("Customizing "+p.Name))
		fmt.Fprintln(out, ui.Dim.Render("Make your changes, then exit the shell to review and commit them."))
	}
	sh := docker.Command(ctx, args...)
	sh.Stdin, sh.Stdout, sh.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := sh.Run()
	defer docker.Command(context.WithoutCancel(ctx), "rm", "-f", ctr).Run()
	if runErr != nil && run != "" {
		return fmt.Errorf("--run failed: %w", runErr)
	}

	diff, err := docker.Command(ctx, "diff", ctr).Output()
	if err != nil {
		return fmt.Errorf("docker diff: %w", err)
	}
	changes := meaningfulChanges(string(diff))
	if len(changes) == 0 {
		ui.Success(out, "No changes; "+p.Image()+" is unchanged")
		return nil
	}
	fmt.Fprintln(out, ui.Heading.Render(fmt.Sprintf("%d changed paths", len(changes))))
	for i, c := range changes {
		if i == 30 {
			fmt.Fprintln(out, ui.Dim.Render(fmt.Sprintf("  … and %d more", len(changes)-30)))
			break
		}
		fmt.Fprintln(out, ui.Dim.Render("  "+c))
	}
	if !yes {
		frozen := ""
		if p.Customized == nil && p.SavedFrom == nil {
			frozen = " (" + p.Name + " can then no longer be rebuilt from flake.nix)"
		}
		ok, err := confirm("Commit these changes as "+p.Image()+frozen+"?", true)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, ui.Dim.Render("Discarded."))
			return nil
		}
	}
	config := docker.FinalChanges(p.Name)
	for _, c := range changes {
		if strings.Contains(c, " /nix/store/") || strings.HasSuffix(c, " /nix/store") {
			// Its store no longer matches what Nix built: microVMs mustn't share it.
			config = append(config, "LABEL "+persona.LabelStore+"="+persona.StorePrivate)
			ui.Warning(out, "/nix/store was changed; this persona's microVM disk keeps its own copy of the store")
			break
		}
	}
	if err := docker.Commit(ctx, ctr, p.Image(), config); err != nil {
		return err
	}
	summary := strings.Join(changes[:min(5, len(changes))], ", ")
	if len(changes) > 5 {
		summary += fmt.Sprintf(", … (%d paths)", len(changes))
	}
	p.Customized = &persona.Customized{At: time.Now().UTC(), Summary: summary}
	if err := p.Save(); err != nil {
		return err
	}
	ui.Success(out, "Committed "+p.Image())
	if err := exportDisk(ctx, p.Name, p.Image(), out); err != nil {
		return err
	}
	fmt.Fprintln(out, ui.Dim.Render("Running spawns keep the old image; new spawns get this one."))
	return nil
}

// meaningfulChanges filters docker diff output ("C /path", "A /path") down
// to the leaf paths that aren't session noise.
func meaningfulChanges(diff string) []string {
	type entry struct{ kind, path string }
	var all []entry
	for _, l := range strings.Split(strings.TrimSpace(diff), "\n") {
		if kind, path, ok := strings.Cut(strings.TrimSpace(l), " "); ok {
			all = append(all, entry{kind, path})
		}
	}
	// Order "/" before every other byte so a directory's contents follow it directly.
	key := func(p string) string { return strings.ReplaceAll(p, "/", "\x00") }
	sort.Slice(all, func(i, j int) bool { return key(all[i].path) < key(all[j].path) })
	var lines []string
	for i, e := range all {
		noise := false
		for _, n := range customizeNoise {
			if e.path == n || strings.HasPrefix(e.path, n+"/") {
				noise = true
				break
			}
		}
		// A changed directory is implied by the changes inside it.
		parent := i+1 < len(all) && strings.HasPrefix(all[i+1].path, e.path+"/")
		if !noise && !parent {
			lines = append(lines, e.kind+" "+e.path)
		}
	}
	return lines
}
