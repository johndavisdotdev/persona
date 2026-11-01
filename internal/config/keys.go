package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// APIKeys are the keys `persona configure` stores for the persona designer.
type APIKeys struct {
	Anthropic string `json:"anthropic_api_key,omitempty"`
	Exa       string `json:"exa_api_key,omitempty"`
}

// CredentialsDir holds secrets, readable only by the user.
func CredentialsDir() string { return filepath.Join(Home(), "credentials") }

// APIKeysPath is where `persona configure` stores API keys.
func APIKeysPath() string { return filepath.Join(CredentialsDir(), "api-keys.json") }

// LoadAPIKeys returns the stored keys (empty if none are stored).
func LoadAPIKeys() (APIKeys, error) {
	var k APIKeys
	b, err := os.ReadFile(APIKeysPath())
	if errors.Is(err, os.ErrNotExist) {
		return k, nil
	} else if err != nil {
		return k, err
	}
	return k, json.Unmarshal(b, &k)
}

// SaveAPIKeys stores keys in a file only the user can read.
func SaveAPIKeys(k APIKeys) error {
	b, _ := json.MarshalIndent(k, "", "  ")
	return WriteSecret(APIKeysPath(), b)
}

// WriteSecret atomically writes a 0600 file in a 0700 directory.
func WriteSecret(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".secret-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// applyStoredKeys makes stored keys the lowest-precedence source, below env
// vars, the config file, and flags.
func applyStoredKeys() error {
	k, err := LoadAPIKeys()
	if err != nil {
		return err
	}
	if k.Anthropic != "" {
		viper.SetDefault(KeyAnthropicAPIKey, k.Anthropic)
	}
	if k.Exa != "" {
		viper.SetDefault(KeyExaAPIKey, k.Exa)
	}
	return nil
}
