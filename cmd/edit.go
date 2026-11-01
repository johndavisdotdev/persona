package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/johndavisdev/persona/internal/editor"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

func newEditCmd() *cobra.Command {
	var spec, settings, noBuild, build bool
	var file string
	cmd := &cobra.Command{
		Use:   "edit <name>",
		Short: "Hand-edit a persona's flake.nix, setup scripts, settings.json, or definition (--spec)",
		Long: `A persona's image is defined by generated files, which you can edit by hand:

  flake.nix       (default)          the Nix base image: packages and toolchains
  setup-root.sh   (--file setup-root.sh)
  setup-user.sh   (--file setup-user.sh)
                                     setup run in a container from the base image
                                     (harness, plugins, tools Nix can't express)
  settings.json   (--settings)       ~/.claude/settings.json: marketplaces, enabled
                                     plugins (pre-installed at build), model, permissions…

To change the built image by hand instead (interactive configuration,
packages Nix lacks), use ` + "`persona customize`" + `.

Hand-edited files are protected: ` + "`persona update`" + ` and base changes won't overwrite
them without --force. With --spec, edits persona.yaml and regenerates whichever
files were not hand-edited. Its "settings:" map holds overrides deep-merged into
settings.json that survive regeneration.

The editor is $PERSONA_EDITOR, $VISUAL, or $EDITOR. After saving, the image is
rebuilt (with a prompt when interactive).`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completePersonas,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			if spec && settings {
				return errors.New("--spec and --settings are mutually exclusive")
			}
			p, err := persona.Load(args[0])
			if err != nil {
				return err
			}
			if p.Frozen() {
				return persona.ErrFrozen(p)
			}
			if !slices.Contains(persona.EditableFiles, file) {
				return fmt.Errorf("--file must be one of %s", strings.Join(persona.EditableFiles, ", "))
			}
			// Make sure generated files exist (e.g. personas from before Nix builds).
			for _, f := range persona.EditableFiles {
				if _, err := os.Stat(filepath.Join(persona.Dir(p.Name), f)); os.IsNotExist(err) {
					if _, err := p.Generate(false); err != nil {
						return err
					}
					break
				}
			}

			path, validate := filepath.Join(persona.Dir(p.Name), file), func() error { return nil }
			if file == persona.FileSettings {
				validate = func() error { return validateJSONFile(path) }
			}
			switch {
			case spec:
				path = persona.SpecPath(p.Name)
				validate = func() error {
					np, err := validateSpecFile(p.Name)
					if err == nil {
						p = np
					}
					return err
				}
			case settings:
				path = persona.SettingsPath(p.Name)
				validate = func() error { return validateJSONFile(path) }
			}

			changed, err := editLoop(cmd, path, validate)
			if err != nil || !changed {
				return err
			}
			if spec {
				skipped, err := p.Generate(false)
				if err != nil {
					return err
				}
				warnSkipped(out, p.Name, skipped)
			}
			ui.Success(out, "Saved "+path)

			doBuild := build
			if !build && !noBuild {
				if doBuild, err = confirm("Rebuild the image now?", true); err != nil {
					return err
				}
			}
			if doBuild && !noBuild {
				return buildImage(ctx, p.Name, false, out)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&spec, "spec", false, "edit persona.yaml instead of flake.nix")
	cmd.Flags().BoolVar(&settings, "settings", false, "edit the persona's settings.json (same as --file settings.json)")
	cmd.Flags().StringVar(&file, "file", persona.FileFlake, "generated file to edit: "+strings.Join(persona.EditableFiles, ", "))
	cmd.Flags().BoolVar(&build, "build", false, "rebuild after editing without prompting")
	cmd.Flags().BoolVar(&noBuild, "no-build", false, "do not rebuild after editing")
	return cmd
}

// editLoop opens path in the editor until validate passes or the user gives up
// (restoring the original). It reports whether the file changed.
func editLoop(cmd *cobra.Command, path string, validate func() error) (bool, error) {
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	before, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	for {
		if err := editor.Open(ctx, path); err != nil {
			return false, fmt.Errorf("editor: %w", err)
		}
		verr := validate()
		if verr == nil {
			break
		}
		ui.Warning(out, verr.Error())
		again, err := confirm("The file is invalid. Edit again?", true)
		if err != nil {
			return false, err
		}
		if !again {
			if err := os.WriteFile(path, before, 0o644); err != nil {
				return false, err
			}
			return false, fmt.Errorf("reverted %s: %w", path, verr)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if bytes.Equal(before, after) {
		fmt.Fprintln(out, ui.Dim.Render("No changes."))
		return false, nil
	}
	return true, nil
}

func warnSkipped(out io.Writer, name string, skipped []string) {
	if len(skipped) > 0 {
		ui.Warning(out, fmt.Sprintf("%s: hand-edited %s not regenerated (run `persona update %s --force` to regenerate)",
			name, strings.Join(skipped, " and "), name))
	}
}

func validateJSONFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("invalid JSON in %s: %w", path, err)
	}
	return nil
}

func validateSpecFile(name string) (*persona.Persona, error) {
	b, err := os.ReadFile(persona.SpecPath(name))
	if err != nil {
		return nil, err
	}
	var p persona.Persona
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if p.Name != name {
		return nil, fmt.Errorf("name must stay %q (rename by creating a new persona)", name)
	}
	if _, err := persona.Render(&p); err != nil {
		return nil, err
	}
	return &p, nil
}
