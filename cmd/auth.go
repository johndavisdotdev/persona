package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/auth"
	"github.com/johndavisdev/persona/internal/ui"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Store the Claude subscription token spawned instances sign in with",
		Long: `Instances run Claude Code with your Claude subscription through a long-lived
OAuth token from ` + "`claude setup-token`" + ` (valid for one year). persona stores it
in a file only you can read and gives it to every instance as
CLAUDE_CODE_OAUTH_TOKEN. An exported CLAUDE_CODE_OAUTH_TOKEN takes precedence.

` + "`persona new`" + ` still uses ANTHROPIC_API_KEY.`,
	}
	cmd.AddCommand(newAuthLoginCmd(), newAuthStatusCmd(), newAuthLogoutCmd())
	return cmd
}

func newAuthLoginCmd() *cobra.Command {
	var fromStdin bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Create a token with `claude setup-token` and store it",
		Example: `  persona auth login
  claude setup-token   # on another machine, then:
  printf %s "$TOKEN" | persona auth login --stdin`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			var token string
			if fromStdin {
				b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10))
				if err != nil {
					return err
				}
				token = string(b)
			} else {
				if !ui.Interactive() {
					return errors.New("not a terminal: pipe the token in with --stdin")
				}
				if path, err := exec.LookPath("claude"); err == nil {
					fmt.Fprintln(out, ui.Dim.Render("Running `claude setup-token`. Sign in, then copy the token it prints."))
					c := exec.CommandContext(cmd.Context(), path, "setup-token")
					c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
					if err := c.Run(); err != nil {
						return fmt.Errorf("claude setup-token: %w", err)
					}
				} else {
					ui.Warning(out, "Claude Code isn't installed here; run `claude setup-token` wherever it is and paste the token below.")
				}
				err := huh.NewInput().Title("Paste the token").EchoMode(huh.EchoModePassword).
					Validate(func(s string) error {
						if strings.TrimSpace(s) == "" {
							return errors.New("paste the token `claude setup-token` printed")
						}
						return nil
					}).Value(&token).Run()
				if errors.Is(err, huh.ErrUserAborted) {
					return errors.New("cancelled")
				} else if err != nil {
					return err
				}
			}
			token = strings.TrimSpace(token)
			if !strings.HasPrefix(token, "sk-ant-") {
				ui.Warning(out, "that doesn't look like a Claude token (they start with sk-ant-); storing it anyway")
			}
			s, err := auth.Save(token)
			if err != nil {
				return err
			}
			ui.Success(out, fmt.Sprintf("Stored token %s in %s", s.Masked(), auth.Path()))
			fmt.Fprintln(out, ui.Dim.Render("New instances will use it; running ones keep their old credentials until respawned."))
			return nil
		},
	}
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read the token from stdin instead of running claude setup-token")
	return cmd
}

func newAuthStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show which credentials new instances will get",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			s, err := auth.Load()
			if err != nil {
				return err
			}
			_, hostToken := os.LookupEnv(auth.TokenEnv)
			_, hostKey := os.LookupEnv(auth.APIKeyEnv)
			switch {
			case hostToken:
				fmt.Fprintln(out, "Instances use CLAUDE_CODE_OAUTH_TOKEN from your environment.")
			case s != nil:
				left := time.Until(s.Expires())
				line := fmt.Sprintf("Instances use the stored token %s, created %s, expires %s",
					s.Masked(), s.Created.Local().Format("2006-01-02"), s.Expires().Local().Format("2006-01-02"))
				switch {
				case left <= 0:
					ui.Warning(out, line+": expired, run `persona auth login`")
				case left < 30*24*time.Hour:
					ui.Warning(out, line+": renew soon with `persona auth login`")
				default:
					fmt.Fprintln(out, line+".")
				}
			case hostKey:
				fmt.Fprintln(out, "No token stored: instances use ANTHROPIC_API_KEY (API billing). Run `persona auth login` to use your subscription.")
				return nil
			default:
				fmt.Fprintln(out, "No credentials: Claude Code will ask each instance to log in. Run `persona auth login`.")
				return nil
			}
			if hostKey {
				fmt.Fprintln(out, ui.Dim.Render("ANTHROPIC_API_KEY is set but not forwarded (pass -e ANTHROPIC_API_KEY to spawn to use it)."))
			}
			return nil
		},
	}
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Delete the stored token",
		Long: `Deletes the stored token. It stays valid until it expires; revoke it from
your Claude account settings if it may have leaked.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := auth.Delete(); err != nil {
				return err
			}
			ui.Success(cmd.OutOrStdout(), "Deleted the stored token")
			return nil
		},
	}
}
