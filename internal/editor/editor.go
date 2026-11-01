// Package editor opens files in the user's preferred text editor.
package editor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/config"
)

// Command returns the editor command line for a file. It honors
// PERSONA_EDITOR, VISUAL, and EDITOR (via viper), then falls back to a
// platform default. Editor values may include arguments, e.g. "code --wait".
func Command(ctx context.Context, path string) (*exec.Cmd, error) {
	parts := strings.Fields(viper.GetString(config.KeyEditor))
	if len(parts) == 0 {
		parts = []string{fallback()}
	}
	if parts[0] == "" {
		return nil, errors.New("no editor found: set $EDITOR")
	}
	cmd := exec.CommandContext(ctx, parts[0], append(parts[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

// Open edits path and blocks until the editor exits.
func Open(ctx context.Context, path string) error {
	cmd, err := Command(ctx, path)
	if err != nil {
		return err
	}
	return cmd.Run()
}

func fallback() string {
	if runtime.GOOS == "windows" {
		return "notepad"
	}
	for _, e := range []string{"nano", "vim", "vi"} {
		if _, err := exec.LookPath(e); err == nil {
			return e
		}
	}
	return ""
}
