// Package config wires viper to persona's on-disk layout and environment.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"

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
	// KeyRuntime selects where instances run: "firecracker" (microVMs; the
	// default on Linux, needs KVM and the persona .deb) or "docker"
	// (containers; the default elsewhere).
	KeyRuntime  = "runtime"
	KeyVMKernel = "vm_kernel" // guest kernel (uncompressed vmlinux)
	KeyVMVCPUs  = "vm_vcpus"
	KeyVMMemory = "vm_memory_mib"
	KeyVMDisk   = "vm_disk_gib" // size of each VM's writable layer (sparse)
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
	viper.SetDefault(KeyRuntime, DefaultRuntime())
	viper.SetDefault(KeyVMKernel, "/usr/share/persona/vmlinux")
	viper.SetDefault(KeyVMVCPUs, 2)
	viper.SetDefault(KeyVMMemory, 4096)
	viper.SetDefault(KeyVMDisk, 32)

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

// DefaultRuntime is firecracker on Linux, where microVMs can run, and docker
// everywhere else.
func DefaultRuntime() string {
	if runtime.GOOS == "linux" {
		return "firecracker"
	}
	return "docker"
}

// InstancesDir holds one record per microVM instance.
func InstancesDir() string { return filepath.Join(Home(), "instances") }

// VMDir holds microVM images and the blank overlay disk. It should be on the
// same filesystem as /srv/jailer so VMs can share images by hard link.
func VMDir() string { return filepath.Join(Home(), "vm") }

// WorktreesDir holds one git worktree per microVM instance.
func WorktreesDir() string { return filepath.Join(Home(), "worktrees") }

// CacheDir holds cached marketplace manifests.
func CacheDir() string { return filepath.Join(Home(), "cache") }
