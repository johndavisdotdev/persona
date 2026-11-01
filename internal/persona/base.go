package persona

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/johndavisdev/persona/internal/config"
)

// Base holds requirements applied to every persona. A persona's own entries
// win when names collide (same plugin id, MCP server, or skill name).
type Base struct {
	// Requirements is the plain-language history of what was asked for.
	Requirements []string      `yaml:"requirements,omitempty"`
	Instructions string        `yaml:"instructions,omitempty"` // appended to every CLAUDE.md
	Model        string        `yaml:"model,omitempty"`        // default model when a persona sets none
	Marketplaces []Marketplace `yaml:"marketplaces,omitempty"`
	Plugins      []Plugin      `yaml:"plugins,omitempty"`
	MCPServers   []MCPServer   `yaml:"mcp_servers,omitempty"`
	Skills       []Skill       `yaml:"skills,omitempty"`
	Packages     []string      `yaml:"packages,omitempty"` // nixpkgs attributes
	AptPackages  []string      `yaml:"apt_packages,omitempty"`
	Toolchains   []Toolchain   `yaml:"toolchains,omitempty"`
	Tools        []Tool        `yaml:"tools,omitempty"`
	Env          []string      `yaml:"env,omitempty"`
	// Harness is the default harness for personas that don't pick one.
	Harness string `yaml:"harness,omitempty"`
	// HarnessPackages are the pi packages every pi persona gets. Unset means
	// DefaultPiPackages; an empty list means none.
	HarnessPackages *[]string `yaml:"harness_packages,omitempty"`
}

// PiPackages is the base's pi package list (DefaultPiPackages unless set).
func (b *Base) PiPackages() []string {
	if b == nil || b.HarnessPackages == nil {
		return DefaultPiPackages
	}
	return *b.HarnessPackages
}

// EffectiveHarness is p's harness, else the base's, else DefaultHarness.
func EffectiveHarness(p *Persona, b *Base) string {
	if p.Harness != "" {
		return p.Harness
	}
	if b != nil && b.Harness != "" {
		return b.Harness
	}
	return DefaultHarness
}

const baseHeader = `# persona base requirements — applied to EVERY persona.
# Edit with ` + "`persona edit-base`" + ` or describe changes in plain language with
# ` + "`persona base \"All personas should have ...\"`" + `.
# A persona's own plugins / MCP servers / skills win on name collisions.
`

// BasePath is the location of base.yaml.
func BasePath() string { return filepath.Join(config.Home(), "base.yaml") }

// LoadBase reads the base; a missing file yields an empty base.
func LoadBase() (*Base, error) {
	b, err := os.ReadFile(BasePath())
	if errors.Is(err, os.ErrNotExist) {
		return &Base{}, nil
	}
	if err != nil {
		return nil, err
	}
	var base Base
	if err := yaml.Unmarshal(b, &base); err != nil {
		return nil, fmt.Errorf("parse %s: %w", BasePath(), err)
	}
	return &base, nil
}

// Save writes base.yaml.
func (b *Base) Save() error {
	b.normalize()
	out, err := yaml.Marshal(b)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return err
	}
	return writeAtomic(BasePath(), append([]byte(baseHeader), out...))
}

// Empty reports whether the base contributes nothing.
func (b *Base) Empty() bool {
	return b.Instructions == "" && b.Model == "" && len(b.Plugins) == 0 && len(b.MCPServers) == 0 &&
		len(b.Skills) == 0 && len(b.AptPackages) == 0 && len(b.Packages) == 0 && len(b.Toolchains) == 0 && len(b.Tools) == 0 && len(b.Env) == 0 &&
		b.Harness == "" && b.HarnessPackages == nil
}

// AsPersona views the base as a persona (for rendering and AI revision).
func (b *Base) AsPersona() *Persona {
	return &Persona{
		Name: "base", Instructions: b.Instructions, Model: b.Model,
		Marketplaces: b.Marketplaces, Plugins: b.Plugins, MCPServers: b.MCPServers,
		Skills: b.Skills, Packages: b.Packages, AptPackages: b.AptPackages, Toolchains: b.Toolchains, Tools: b.Tools, Env: b.Env,
		Harness: b.Harness, HarnessPackages: b.PiPackages(),
	}
}

// SetFromPersona replaces the base's components with those of p.
func (b *Base) SetFromPersona(p *Persona) {
	b.Instructions, b.Model = p.Instructions, p.Model
	b.Marketplaces, b.Plugins, b.MCPServers, b.Skills = p.Marketplaces, p.Plugins, p.MCPServers, p.Skills
	b.Packages, b.AptPackages, b.Toolchains, b.Tools, b.Env = p.Packages, p.AptPackages, p.Toolchains, p.Tools, p.Env
	b.Harness = p.Harness
	if !slices.Equal(p.HarnessPackages, DefaultPiPackages) {
		pk := slices.Clone(p.HarnessPackages)
		if pk == nil {
			pk = []string{}
		}
		b.HarnessPackages = &pk
	} else {
		b.HarnessPackages = nil
	}
}

func (b *Base) normalize() {
	p := b.AsPersona()
	p.Normalize()
	b.SetFromPersona(p)
}

// Effective returns a copy of p with the base merged in, plus the plugins
// its toolchains imply.
func Effective(p *Persona, b *Base) *Persona {
	e := *p
	defer func() { // every harness gets the beacon and persona plugins
		e.Marketplaces = append(e.Marketplaces, BeaconMarketplace)
		e.Plugins = append(e.Plugins, BeaconPlugin, PersonaPlugin)
		e.Normalize()
	}()
	e.Harness = EffectiveHarness(p, b)
	e.HarnessPackages = nil
	if e.Harness == HarnessPi {
		e.HarnessPackages = harnessPackages(b.PiPackages(), p.HarnessPackages)
	}
	if b == nil || b.Empty() {
		e.Plugins = append(append([]Plugin{}, p.Plugins...), impliedPlugins(normalizeToolchains(p.Toolchains))...)
		e.Normalize()
		return &e
	}
	if e.Model == "" {
		e.Model = b.Model
	}
	if strings.TrimSpace(b.Instructions) != "" {
		e.Instructions = strings.TrimSpace(e.Instructions + "\n\n## Requirements for all personas\n\n" + strings.TrimSpace(b.Instructions))
	}
	// Persona entries first so they win during Normalize's dedupe.
	e.Marketplaces = append(append([]Marketplace{}, p.Marketplaces...), b.Marketplaces...)
	e.Plugins = append(append([]Plugin{}, p.Plugins...), b.Plugins...)
	e.MCPServers = append(append([]MCPServer{}, p.MCPServers...), b.MCPServers...)
	e.Skills = append(append([]Skill{}, p.Skills...), b.Skills...)
	e.Packages = append(append([]string{}, p.Packages...), b.Packages...)
	e.AptPackages = append(append([]string{}, p.AptPackages...), b.AptPackages...)
	e.Toolchains = normalizeToolchains(append(append([]Toolchain{}, p.Toolchains...), b.Toolchains...))
	e.Tools = append(append([]Tool{}, p.Tools...), b.Tools...)
	e.Plugins = append(e.Plugins, impliedPlugins(e.Toolchains)...)
	e.Env = append(append([]string{}, p.Env...), b.Env...)
	e.Normalize()
	return &e
}
