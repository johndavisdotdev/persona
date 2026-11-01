package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/auth"
	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/ui"
)

func newConfigureCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "configure",
		Short: "Store API keys: Anthropic and Exa for the designer, OpenRouter for spawns",
		Long: `Prompts for an Anthropic API key (needed by ` + "`persona new`" + `, ` + "`update --redesign`" + ` and
` + "`base`" + `), an optional Exa API key (lets the designer search the web), and an
OpenRouter API key (passed to spawns as OPENROUTER_API_KEY, for harnesses such
as pi that reach models through OpenRouter). Keys are stored in
$PERSONA_HOME/credentials/api-keys.json, readable only by you.

Leave a prompt blank to keep the current key, or enter - to remove it.
ANTHROPIC_API_KEY, EXA_API_KEY and OPENROUTER_API_KEY in your environment take
precedence over stored keys.

Claude Code in spawns signs in with your Claude subscription instead (see
` + "`persona auth login`" + `).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			if !ui.Interactive() {
				return errors.New("configure needs a terminal; in scripts, set ANTHROPIC_API_KEY, EXA_API_KEY and OPENROUTER_API_KEY instead")
			}
			keys, err := config.LoadAPIKeys()
			if err != nil {
				return err
			}
			var anthropic, exa, openrouter string
			form := huh.NewForm(huh.NewGroup(
				huh.NewInput().Title("Anthropic API key").
					Description(keyStatus(keys.Anthropic, "ANTHROPIC_API_KEY", "required by the persona designer")).
					EchoMode(huh.EchoModePassword).Value(&anthropic),
				huh.NewInput().Title("Exa API key").
					Description(keyStatus(keys.Exa, "EXA_API_KEY", "optional: lets the designer search the web")).
					EchoMode(huh.EchoModePassword).Value(&exa),
				huh.NewInput().Title("OpenRouter API key").
					Description(keyStatus(keys.OpenRouter, "OPENROUTER_API_KEY", "used by spawns whose harness reaches models through OpenRouter (pi)")).
					EchoMode(huh.EchoModePassword).Value(&openrouter),
			))
			if err := form.Run(); err != nil {
				if errors.Is(err, huh.ErrUserAborted) {
					return errors.New("cancelled")
				}
				return err
			}

			changed := false
			apply := func(input string, current *string, name string) error {
				switch v := strings.TrimSpace(input); v {
				case "":
					return nil
				case "-":
					if *current != "" {
						*current, changed = "", true
						ui.Step(out, "removed the stored "+name)
					}
					return nil
				default:
					if strings.ContainsAny(v, " \t") {
						return fmt.Errorf("the %s contains spaces; paste only the key", name)
					}
					*current, changed = v, true
					return nil
				}
			}
			if err := apply(anthropic, &keys.Anthropic, "Anthropic API key"); err != nil {
				return err
			}
			if err := apply(exa, &keys.Exa, "Exa API key"); err != nil {
				return err
			}
			if err := apply(openrouter, &keys.OpenRouter, "OpenRouter API key"); err != nil {
				return err
			}
			if v := strings.TrimSpace(anthropic); v != "" && v != "-" {
				if err := checkAnthropicKey(ctx, v); err != nil {
					var bad *badKeyError
					if errors.As(err, &bad) {
						return fmt.Errorf("%w; nothing was saved", err)
					}
					ui.Warning(out, "couldn't verify the Anthropic key ("+err.Error()+"); saving it anyway")
				} else {
					ui.Step(out, "Anthropic API key verified")
				}
			}
			if v := strings.TrimSpace(openrouter); v != "" && v != "-" {
				if err := checkBearerKey(ctx, "https://openrouter.ai/api/v1/key", v); err != nil {
					var bad *badKeyError
					if errors.As(err, &bad) {
						return fmt.Errorf("OpenRouter rejected the key (%s); nothing was saved", bad.status)
					}
					ui.Warning(out, "couldn't verify the OpenRouter key ("+err.Error()+"); saving it anyway")
				} else {
					ui.Step(out, "OpenRouter API key verified")
				}
			}
			if changed {
				if err := config.SaveAPIKeys(keys); err != nil {
					return err
				}
				ui.Success(out, "Saved API keys to "+config.APIKeysPath())
			} else {
				fmt.Fprintln(out, ui.Dim.Render("No changes."))
			}
			for _, env := range []string{"ANTHROPIC_API_KEY", "EXA_API_KEY", "OPENROUTER_API_KEY"} {
				if _, ok := os.LookupEnv(env); ok {
					fmt.Fprintln(out, ui.Dim.Render(env+" is set in your environment and takes precedence over the stored key."))
				}
			}
			if s, _ := auth.Load(); s == nil && os.Getenv(auth.TokenEnv) == "" {
				fmt.Fprintln(out, "Next: run "+ui.Bold.Render("persona auth login")+" so instances use your Claude subscription.")
			}
			return nil
		},
	}
}

// keyStatus describes where a key currently comes from.
func keyStatus(stored, env, purpose string) string {
	var src string
	switch v, ok := os.LookupEnv(env); {
	case ok && v != "":
		src = "using " + env + " from your environment"
	case stored != "":
		src = "stored: " + maskKey(stored) + " (blank keeps it, - removes it)"
	default:
		src = "not set"
	}
	return purpose + "; " + src
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return "…"
	}
	return "…" + k[len(k)-4:]
}

type badKeyError struct{ status string }

func (e *badKeyError) Error() string { return "Anthropic rejected the key (" + e.status + ")" }

// checkAnthropicKey makes one cheap authenticated request (listing models).
func checkAnthropicKey(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	base := strings.TrimRight(viper.GetString(config.KeyAnthropicURL), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models?limit=1", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &badKeyError{status: resp.Status}
	case resp.StatusCode/100 != 2:
		return errors.New(resp.Status)
	}
	return nil
}

// checkBearerKey makes one authenticated GET with a bearer token.
func checkBearerKey(ctx context.Context, url, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &badKeyError{status: resp.Status}
	case resp.StatusCode/100 != 2:
		return errors.New(resp.Status)
	}
	return nil
}
