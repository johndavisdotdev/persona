// Package config wires viper to persona's on-disk layout and environment.
package config

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// Keys used with viper. Every key can be overridden by an env var with the
// PERSONA_ prefix (e.g. PERSONA_MODEL), by the config file, or by a flag.
const (
	KeyHome            = "home"
	KeyModel           = "model"
	KeyAnthropicAPIKey = "anthropic_api_key"
	KeyAnthropicURL    = "anthropic_base_url"
	KeyExaAPIKey       = "exa_api_key"
	KeyMarketplaces    = "marketplaces"
	KeyBaseImage       = "base_image"
	KeyEditor          = "editor"
	KeyDockerBin       = "docker"
	KeyForwardEnv      = "forward_env"
)

// DefaultMarketplaces are the built-in marketplaces the AI always searches.
var DefaultMarketplaces = []string{
	"anthropics/claude-plugins-official",
	"anthropics/claude-code",
	"anthropics/skills",
}

// Init sets defaults, binds env vars, and reads the config file if present.
// cfgFile may be empty, in which case $PERSONA_HOME/config.yaml is used.
// home, if non-empty, overrides the state directory.
func Init(cfgFile, home string) error {
	viper.SetEnvPrefix("PERSONA")
	viper.AutomaticEnv()

	viper.SetDefault(KeyHome, defaultHome())
	if home != "" {
		viper.Set(KeyHome, home)
	}
	viper.SetDefault(KeyModel, "claude-opus-5-5")
	viper.SetDefault(KeyAnthropicURL, "https://api.anthropic.com")
	viper.SetDefault(KeyMarketplaces, DefaultMarketplaces)
	viper.SetDefault(KeyBaseImage, "debian:trixie-slim")
	viper.SetDefault(KeyDockerBin, "docker")
	viper.SetDefault(KeyForwardEnv, []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"})

	// Fall back to the conventional, unprefixed env vars for API keys.
	_ = viper.BindEnv(KeyAnthropicAPIKey, "PERSONA_ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY")
	_ = viper.BindEnv(KeyExaAPIKey, "PERSONA_EXA_API_KEY", "EXA_API_KEY")
	_ = viper.BindEnv(KeyEditor, "PERSONA_EDITOR", "VISUAL", "EDITOR")

	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.AddConfigPath(Home())
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
	}
	if err := viper.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) && cfgFile != "" {
			return err
		}
	}
	return nil
}

func defaultHome() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "persona")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".persona")
}

// Home is the root directory for persona state.
func Home() string { return viper.GetString(KeyHome) }

// PersonasDir holds one subdirectory per persona.
func PersonasDir() string { return filepath.Join(Home(), "personas") }

// CacheDir holds cached marketplace manifests.
func CacheDir() string { return filepath.Join(Home(), "cache") }
