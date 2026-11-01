package persona

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/johndavisdev/persona/internal/config"
)

// DefaultBaseSettings is the built-in base settings.json, embedded from
// base-settings.json at the project root (see main.go).
var DefaultBaseSettings []byte

const settingsFileName = "settings.json"

// BaseSettingsPath is the user's override of the built-in base settings.
func BaseSettingsPath() string { return filepath.Join(config.Home(), "base-settings.json") }

// SettingsPath is a persona's generated ~/.claude/settings.json.
func SettingsPath(name string) string { return filepath.Join(Dir(name), settingsFileName) }

// LoadBaseSettings returns $PERSONA_HOME/base-settings.json if present,
// otherwise the embedded default.
func LoadBaseSettings() (map[string]any, error) {
	b, err := os.ReadFile(BaseSettingsPath())
	src := BaseSettingsPath()
	if errors.Is(err, os.ErrNotExist) {
		b, src = DefaultBaseSettings, "built-in base-settings.json"
	} else if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", src, err)
	}
	return m, nil
}

// SeedBaseSettings writes the embedded default to BaseSettingsPath if missing.
func SeedBaseSettings() error {
	if _, err := os.Stat(BaseSettingsPath()); err == nil {
		return nil
	}
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return err
	}
	b := DefaultBaseSettings
	if len(b) == 0 {
		b = []byte("{}\n")
	}
	return os.WriteFile(BaseSettingsPath(), b, 0o644)
}

// RenderSettings produces a persona's settings.json by deep-merging, from
// lowest to highest precedence: base settings, the base.yaml + persona spec
// (model, marketplaces, enabled plugins), then the persona's `settings` overrides.
func RenderSettings(p *Persona) (string, error) {
	p.Normalize()
	baseSettings, err := LoadBaseSettings()
	if err != nil {
		return "", err
	}
	base, err := LoadBase()
	if err != nil {
		return "", err
	}
	merged := MergeSettings(baseSettings, specSettings(Effective(p, base)), p.Settings)
	b, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

func specSettings(p *Persona) map[string]any {
	s := map[string]any{}
	if p.Model != "" {
		s["model"] = p.Model
	}
	if len(p.Marketplaces) > 0 {
		mps := map[string]any{}
		for _, m := range p.Marketplaces {
			mps[m.Name] = map[string]any{"source": marketplaceSource(m.Source)}
		}
		s["extraKnownMarketplaces"] = mps
	}
	if len(p.Plugins) > 0 {
		en := map[string]any{}
		for _, pl := range p.Plugins {
			en[pl.ID()] = true
		}
		s["enabledPlugins"] = en
	}
	return s
}

// MergeSettings deep-merges JSON objects; later layers win. Nested objects
// merge key by key; arrays and scalars are replaced. A null value deletes a key.
func MergeSettings(layers ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, l := range layers {
		mergeInto(out, l)
	}
	return out
}

func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if v == nil {
			delete(dst, k)
			continue
		}
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				mergeInto(dv, sv)
				continue
			}
			cp := map[string]any{}
			mergeInto(cp, sv)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// SettingsPlugins lists enabled plugin ids and marketplace sources declared in
// settings (used to tell the designer what every persona already has).
func SettingsPlugins(s map[string]any) (plugins, marketplaceSources []string) {
	if en, ok := s["enabledPlugins"].(map[string]any); ok {
		for id, v := range en {
			if b, _ := v.(bool); b {
				plugins = append(plugins, id)
			}
		}
	}
	if mps, ok := s["extraKnownMarketplaces"].(map[string]any); ok {
		for _, v := range mps {
			m, _ := v.(map[string]any)
			src, _ := m["source"].(map[string]any)
			for _, k := range []string{"repo", "url"} {
				if s, ok := src[k].(string); ok && s != "" {
					marketplaceSources = append(marketplaceSources, s)
					break
				}
			}
		}
	}
	sort.Strings(plugins)
	sort.Strings(marketplaceSources)
	return plugins, marketplaceSources
}
