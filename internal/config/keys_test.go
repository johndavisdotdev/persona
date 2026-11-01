package config

import (
	"os"
	"testing"

	"github.com/spf13/viper"
)

func TestStoredKeysAreLowestPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "")
	os.Unsetenv("ANTHROPIC_API_KEY")
	os.Unsetenv("PERSONA_ANTHROPIC_API_KEY")
	os.Unsetenv("EXA_API_KEY")
	os.Unsetenv("PERSONA_EXA_API_KEY")
	viper.Reset()
	if err := Init("", home); err != nil {
		t.Fatal(err)
	}
	if err := SaveAPIKeys(APIKeys{Anthropic: "stored-a", Exa: "stored-e"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(APIKeysPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi.Mode().Perm(), err)
	}
	viper.Reset()
	if err := Init("", home); err != nil {
		t.Fatal(err)
	}
	if got := viper.GetString(KeyAnthropicAPIKey); got != "stored-a" {
		t.Errorf("stored key not used: %q", got)
	}
	t.Setenv("ANTHROPIC_API_KEY", "from-env")
	if got := viper.GetString(KeyAnthropicAPIKey); got != "from-env" {
		t.Errorf("env should win: %q", got)
	}
	if got := viper.GetString(KeyExaAPIKey); got != "stored-e" {
		t.Errorf("exa = %q", got)
	}
	viper.Reset()
}
