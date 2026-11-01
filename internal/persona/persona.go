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
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/johndavisdev/persona/internal/config"
)

const specFile = "persona.yaml"

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
	Name         string `yaml:"name"`
	Description  string `yaml:"description"`
	Summary      string `yaml:"summary,omitempty"`
	Instructions string `yaml:"instructions,omitempty"` // becomes ~/.claude/CLAUDE.md
	Model        string `yaml:"model,omitempty"`        // harness model: Claude Code alias, or provider/model for pi
	// Harness is the coding agent spawns run: pi or claude (default: the
	// base's, else DefaultHarness).
	Harness string `yaml:"harness,omitempty"`
	// HarnessPackages are pi packages beyond the base's ("npm:<name>"); a
	// leading "-" removes one of the base's.
	HarnessPackages []string      `yaml:"harness_packages,omitempty"`
	BaseImage       string        `yaml:"base_image,omitempty"`
	Marketplaces    []Marketplace `yaml:"marketplaces,omitempty"`
	Plugins         []Plugin      `yaml:"plugins,omitempty"`
	MCPServers      []MCPServer   `yaml:"mcp_servers,omitempty"`
	Skills          []Skill       `yaml:"skills,omitempty"`
	// Packages are nixpkgs attributes in the image ("jq", "python3Packages.ruff").
	Packages []string `yaml:"packages,omitempty"`
	// AptPackages are Debian package names from older specs; they're mapped to
	// nixpkgs (see AptToNix).
	AptPackages []string `yaml:"apt_packages,omitempty"`
	// Toolchains are language SDKs/runtimes installed from their primary sources.
	Toolchains []Toolchain `yaml:"toolchains,omitempty"`
	// Tools are developer tools (linters, formatters, language servers)
	// installed with a toolchain's package manager or apt.
	Tools []Tool   `yaml:"tools,omitempty"`
	Env   []string `yaml:"env,omitempty"` // host env var names forwarded on spawn
	// Settings are persona-specific overrides deep-merged last into settings.json
	// (after base-settings.json and the base). A null value removes a key.
	Settings  map[string]any `yaml:"settings,omitempty"`
	CreatedAt time.Time      `yaml:"created_at"`
	UpdatedAt time.Time      `yaml:"updated_at"`
	// Hashes of the generated files (flake.nix, setup scripts, settings.json)
	// as last written. A mismatch means the file was hand-edited and must not
	// be silently regenerated.
	FileHashes map[string]string `yaml:"file_hashes,omitempty"`
	// DockerfileHash is from Dockerfile-built personas (before Nix); unused.
	DockerfileHash string `yaml:"dockerfile_hash,omitempty"`
	// SettingsHash is from before FileHashes; unused.
	SettingsHash string `yaml:"settings_hash,omitempty"`
	// SavedFrom is set for personas captured from a spawn with `persona save`.
	// Their image is that capture, with no definition behind it, so they can
	// only change through another spawn and save.
	SavedFrom *SavedFrom `yaml:"saved_from,omitempty"`
	// Customized is set once the image was changed by hand with `persona
	// customize`. Rebuilding from the flake would lose those changes, so the
	// persona is frozen like a saved one.
	Customized *Customized `yaml:"customized,omitempty"`
}

// Customized records hand changes made with `persona customize`.
type Customized struct {
	At      time.Time `yaml:"at"`
	Summary string    `yaml:"summary,omitempty"` // what changed (docker diff, abbreviated)
}

// SavedFrom records where a saved persona came from.
type SavedFrom struct {
	Persona string    `yaml:"persona"`
	Spawn   string    `yaml:"spawn"`
	At      time.Time `yaml:"at"`
}

// Saved reports whether p was captured from a spawn (see SavedFrom).
func (p *Persona) Saved() bool { return p.SavedFrom != nil }

// Frozen reports whether p's image can no longer be rebuilt from its
// definition: it was saved from a spawn or customized by hand.
func (p *Persona) Frozen() bool { return p.SavedFrom != nil || p.Customized != nil }

// ErrFrozen explains why a frozen persona can't be edited or rebuilt.
func ErrFrozen(p *Persona) error {
	if p.SavedFrom != nil {
		return fmt.Errorf("%s was saved from spawn %s and has no definition to rebuild from; change it with `persona customize %s`, or by spawning it, making the changes there, and running `persona save <spawn> <name>`",
			p.Name, p.SavedFrom.Spawn, p.Name)
	}
	return fmt.Errorf("%s was customized by hand (%s) and rebuilding it from its flake would lose that; change it with `persona customize %s`",
		p.Name, p.Customized.At.Local().Format("2006-01-02 15:04"), p.Name)
}

// Dir is the persona's directory.
func Dir(name string) string { return filepath.Join(config.PersonasDir(), name) }

// FlakePath is the persona's flake.nix.
func FlakePath(name string) string { return filepath.Join(Dir(name), FileFlake) }

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
		// Not a persona (yet): e.g. a build left only its output behind.
		if _, err := os.Stat(filepath.Join(config.PersonasDir(), e.Name(), "persona.yaml")); errors.Is(err, os.ErrNotExist) {
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

// Generated files that make up a persona's image definition. Each is
// hash-protected: a hand-edited one isn't overwritten without force.
const (
	FileFlake      = "flake.nix"
	FileSetupRoot  = "setup-root.sh"
	FileSetupUser  = "setup-user.sh"
	FileSettings   = "settings.json"
	DirFiles       = "files" // supporting files the setup scripts install; always regenerated
	fileDockerfile = "Dockerfile"
)

// EditableFiles are the generated files, in the order `persona edit` offers them.
var EditableFiles = []string{FileFlake, FileSetupRoot, FileSetupUser, FileSettings}

// Generate renders and writes the persona's image definition (flake.nix, the
// setup scripts, settings.json and files/), recording their hashes. Without
// force, a hand-edited file is left alone and its name returned in skipped.
// The spec is saved either way.
func (p *Persona) Generate(force bool) (skipped []string, err error) {
	if p.Frozen() {
		return nil, p.Save() // its image is the definition
	}
	def, err := Render(p)
	if err != nil {
		return nil, err
	}
	dir := Dir(p.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if p.FileHashes == nil {
		p.FileHashes = map[string]string{}
	}
	for _, name := range EditableFiles {
		content := def.Files[name]
		path := filepath.Join(dir, name)
		if !force {
			edited, err := fileEdited(path, p.FileHashes[name])
			if err != nil {
				return nil, err
			}
			if edited {
				skipped = append(skipped, name)
				continue
			}
		}
		if err := writeAtomic(path, []byte(content)); err != nil {
			return nil, err
		}
		if strings.HasSuffix(name, ".sh") {
			if err := os.Chmod(path, 0o755); err != nil {
				return nil, err
			}
		}
		p.FileHashes[name] = hash([]byte(content))
	}
	if err := writeFilesDir(filepath.Join(dir, DirFiles), def.Support); err != nil {
		return nil, err
	}
	// Dockerfile-era leftovers.
	os.Remove(filepath.Join(dir, fileDockerfile))
	p.DockerfileHash, p.SettingsHash = "", ""
	return skipped, p.Save()
}

// writeFilesDir replaces dir with files (relative path → content; a path
// ending in ".sh" or under bin/ is executable).
func writeFilesDir(dir string, files map[string]string) error {
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	for rel, content := range files {
		path := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") || strings.HasPrefix(rel, "bin/") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}

// EditedFiles lists generated files that were hand-edited since generation.
func (p *Persona) EditedFiles() ([]string, error) {
	var out []string
	for _, name := range EditableFiles {
		edited, err := fileEdited(filepath.Join(Dir(p.Name), name), p.FileHashes[name])
		if err != nil {
			return nil, err
		}
		if edited {
			out = append(out, name)
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
	p.Packages = slices.DeleteFunc(dedupe(p.Packages), func(s string) bool { return ValidateNixPackage(s) != nil })
	p.HarnessPackages = dedupe(p.HarnessPackages)
	p.Tools = normalizeTools(p.Tools)
	for _, tc := range toolToolchains(p.Tools) {
		if !slices.ContainsFunc(p.Toolchains, func(t Toolchain) bool { return t.Name == tc }) {
			p.Toolchains = append(p.Toolchains, Toolchain{Name: tc, Reason: "needed by its developer tools"})
		}
	}
	if p.Harness == HarnessPi && !slices.ContainsFunc(p.Toolchains, func(t Toolchain) bool { return t.Name == "node" }) {
		p.Toolchains = append(p.Toolchains, Toolchain{Name: "node", Reason: "needed by the pi harness"})
	}
	p.Toolchains = normalizeToolchains(p.Toolchains)
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
