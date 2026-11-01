// Package auth stores the Claude Code OAuth token instances use, created with
// `claude setup-token` and held in a 0600 file under $PERSONA_HOME, and
// decides which credentials a spawned instance receives.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/johndavisdev/persona/internal/config"
)

// Env var names Claude Code reads its credentials from.
const (
	TokenEnv  = "CLAUDE_CODE_OAUTH_TOKEN"
	APIKeyEnv = "ANTHROPIC_API_KEY"
)

// Lifetime is how long a `claude setup-token` token is valid.
const Lifetime = 365 * 24 * time.Hour

// Stored is the token persona keeps for instances.
type Stored struct {
	Token   string    `json:"token"`
	Created time.Time `json:"created"`
}

// Expires is when the token stops working.
func (s *Stored) Expires() time.Time { return s.Created.Add(Lifetime) }

// Masked shows only the end of the token.
func (s *Stored) Masked() string {
	if len(s.Token) <= 8 {
		return "…"
	}
	return "…" + s.Token[len(s.Token)-6:]
}

// Path is where the token is stored.
func Path() string { return filepath.Join(config.CredentialsDir(), "claude-oauth-token.json") }

// Load returns the stored token, or nil if there is none.
func Load() (*Stored, error) {
	b, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var s Stored
	if err := json.Unmarshal(b, &s); err != nil || s.Token == "" {
		return nil, fmt.Errorf("%s is unreadable; run `persona auth login` again", Path())
	}
	return &s, nil
}

// Save stores token, readable only by this user.
func Save(token string) (*Stored, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("empty token")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("the token contains whitespace; paste only the token `claude setup-token` printed")
	}
	s := &Stored{Token: token, Created: time.Now().UTC()}
	b, _ := json.Marshal(s)
	return s, config.WriteSecret(Path(), b)
}

// Delete removes the stored token.
func Delete() error {
	err := os.Remove(Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Plan is what a spawned instance receives.
type Plan struct {
	Forward []string // env var names whose host values are forwarded
	Inject  []string // NAME=VALUE pairs to set (the stored token)
	Notes   []string // explanations for the user
}

// ForInstance decides an instance's credentials from the host environment,
// the configured forward list, and the stored token. An exported
// CLAUDE_CODE_OAUTH_TOKEN wins; otherwise a stored token is used. Either way
// ANTHROPIC_API_KEY is held back unless explicit (e.g. `spawn -e`), because
// Claude Code prefers an API key over a token and would bill the API instead
// of the subscription. explicit lists names the user asked to forward.
func ForInstance(lookup func(string) (string, bool), forward, explicit []string, stored *Stored) Plan {
	var p Plan
	_, hostToken := lookup(TokenEnv)
	useToken := hostToken || stored != nil
	for _, k := range forward {
		if _, ok := lookup(k); !ok {
			continue
		}
		if k == APIKeyEnv && useToken && !slices.Contains(explicit, k) {
			p.Notes = append(p.Notes, "not forwarding ANTHROPIC_API_KEY: instances use your Claude subscription token (pass -e ANTHROPIC_API_KEY to use the key)")
			continue
		}
		p.Forward = append(p.Forward, k)
	}
	if !hostToken && stored != nil {
		p.Forward = slices.DeleteFunc(p.Forward, func(k string) bool { return k == TokenEnv })
		p.Inject = append(p.Inject, TokenEnv+"="+stored.Token)
		if left := time.Until(stored.Expires()); left < 30*24*time.Hour {
			p.Notes = append(p.Notes, expiryNote(left))
		}
	}
	return p
}

func expiryNote(left time.Duration) string {
	if left <= 0 {
		return "the stored Claude token has expired; run `persona auth login` to create a new one"
	}
	return fmt.Sprintf("the stored Claude token expires in %d days; run `persona auth login` to renew it", int(left.Hours()/24)+1)
}

// HasCredentials reports whether an instance will be able to sign in.
func (p Plan) HasCredentials() bool {
	for _, k := range p.Forward {
		if k == TokenEnv || k == APIKeyEnv {
			return true
		}
	}
	return len(p.Inject) > 0
}
