package persona

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"runtime"
	"slices"
	"strings"
	"text/template"

	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/config"
)

// SessionName is the tmux session that runs the harness inside every spawn.
const SessionName = "persona"

// Labels applied to images and containers.
const (
	LabelManaged  = "persona.managed"
	LabelName     = "persona.name"
	LabelInstance = "persona.instance"
	// LabelStore is StorePrivate on images whose /nix/store was changed by
	// hand (persona customize): microVMs don't share it then.
	LabelStore   = "persona.store"
	StorePrivate = "private"
)

// Image layout shared by the build and runtimes.
const (
	ImageUser       = "claude"
	ImageHome       = "/home/claude"
	ImageWorkdir    = "/workspace"
	ImageEntrypoint = "/home/claude/.local/bin/persona-entrypoint"
	// SetupDir is where the build copies a persona's directory into the setup
	// container; the setup scripts read their files from there.
	SetupDir = "/persona-setup"
	// MarketplaceDir is where images keep the local "persona" marketplace.
	MarketplaceDir = "/opt/persona/marketplace"
	// ManagedSettingsPath is Claude Code's system-wide managed settings file.
	ManagedSettingsPath = "/etc/claude-code/managed-settings.json"
)

// The beacon plugin, from the local "persona" marketplace, is in every persona.
// BeaconMCPName is the MCP server it provides.
const BeaconMCPName = "persona-beacon"

var (
	BeaconMarketplace = Marketplace{Name: "persona", Source: MarketplaceDir}
	BeaconPlugin      = Plugin{Name: "beacon", Marketplace: "persona",
		Reason: "discover and message the other persona agents, and reach the persona host"}
	// PersonaPlugin has the skills for working inside a spawn (its git
	// workspace through persona-agent). pi gets its skills copied instead.
	PersonaPlugin = Plugin{Name: "persona", Marketplace: "persona",
		Reason: "skills for working inside a persona spawn: its git workspace through persona-agent"}
)

// Definition is a rendered persona: the editable files (by name, see
// EditableFiles) and the supporting files under files/.
type Definition struct {
	Files   map[string]string
	Support map[string]string
}

// Render produces a persona's image definition, with the base requirements
// (base.yaml) merged in.
func Render(p *Persona) (*Definition, error) {
	p.Normalize()
	base, err := LoadBase()
	if err != nil {
		return nil, err
	}
	settings, err := RenderSettings(p)
	if err != nil {
		return nil, err
	}
	return render(Effective(p, base), settings)
}

func render(p *Persona, settings string) (*Definition, error) {
	if p.Harness == "" {
		p.Harness = DefaultHarness
	}
	flake, err := renderFlake(p, viper.GetString(config.KeyNixpkgs), nixSystem())
	if err != nil {
		return nil, err
	}
	if p.Harness == HarnessPi && !slices.ContainsFunc(p.MCPServers, func(m MCPServer) bool { return m.Name == BeaconMCPName }) {
		// pi can't load Claude Code plugins, but pi-code reads ~/.claude.json's
		// MCP servers: register the beacon plugin's server directly.
		p.MCPServers = append(p.MCPServers, MCPServer{Name: BeaconMCPName, Type: "stdio",
			Command: MarketplaceDir + "/plugins/beacon/bin/persona-beacon", Env: map[string]string{"LD_LIBRARY_PATH": "/lib"},
			Reason: BeaconPlugin.Reason})
	}
	mcp, err := mcpJSON(p)
	if err != nil {
		return nil, err
	}
	d := &Definition{
		Files: map[string]string{FileFlake: flake, FileSettings: settings},
		Support: map[string]string{
			"bin/persona-status":     StatusScript,
			"bin/persona-entrypoint": entrypoint + "\n",
			"instructions.md":        claudeMD(p),
			"mcp.json":               mcp + "\n",
		},
	}
	if p.Harness == HarnessPi {
		d.Support["pi/settings.json"] = piSettings(p) + "\n"
		d.Support["pi/extensions/persona-status.ts"] = piExtension
		d.Support["tmux.conf"] = "set -g extended-keys on\nset -g extended-keys-format csi-u\n"
	}
	if d.Files[FileSetupRoot], err = execTmpl(setupRootTmpl, setupData(p)); err != nil {
		return nil, err
	}
	if d.Files[FileSetupUser], err = execTmpl(setupUserTmpl, setupData(p)); err != nil {
		return nil, err
	}
	return d, nil
}

// nixSystem is the Nix platform images are built for.
func nixSystem() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64-linux"
	}
	return "x86_64-linux"
}

func execTmpl(t *template.Template, data any) (string, error) {
	var b bytes.Buffer
	err := t.Execute(&b, data)
	return b.String(), err
}

type setupTmplData struct {
	P                *Persona
	Harness          string
	Custom           []customStep
	Rust             *Toolchain
	Playwright       *Toolchain
	PiPackage        string
	PiPackages       []string
	InstructionsPath string
	SkillsDir        string
	Skills           []skillCmd
	Tools            []toolCmd
	Bins             []string // must be on PATH
	CustomBins       []string // should be (custom toolchains; names vary)
	GitAuth          string
}

type customStep struct{ Label, Source, Version, Install string }
type skillCmd struct{ Name, Repo, Path, Ref string }
type toolCmd struct{ Cmd, Warning string }

func setupData(p *Persona) setupTmplData {
	d := setupTmplData{P: p, Harness: p.Harness, GitAuth: gitAuth,
		InstructionsPath: ImageHome + "/.claude/CLAUDE.md", SkillsDir: ImageHome + "/.claude/skills"}
	if p.Harness == HarnessPi {
		d.InstructionsPath, d.SkillsDir = ImageHome+"/.pi/agent/AGENTS.md", ImageHome+"/.pi/agent/skills"
		d.PiPackage, d.PiPackages = PiPackage, p.HarnessPackages
	}
	for i, t := range p.Toolchains {
		switch {
		case t.Name == "rust":
			d.Rust = &p.Toolchains[i]
		case t.Name == "playwright":
			d.Playwright = &p.Toolchains[i]
		case t.Install != "" && t.Nix == "":
			d.Custom = append(d.Custom, customStep{Label: t.Name + optional(" "+t.Version, t.Version != ""),
				Source: t.Source, Version: shellQuote(t.Version), Install: strings.TrimSpace(t.Install)})
		}
		if info, ok := toolchainInfo(t.Name); ok {
			d.Bins = append(d.Bins, info.Bins...)
		} else {
			d.CustomBins = append(d.CustomBins, t.Name)
		}
	}
	d.Bins = append(d.Bins, MCPLaunchers...)
	switch p.Harness {
	case HarnessPi:
		d.Bins = append(d.Bins, "pi")
	default:
		d.Bins = append(d.Bins, "claude")
	}
	d.Bins = dedupe(d.Bins)
	d.Tools = toolCommands(p.Tools)
	for _, s := range p.Skills {
		sp := strings.Trim(path.Clean("/"+s.Path), "/")
		if sp == "" {
			sp = "."
		}
		d.Skills = append(d.Skills, skillCmd{Name: shellQuote(s.Name), Repo: shellQuote(gitURL(s.Repo)), Path: shellQuote(sp), Ref: s.Ref})
	}
	return d
}

var funcs = template.FuncMap{"shq": shellQuote, "join": strings.Join}

var setupRootTmpl = template.Must(template.New(FileSetupRoot).Funcs(funcs).Parse(`#!/bin/bash
# persona: {{.P.Name}} — image setup, run as root
# Generated by persona. The build runs this in a container from the Nix-built
# base image (flake.nix), then setup-user.sh as claude, and commits the result.
# Edit freely with ` + "`persona edit {{.P.Name}}`" + `; files it installs are in files/.
set -euo pipefail
S=` + SetupDir + `

# Status stream: harness hooks report through this (see persona manage).
install -D -m 0755 "$S/files/bin/persona-status" /usr/local/bin/persona-status

# The local "persona" Claude Code marketplace (plugin "beacon"), copied in by
# the build from the published beacon.
if [ -d "$S/marketplace" ]; then
  rm -rf ` + MarketplaceDir + ` && mkdir -p "$(dirname ` + MarketplaceDir + `)"
  cp -R "$S/marketplace" ` + MarketplaceDir + ` && chmod -R a+rX ` + MarketplaceDir + `
  # The beacon is a native .NET binary: point it at the image's /lib (libstdc++,
  # ICU), which Nix's loader doesn't search by itself.
  m=` + MarketplaceDir + `/plugins/beacon/.mcp.json
  if [ -f "$m" ]; then
    jq '(.mcpServers[]? | select(.command | endswith("/persona-beacon")) | .env.LD_LIBRARY_PATH) = "/lib"' "$m" > "$m.tmp" && mv "$m.tmp" "$m" && chmod a+r "$m"
  fi
else
  echo "persona: warning: no published persona marketplace; the beacon plugin is unavailable" >&2
fi
{{- if eq .Harness "claude"}}

# Claude Code managed settings (system-wide, not editable by the agent).
if [ -f "$S/managed-settings.json" ]; then
  install -D -m 0644 "$S/managed-settings.json" ` + ManagedSettingsPath + `
else
  echo "persona: warning: no managed-settings.json to install at ` + ManagedSettingsPath + `" >&2
fi
{{- end}}
{{range .Custom}}
# {{.Label}}, installed as documented at {{.Source}}
(
set -eux
VERSION={{.Version}}
{{.Install}}
) || echo "persona: warning: installing {{.Label}} from {{.Source}} failed" >&2
{{end}}
chown -R claude:claude /home/claude /workspace
`))

var setupUserTmpl = template.Must(template.New(FileSetupUser).Funcs(funcs).Parse(`#!/bin/bash
# persona: {{.P.Name}} — image setup, run as claude
# Generated by persona: what Nix can't express (the harness, its plugins and
# settings, npm/uv/go/cargo tools, browsers). Runs after setup-root.sh; the
# container is then committed as the persona's image.
# Edit freely with ` + "`persona edit {{.P.Name}}`" + `.
set -euo pipefail
S=` + SetupDir + `
cd ~
warn() { echo "persona: warning: $*" >&2; }
# Private GitHub repos use GITHUB_TOKEN, passed to this run only (never committed).
{{.GitAuth}}
mkdir -p ~/.local/bin ~/.config
install -m 0755 "$S/files/bin/persona-entrypoint" ~/.local/bin/persona-entrypoint
{{- if .Rust}}

# Rust (rustup from nixpkgs; the toolchain lives in ~/.rustup so claude can add targets)
# The minimal profile skips the offline docs (~900 MB).
rustup set profile minimal
rustup default {{if .Rust.Version}}{{shq .Rust.Version}}{{else}}stable{{end}}
rustup component add clippy rustfmt
{{- end}}
{{- if eq .Harness "claude"}}

# Claude Code (native installer)
curl -fsSL https://claude.ai/install.sh | bash
claude --version
{{- end}}
{{- if eq .Harness "pi"}}

# pi coding agent (https://pi.dev); models through OpenRouter (OPENROUTER_API_KEY).
npm install -g --ignore-scripts {{.PiPackage}}
pi --version
install -D -m 0644 "$S/files/pi/settings.json" ~/.pi/agent/settings.json
install -D -m 0644 "$S/files/pi/extensions/persona-status.ts" ~/.pi/agent/extensions/persona-status.ts
install -m 0644 "$S/files/tmux.conf" ~/.tmux.conf
# The persona plugin's skills (pi can't load Claude Code plugins).
if [ -d ` + MarketplaceDir + `/plugins/persona/skills ]; then
  mkdir -p {{.SkillsDir}} && cp -R ` + MarketplaceDir + `/plugins/persona/skills/. {{.SkillsDir}}/
fi
{{- if .PiPackages}}
# pi packages. Failures are non-fatal.
for p in{{range .PiPackages}} {{shq .}}{{end}}; do
  pi install "$p" </dev/null || warn "could not install pi package $p"
done
{{- end}}
{{- end}}
{{- if .Playwright}}

# Playwright CLI with Chromium's headless shell (its system libraries come from flake.nix),
# and the Playwright CLI skills.
npm install -g "@playwright/cli@{{if .Playwright.Version}}{{.Playwright.Version}}{{else}}latest{{end}}"
# Only the headless shell: agents browse headless, and full Chromium adds ~400 MB.
playwright-cli install-browser chromium --only-shell
playwright-cli install --skills || warn "could not install the Playwright CLI skills"
# install --skills pins full Chromium ("channel": "chromium") and downloads it;
# without a channel, headless launches use the shell.
mkdir -p ~/.playwright
echo '{"browser":{"browserName":"chromium","launchOptions":{"headless":true}}}' > ~/.playwright/cli.config.json
find ~/.cache/ms-playwright -mindepth 1 -maxdepth 1 -name 'chromium-*' -exec rm -rf {} +
{{- if eq .Harness "pi"}}
if [ -d ~/.claude/skills/playwright-cli ]; then
  mkdir -p {{.SkillsDir}} && cp -R ~/.claude/skills/playwright-cli {{.SkillsDir}}/
fi
{{- end}}
{{- end}}
{{- if .Tools}}

# Developer tools: linters, formatters, language servers. Failures are non-fatal.
{{- range .Tools}}
{{.Cmd}} || warn {{.Warning}}
{{- end}}
{{- end}}

# Persona instructions.
install -D -m 0644 "$S/files/instructions.md" {{.InstructionsPath}}
{{- if eq .Harness "claude"}}

# Claude Code user settings (settings.json, generated from base-settings.json +
# base + persona). Pre-install every marketplace and enabled plugin it
# declares; failures are non-fatal (Claude Code retries them on first launch).
install -D -m 0644 "$S/settings.json" ~/.claude/settings.json
export CLAUDE_CODE_PLUGIN_GIT_TIMEOUT_MS=300000 # large marketplaces on slow links
jq -r '.extraKnownMarketplaces // {} | to_entries[] | .value.source | (.repo // .url // .path // empty)' ~/.claude/settings.json |
  while read -r src; do claude plugin marketplace add "$src" || warn "could not add marketplace $src"; done
jq -r '.enabledPlugins // {} | to_entries[] | select(.value == true) | .key' ~/.claude/settings.json |
  while read -r id; do claude plugin install "$id" --scope user || warn "could not pre-install $id"; done
{{- end}}
{{- if .Skills}}

# Standalone skills.
mkdir -p {{.SkillsDir}}
{{- range .Skills}}
rm -rf /tmp/skill-src
if git clone --depth 1 {{if .Ref}}--branch {{shq .Ref}} {{end}}{{.Repo}} /tmp/skill-src; then
  rm -rf {{$.SkillsDir}}/{{.Name}}
  cp -R /tmp/skill-src/{{.Path}} {{$.SkillsDir}}/{{.Name}} || warn "skill "{{.Name}}": no "{{.Path}}" in its repository"
else
  warn "could not fetch skill "{{.Name}}
fi
{{- end}}
rm -rf /tmp/skill-src
{{- end}}

# Skip onboarding, pre-trust /workspace, and register MCP servers.
f=~/.claude.json; [ -s "$f" ] || echo '{}' > "$f"
jq --slurpfile mcp "$S/files/mcp.json" \
  '.hasCompletedOnboarding = true | .bypassPermissionsModeAccepted = true | .projects["/workspace"].hasTrustDialogAccepted = true | .projects["/workspace"].hasCompletedProjectOnboarding = true | .mcpServers = ((.mcpServers // {}) + $mcp[0])' \
  "$f" > "$f.tmp" && mv "$f.tmp" "$f"

# Login shells (bash -l, e.g. after the harness exits) start from /etc/profile;
# keep the image's PATH and environment.
grep -q '# persona env' ~/.profile 2>/dev/null || {
  echo '# persona env'
  echo "export PATH=\"$PATH\""
} >> ~/.profile

# Clean caches that only matter while building.
rm -rf ~/.npm/_cacache ~/.cache/go-build ~/.cache/uv /tmp/* 2>/dev/null || true
# Nothing runs source maps, type declarations or packages' own tests; they are
# a third of node_modules. (Markdown stays: packages ship skills as .md.)
for d in ~/.npm-global/lib/node_modules ~/.pi/agent/npm/node_modules; do
  [ -d "$d" ] || continue
  find "$d" -type f \( -name '*.map' -o -name '*.d.ts' -o -name '*.d.mts' -o -name '*.d.cts' -o -name '*.tsbuildinfo' \) -delete
  find "$d" -type d \( -name __tests__ -o -name test -o -name tests \) -prune -exec rm -rf {} +
done

# Every toolchain and the harness must be on PATH.
for b in {{join .Bins " "}}; do
  command -v "$b" >/dev/null || { echo "persona: $b is not on PATH" >&2; exit 1; }
done
{{- if .CustomBins}}
for b in {{join .CustomBins " "}}; do
  command -v "$b" >/dev/null || warn "no $b command on PATH (custom toolchain; it may use another name)"
done
{{- end}}
`))

// gitAuth rewrites GitHub URLs to use GITHUB_TOKEN, via env-only git config
// that lives for the setup run.
const gitAuth = `if [ -n "${GITHUB_TOKEN:-}" ]; then export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.https://x-access-token:${GITHUB_TOKEN}@github.com/.insteadOf" GIT_CONFIG_VALUE_0="https://github.com/"; fi`

// entrypoint starts the harness (pi or Claude Code) inside a detached tmux session so that
// `persona attach` and persona manage can attach to it at any time.
const entrypoint = `#!/bin/sh
set -eu
# Any explicit command bypasses the session (e.g. docker run IMAGE bash).
if [ "$#" -gt 0 ]; then exec "$@"; fi

case "${PERSONA_HARNESS:-claude}" in
pi)
  agent='if [ -n "${PERSONA_PROMPT:-}" ]; then pi "$PERSONA_PROMPT"; else pi; fi' ;;
*)
  # Pre-approve a forwarded API key so Claude Code does not prompt for it.
  if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
    f="$HOME/.claude.json"
    tail=$(printf %s "$ANTHROPIC_API_KEY" | tail -c 20)
    jq --arg k "$tail" '.customApiKeyResponses.approved = (((.customApiKeyResponses.approved // []) + [$k]) | unique)' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
  fi
  # The beacon plugin is a channel: load it into every session. Managed
  # settings approve it (allowedChannelPlugins), so --channels takes it without
  # --dangerously-load-development-channels' confirmation prompt, which would
  # hold the session until someone attached and answered it.
  if [ -d ` + MarketplaceDir + `/plugins/beacon ]; then
    PERSONA_CLAUDE_ARGS="--channels plugin:beacon@persona ${PERSONA_CLAUDE_ARGS:-}"
    export PERSONA_CLAUDE_ARGS
  fi
  agent='if [ -n "${PERSONA_PROMPT:-}" ]; then claude ${PERSONA_CLAUDE_ARGS:-} "$PERSONA_PROMPT"; else claude ${PERSONA_CLAUDE_ARGS:-}; fi' ;;
esac

tmux -u new-session -d -s ` + SessionName + ` -x "${PERSONA_COLS:-200}" -y "${PERSONA_ROWS:-50}" \
  "$agent"'; echo; echo "[${PERSONA_HARNESS:-claude} exited - shell kept open]"; exec bash -l'
tmux set-option -t ` + SessionName + ` status-left "[${PERSONA_NAME:-persona}] " >/dev/null
tmux set-option -t ` + SessionName + ` status-left-length 40 >/dev/null

trap 'tmux kill-server 2>/dev/null; exit 0' TERM INT
while tmux has-session -t ` + SessionName + ` 2>/dev/null; do sleep 1; done`

func mcpJSON(p *Persona) (string, error) {
	servers := map[string]any{}
	for _, s := range p.MCPServers {
		entry := map[string]any{"type": s.Type}
		if s.Type == "stdio" {
			entry["command"] = s.Command
			entry["args"] = nonNil(s.Args)
			if len(s.Env) > 0 {
				entry["env"] = s.Env
			}
		} else {
			entry["url"] = s.URL
			if len(s.Headers) > 0 {
				entry["headers"] = s.Headers
			}
		}
		servers[s.Name] = entry
	}
	b, err := json.MarshalIndent(servers, "", "  ")
	return string(b), err
}

func claudeMD(p *Persona) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Persona: %s\n\n", p.Name)
	if p.Summary != "" {
		b.WriteString(p.Summary + "\n\n")
	}
	if p.Instructions != "" {
		b.WriteString(strings.TrimSpace(p.Instructions) + "\n\n")
	}
	if len(p.Toolchains) > 0 {
		b.WriteString("## Installed toolchains\n\n")
		for _, t := range p.Toolchains {
			ver := t.Version
			if ver == "" {
				if info, ok := toolchainInfo(t.Name); ok {
					ver = info.Default
				}
			}
			line := "- " + t.Name
			if ver != "" {
				line += " (" + ver + ")"
			}
			if t.Nix != "" {
				line += ", nixpkgs " + t.Nix
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}
	if len(p.Tools) > 0 {
		b.WriteString("## Developer tools\n\n")
		for _, t := range p.Tools {
			line := "- " + t.Name
			if t.Reason != "" {
				line += ": " + t.Reason
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\nUse them: run the linters and formatters before you consider work done.\n\n")
	}
	b.WriteString("Node.js (npm, npx) and Python with uv (uvx) are always installed, for MCP servers and tools.\n\n")
	b.WriteString("You are running inside a persona container built with Nix: system packages live in /nix/store " +
		"and are linked into /bin and /lib, so there is no apt. Install more tools in your home directory " +
		"(npm -g, uv tool, cargo, go install, pip --user) or ask the user to add nixpkgs packages to the persona.\n" +
		"The user's project, if any, is mounted at /workspace.\n")
	return b.String()
}

func marketplaceSource(src string) map[string]string {
	if strings.HasPrefix(src, "/") {
		return map[string]string{"source": "directory", "path": src}
	}
	if strings.Contains(src, "://") || strings.HasSuffix(src, ".git") || strings.HasPrefix(src, "git@") {
		return map[string]string{"source": "git", "url": src}
	}
	return map[string]string{"source": "github", "repo": src}
}

func gitURL(repo string) string {
	if strings.Contains(repo, "://") || strings.HasPrefix(repo, "git@") {
		return repo
	}
	return "https://github.com/" + strings.TrimSuffix(repo, ".git") + ".git"
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func optional(s string, ok bool) string {
	if ok {
		return s
	}
	return ""
}

// shellQuote single-quotes s for POSIX sh.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
