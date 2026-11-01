// Package persona defines the persona spec and its on-disk store.
package persona

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/johndavisdev/persona/internal/config"
)

const (
	specFile       = "persona.yaml"
	dockerfileName = "Dockerfile"
)

// ErrNotFound is returned when a persona does not exist.
var ErrNotFound = errors.New("persona not found")

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// ValidateName enforces names that are safe as directory, image, and container names.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid persona name %q: use 1-48 lowercase letters, digits, or dashes, starting with a letter or digit", name)
	}
	return nil
}

// Marketplace is a Claude Code plugin marketplace source.
type Marketplace struct {
	Name   string `yaml:"name" json:"name"`
	Source string `yaml:"source" json:"source"` // "owner/repo" or a git URL
}

// Plugin is a plugin to install from a marketplace.
type Plugin struct {
	Name        string `yaml:"name" json:"name"`
	Marketplace string `yaml:"marketplace" json:"marketplace"`
	Reason      string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// ID returns the plugin@marketplace identifier.
func (p Plugin) ID() string { return p.Name + "@" + p.Marketplace }

// MCPServer is an MCP server registered at user scope inside the container.
type MCPServer struct {
	Name    string            `yaml:"name" json:"name"`
	Type    string            `yaml:"type" json:"type"` // stdio | http | sse
	Command string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty" json:"args,omitempty"`
	URL     string            `yaml:"url,omitempty" json:"url,omitempty"`
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Reason  string            `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// Skill is a standalone Agent Skill copied from a git repository.
type Skill struct {
	Name   string `yaml:"name" json:"name"`
	Repo   string `yaml:"repo" json:"repo"` // git URL or owner/repo
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`
	Ref    string `yaml:"ref,omitempty" json:"ref,omitempty"`
	Reason string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// Persona is the full definition of a persona.
type Persona struct {
	Name         string        `yaml:"name"`
	Description  string        `yaml:"description"`
	Summary      string        `yaml:"summary,omitempty"`
	Instructions string        `yaml:"instructions,omitempty"` // becomes ~/.claude/CLAUDE.md
	Model        string        `yaml:"model,omitempty"`        // Claude Code model inside the container
	BaseImage    string        `yaml:"base_image,omitempty"`
	Marketplaces []Marketplace `yaml:"marketplaces,omitempty"`
	Plugins      []Plugin      `yaml:"plugins,omitempty"`
	MCPServers   []MCPServer   `yaml:"mcp_servers,omitempty"`
	Skills       []Skill       `yaml:"skills,omitempty"`
	AptPackages  []string      `yaml:"apt_packages,omitempty"`
	Env          []string      `yaml:"env,omitempty"` // host env var names forwarded on spawn
	// Settings are persona-specific overrides deep-merged last into settings.json
	// (after base-settings.json and the base). A null value removes a key.
	Settings  map[string]any `yaml:"settings,omitempty"`
	CreatedAt time.Time      `yaml:"created_at"`
	UpdatedAt time.Time      `yaml:"updated_at"`
	// DockerfileHash is the hash of the last generated Dockerfile. A mismatch
	// means the Dockerfile was hand-edited and must not be silently regenerated.
	DockerfileHash string `yaml:"dockerfile_hash,omitempty"`
	// SettingsHash is the same protection for the generated settings.json.
	SettingsHash string `yaml:"settings_hash,omitempty"`
}

// Dir is the persona's directory.
func Dir(name string) string { return filepath.Join(config.PersonasDir(), name) }

// DockerfilePath is the persona's Dockerfile path.
func DockerfilePath(name string) string { return filepath.Join(Dir(name), dockerfileName) }

// SpecPath is the persona's spec path.
func SpecPath(name string) string { return filepath.Join(Dir(name), specFile) }

// Image returns the docker image tag for the persona.
func (p *Persona) Image() string { return ImageFor(p.Name) }

// ImageFor returns the docker image tag for a persona name.
func ImageFor(name string) string { return "persona/" + name + ":latest" }

// Exists reports whether a persona with this name exists.
func Exists(name string) bool {
	_, err := os.Stat(SpecPath(name))
	return err == nil
}

// Load reads a persona spec.
func Load(name string) (*Persona, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(SpecPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	var p Persona
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", SpecPath(name), err)
	}
	if p.Name != name {
		return nil, fmt.Errorf("%s: name %q does not match directory %q", SpecPath(name), p.Name, name)
	}
	return &p, nil
}

// List returns all personas sorted by name. Unreadable entries are skipped
// and reported in the returned error slice.
func List() ([]*Persona, []error) {
	entries, err := os.ReadDir(config.PersonasDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	var out []*Persona
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || ValidateName(e.Name()) != nil {
			continue
		}
		p, err := Load(e.Name())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// Save writes the spec (not the Dockerfile).
func (p *Persona) Save() error {
	if err := ValidateName(p.Name); err != nil {
		return err
	}
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	if err := os.MkdirAll(Dir(p.Name), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	return writeAtomic(SpecPath(p.Name), b)
}

// Generated files that make up a persona's image.
const (
	FileDockerfile = "Dockerfile"
	FileSettings   = "settings.json"
)

// Generate renders and writes the persona's Dockerfile and settings.json,
// recording their hashes. Without force, a hand-edited file is left alone and
// its name returned in skipped. The spec is saved either way.
func (p *Persona) Generate(force bool) (skipped []string, err error) {
	dockerfile, err := RenderDockerfile(p)
	if err != nil {
		return nil, err
	}
	settings, err := RenderSettings(p)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(Dir(p.Name), 0o755); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name, path, content string
		hash                *string
	}{
		{FileDockerfile, DockerfilePath(p.Name), dockerfile, &p.DockerfileHash},
		{FileSettings, SettingsPath(p.Name), settings, &p.SettingsHash},
	} {
		if !force {
			edited, err := fileEdited(f.path, *f.hash)
			if err != nil {
				return nil, err
			}
			if edited {
				skipped = append(skipped, f.name)
				continue
			}
		}
		if err := writeAtomic(f.path, []byte(f.content)); err != nil {
			return nil, err
		}
		*f.hash = hash([]byte(f.content))
	}
	return skipped, p.Save()
}

// EditedFiles lists generated files that were hand-edited since generation.
func (p *Persona) EditedFiles() ([]string, error) {
	var out []string
	for _, f := range []struct{ name, path, hash string }{
		{FileDockerfile, DockerfilePath(p.Name), p.DockerfileHash},
		{FileSettings, SettingsPath(p.Name), p.SettingsHash},
	} {
		edited, err := fileEdited(f.path, f.hash)
		if err != nil {
			return nil, err
		}
		if edited {
			out = append(out, f.name)
		}
	}
	return out, nil
}

func fileEdited(path, want string) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return want != "" && hash(b) != want, nil
}

// Delete removes the persona directory.
func Delete(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if !Exists(name) {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return os.RemoveAll(Dir(name))
}

// Normalize trims, dedupes, and fills defaults so specs from the AI or a
// hand-edit are consistent.
func (p *Persona) Normalize() {
	p.Name = strings.TrimSpace(p.Name)
	seenMP := map[string]bool{}
	var mps []Marketplace
	for _, m := range p.Marketplaces {
		m.Name, m.Source = strings.TrimSpace(m.Name), strings.TrimSpace(m.Source)
		if m.Name == "" || m.Source == "" || seenMP[m.Name] {
			continue
		}
		seenMP[m.Name] = true
		mps = append(mps, m)
	}
	p.Marketplaces = mps

	seenPl := map[string]bool{}
	var pls []Plugin
	for _, pl := range p.Plugins {
		if pl.Name == "" || pl.Marketplace == "" || seenPl[pl.ID()] {
			continue
		}
		seenPl[pl.ID()] = true
		pls = append(pls, pl)
	}
	p.Plugins = pls

	seenMCP := map[string]bool{}
	var mcps []MCPServer
	for _, s := range p.MCPServers {
		if s.Name == "" || seenMCP[s.Name] {
			continue
		}
		if s.Type == "" {
			if s.URL != "" {
				s.Type = "http"
			} else {
				s.Type = "stdio"
			}
		}
		seenMCP[s.Name] = true
		mcps = append(mcps, s)
	}
	p.MCPServers = mcps

	seenSk := map[string]bool{}
	var sks []Skill
	for _, s := range p.Skills {
		if s.Name == "" || s.Repo == "" || seenSk[s.Name] {
			continue
		}
		seenSk[s.Name] = true
		sks = append(sks, s)
	}
	p.Skills = sks
	p.AptPackages = dedupe(p.AptPackages)
	p.Env = dedupe(p.Env)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
