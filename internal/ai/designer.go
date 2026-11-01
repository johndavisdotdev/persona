package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/johndavisdev/persona/internal/persona"
)

// Designer turns a natural-language description into a persona spec.
type Designer struct {
	Client       *Client
	Exa          *Exa
	Catalog      *Catalog
	Marketplaces []string // built-in marketplace sources
	// Preinstalled lists plugin ids every persona already gets from the base
	// settings and base requirements; the model should not duplicate them.
	Preinstalled []string
	// Harness is the default harness (the base's, else persona.DefaultHarness)
	// for descriptions that don't ask for one.
	Harness string
	// Progress receives human-readable status lines as the agents work.
	Progress func(string)
	mu       sync.Mutex
}

func (d *Designer) progress(format string, a ...any) {
	if d.Progress != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.Progress(fmt.Sprintf(format, a...))
	}
}

var designerSystem = `You are persona-architect. You design "personas": minimal containers, built with Nix, running a coding agent (the harness: Claude Code by default, or pi), pre-configured with exactly the plugins, skills, MCP servers and toolchains a described role needs.

Process:
1. Interpret the user's persona description: the role, domain, daily tasks, and tools they will touch. Then put yourself in that agent's position on day one: what must already be installed for you to succeed at this job without asking anyone? Languages and their SDKs, build tools, test runners, browsers, database clients, cloud CLIs. Infer all of this from the prose; the user will not list toolchains, and you never ask them.
2. Use search_plugins with several focused queries (domain terms, tool names, task verbs) across the built-in marketplaces.
3. If web_search is available, look for niche third-party Claude Code plugin marketplaces (GitHub repos containing .claude-plugin/marketplace.json) and MCP servers relevant to the domain. Verify any candidate marketplace with inspect_marketplace before using its plugins. Use read_url to check an MCP server's README for its exact launch command.
4. Choose toolchains (see Toolchains below).
5. Call submit_persona with your final selection.

Selection rules:
- Be minimal and precise. Every plugin, MCP server, and skill must clearly serve the persona; prefer 3-10 high-signal items over long lists. Each needs a one-line reason.
- Plugins must be referenced as name@marketplace and must exist in a marketplace you searched or inspected.
- Prefer remote (http) MCP servers when an official one exists; otherwise stdio launched via npx (Node) or uvx (Python). Never invent package names: only use commands you saw documented.
- Secrets are never hard-coded. Reference them as ${VAR_NAME} in MCP env/headers and list VAR_NAME in env so the host forwards it at spawn time.
- Use packages for other CLI tools, as nixpkgs attribute names (e.g. postgresql for psql, sqlite, graphviz, ffmpeg, shellcheck, awscli2). Never use packages for language runtimes; use toolchains.
- instructions becomes the container's CLAUDE.md: write a focused operating guide in second person (role, priorities, conventions, how to use the installed tools). 150-400 words.

Harness (the coding agent the persona's spawns run):
- claude (the default): Claude Code.
- pi: the pi coding agent (https://pi.dev), reaching models through OpenRouter. Choose it only when the description asks for pi. It gets these pi packages by default: @PIPACKAGES@. Add more in harness_packages as "npm:<name>" only when the role clearly benefits and you verified the package is a pi package (its npm metadata has the "pi-package" keyword; check https://registry.npmjs.org/<name> with read_url). Remove a default with "-npm:<name>" only if the description asks.
- Claude Code plugins only work with the claude harness: for pi, leave plugins empty. Skills, MCP servers, toolchains and packages work with both.
- If the description names a model, set model: a Claude Code alias (opus, sonnet, haiku) for claude, or an OpenRouter model id (e.g. anthropic/claude-sonnet-4.5) for pi.

Toolchains (language SDKs and runtimes, from nixpkgs; persona picks the newest stable version of each):
- Every language, framework, or ecosystem the persona works in needs its toolchain: a .NET/C#/ASP.NET/Blazor expert gets dotnet; Rust gets rust; Go gets go; Java/Kotlin/Spring/Gradle gets java; Python/data/ML gets python; Node/TypeScript gets node (bun or deno only when mentioned). Frameworks imply their language.
- Anything web-oriented (a website, web app, frontend, React/Vue/Angular/Svelte/Next.js, HTML/CSS, full-stack, browser or end-to-end testing) gets the playwright toolchain, which brings Node.js, the Playwright CLI with Chromium, its skills, and the playwright plugin. Mention in instructions that playwright-cli can drive a real browser to verify UI work.
- Leave version empty for the default (latest stable; LTS for node and java) unless the description names a version ("NET 8", "Python 3.11", "Go 1.22").
- Built-in toolchains (just give name, version and reason):
@TOOLCHAINS@
- Any other runtime (Zig, Elixir/Erlang, Haskell, OCaml, Julia, Swift, Dart/Flutter, …) is a custom toolchain: set nix to its nixpkgs attribute (e.g. zig, elixir, ghc, ocaml, julia). Images are built with Nix, so that is the install. Only for a runtime nixpkgs doesn't have, if web_search is available, find its primary source (official site or download page, not a blog) and write install: root shell commands from it for a Nix-based image with no apt (curl, tar, unzip and xz are there): detect the architecture with uname -m, download from the official URL, verify a published checksum when there is one, install under /opt or /usr/local, list any bin directory in path, and set source to the page you used.`

func init() {
	var lines []string
	for _, t := range persona.ToolchainCatalog {
		lines = append(lines, fmt.Sprintf("  - %s: %s (default: %s)", t.Name, t.Summary, t.Default))
	}
	designerSystem = strings.Replace(designerSystem, "@TOOLCHAINS@", strings.Join(lines, "\n"), 1)
	designerSystem = strings.Replace(designerSystem, "@PIPACKAGES@", strings.Join(persona.DefaultPiPackages, ", "), 1)
}

func (d *Designer) tools() []Tool {
	str := map[string]any{"type": "string"}
	strArr := map[string]any{"type": "array", "items": str}
	strMap := map[string]any{"type": "object", "additionalProperties": str}
	tools := []Tool{
		{
			Name:        "search_plugins",
			Description: "Keyword search over plugins in all loaded marketplaces (built-in plus any inspected). Returns plugin ids as name@marketplace with descriptions.",
			InputSchema: obj(map[string]any{
				"query":       str,
				"marketplace": map[string]any{"type": "string", "description": "Optional: restrict to one marketplace name."},
				"limit":       map[string]any{"type": "integer", "description": "Max results, default 15."},
			}, "query"),
		},
		{
			Name:        "inspect_marketplace",
			Description: "Load a third-party marketplace (GitHub owner/repo, GitHub URL, or a direct marketplace.json URL). On success its plugins become searchable and usable.",
			InputSchema: obj(map[string]any{"source": str}, "source"),
		},
		{
			Name:        "read_url",
			Description: "Read the text of a web page (e.g. a README) to confirm install commands or configuration.",
			InputSchema: obj(map[string]any{"url": str}, "url"),
		},
		{
			Name:        "submit_persona",
			Description: "Submit the final persona design. Call exactly once.",
			InputSchema: obj(map[string]any{
				"summary":      map[string]any{"type": "string", "description": "One or two sentences describing the persona."},
				"instructions": map[string]any{"type": "string", "description": "Markdown operating guide that becomes CLAUDE.md."},
				"model":        map[string]any{"type": "string", "description": "Optional Claude Code model alias for the container: opus, sonnet, or haiku."},
				"plugins": map[string]any{"type": "array", "items": obj(map[string]any{
					"id": map[string]any{"type": "string", "description": "name@marketplace"}, "reason": str,
				}, "id", "reason")},
				"mcp_servers": map[string]any{"type": "array", "items": obj(map[string]any{
					"name":    str,
					"type":    map[string]any{"type": "string", "enum": []string{"stdio", "http", "sse"}},
					"command": str, "args": strArr, "url": str, "env": strMap, "headers": strMap, "reason": str,
				}, "name", "type", "reason")},
				"skills": map[string]any{"type": "array", "items": obj(map[string]any{
					"name": str, "repo": map[string]any{"type": "string", "description": "GitHub owner/repo or git URL"},
					"path": map[string]any{"type": "string", "description": "Directory of the skill (containing SKILL.md) within the repo"},
					"ref":  str, "reason": str,
				}, "name", "repo", "path", "reason")},
				"packages": map[string]any{"type": "array", "items": str, "description": "nixpkgs attribute names of CLIs the role uses."},
				"harness":  map[string]any{"type": "string", "enum": persona.Harnesses, "description": "The coding agent: claude (default) or pi (only when the description asks for pi)."},
				"harness_packages": map[string]any{"type": "array", "items": str,
					"description": "pi only: extra pi packages (npm:<name>) beyond the defaults; -npm:<name> removes a default."},
				"toolchains": map[string]any{"type": "array", "description": "Language SDKs/runtimes the persona needs.", "items": obj(map[string]any{
					"name":    map[string]any{"type": "string", "description": "A built-in toolchain name, or a lowercase name for a custom runtime."},
					"version": map[string]any{"type": "string", "description": "Only when the description names one; empty for the default."},
					"reason":  str,
					"source":  map[string]any{"type": "string", "description": "Custom installs only: https URL of the primary source the install commands come from."},
					"install": map[string]any{"type": "string", "description": "Only for runtimes nixpkgs lacks: root shell commands from the primary source (Nix-based image, no apt). $VERSION holds the version."},
					"nix":     map[string]any{"type": "string", "description": "Custom toolchains: the nixpkgs attribute."},
					"path":    map[string]any{"type": "array", "items": str, "description": "Custom installs only: directories to add to PATH."},
				}, "name", "reason")},
				"env": map[string]any{"type": "array", "items": str, "description": "Host environment variable names to forward into the container."},
			}, "summary", "instructions", "plugins", "mcp_servers", "skills"),
		},
	}
	if d.Exa.Enabled() {
		tools = append(tools, Tool{
			Name:        "web_search",
			Description: "Search the web with Exa. Use it to discover niche Claude Code plugin marketplaces, skills repos, and MCP servers.",
			InputSchema: obj(map[string]any{"query": str, "num_results": map[string]any{"type": "integer"}}, "query"),
		})
	}
	return tools
}

func obj(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

type submission struct {
	Summary      string `json:"summary"`
	Instructions string `json:"instructions"`
	Model        string `json:"model"`
	Plugins      []struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	} `json:"plugins"`
	MCPServers  []persona.MCPServer `json:"mcp_servers"`
	Skills      []persona.Skill     `json:"skills"`
	Packages    []string            `json:"packages"`
	Toolchains  []persona.Toolchain `json:"toolchains"`
	Env         []string            `json:"env"`
	Harness     string              `json:"harness"`
	HarnessPkgs []string            `json:"harness_packages"`
}

const baseSystemAddendum = `

SPECIAL MODE — BASE REQUIREMENTS: You are not designing a single persona. You maintain the BASE that is merged into EVERY persona. The user states a requirement in plain language (e.g. "All personas should have the Linear plugin from the default Anthropic marketplace"). Apply exactly that change to the current base: keep every existing item unless the user asks to remove or replace it, and do not add anything the user did not ask for. "The default Anthropic marketplace" means claude-plugins-official. In submit_persona, set summary to a one-line description of the change; set instructions only to org-wide guidance that every persona must follow (keep any existing base instructions; use an empty string if there are none).`

// DesignBase applies a plain-language requirement to the base requirements.
func (d *Designer) DesignBase(ctx context.Context, requirement string, current *persona.Base) (*persona.Base, string, error) {
	p, err := d.design(ctx, "base", requirement, current.AsPersona(), true)
	if err != nil {
		return nil, "", err
	}
	nb := *current
	nb.SetFromPersona(p)
	nb.Requirements = append(nb.Requirements, requirement)
	return &nb, p.Summary, nil
}

// Design produces a persona with three specialist agents working in
// parallel (see designParallel). If prev is non-nil, they revise it.
func (d *Designer) Design(ctx context.Context, name, description string, prev *persona.Persona) (*persona.Persona, error) {
	return d.designParallel(ctx, name, description, prev)
}

func (d *Designer) design(ctx context.Context, name, description string, prev *persona.Persona, baseMode bool) (*persona.Persona, error) {
	d.progress("Loading %d built-in marketplaces", len(d.Marketplaces))
	for _, err := range d.Catalog.LoadAll(ctx, d.Marketplaces) {
		d.progress("warning: %v", err)
	}
	if prev != nil {
		for _, m := range prev.Marketplaces {
			if _, err := d.Catalog.Load(ctx, m.Source); err != nil {
				d.progress("warning: %v", err)
			}
		}
	}

	var b strings.Builder
	if baseMode {
		fmt.Fprintf(&b, "Requested change to the base requirements:\n%s\n\n", description)
	} else {
		fmt.Fprintf(&b, "Persona name: %s\n\nPersona description:\n%s\n\n", name, description)
	}
	fmt.Fprintf(&b, "Loaded marketplaces:\n- %s\n", strings.Join(d.Catalog.Names(), "\n- "))
	if len(d.Preinstalled) > 0 && !baseMode {
		fmt.Fprintf(&b, "\nEvery persona already gets these plugins from the base settings (do not include them again):\n- %s\n", strings.Join(d.Preinstalled, "\n- "))
	}
	if !d.Exa.Enabled() {
		b.WriteString("\nWeb search is unavailable for this run; use only the loaded marketplaces and well-known MCP servers you can verify with read_url.\n")
	}
	if prev != nil && baseMode {
		y, _ := yaml.Marshal(prev)
		fmt.Fprintf(&b, "\nCurrent base (submit the COMPLETE updated base):\n```yaml\n%s```\n", y)
	} else if prev != nil {
		y, _ := yaml.Marshal(prev)
		fmt.Fprintf(&b, "\nThis is a REVISION of an existing persona. Keep what still fits, change what the new description requires. Current definition:\n```yaml\n%s```\n", y)
	}

	var result *persona.Persona
	nudged := false
	handler := func(ctx context.Context, tool string, input json.RawMessage) (string, bool, error) {
		if out, handled, err := d.researchTool(ctx, "", tool, input); handled {
			return out, false, err
		}
		switch tool {
		case "submit_persona":
			var s submission
			if err := json.Unmarshal(input, &s); err != nil {
				return "", false, err
			}
			p, err := d.toPersona(name, description, s, prev, baseMode)
			if err != nil {
				d.progress("Rejected submission: %v", err)
				return "", false, err
			}
			// Once, point out toolchains the description implies but the
			// submission lacks; the model decides.
			if !baseMode && !nudged {
				nudged = true
				if missing := MissingToolchains(description, p.Toolchains); len(missing) > 0 {
					d.progress("Asking the designer to reconsider toolchains: %s", strings.Join(missing, ", "))
					return "Not accepted yet. The description mentions " + strings.Join(missing, "; ") +
						", but toolchains doesn't include it. Add the toolchain(s) and call submit_persona again; if one truly isn't needed, call submit_persona again unchanged.", false, nil
				}
			}
			if !baseMode {
				// Every language the description mentions is installed.
				if added := EnsureMentioned(description, p); len(added) > 0 {
					d.progress("Added what the description mentions: %s", strings.Join(added, ", "))
				}
				if h := MentionedHarness(description); h != "" && h != p.Harness {
					d.progress("Using the %s harness, as the description asks", h)
					p.Harness = h
				}
				p.Normalize()
			}
			result = p
			d.progress("Design complete: %d plugins, %d MCP servers, %d skills, %d toolchains", len(p.Plugins), len(p.MCPServers), len(p.Skills), len(p.Toolchains))
			return "Persona accepted.", true, nil
		}
		return "", false, fmt.Errorf("unknown tool %q", tool)
	}

	d.progress("Designing persona with %s", d.Client.Model)
	system := designerSystem
	if baseMode {
		system += baseSystemAddendum
	}
	if err := d.Client.RunTools(ctx, system, b.String(), d.tools(), handler, 40); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("the model did not submit a persona")
	}
	return result, nil
}

// researchTool runs the tools all designer agents share: plugin search,
// marketplace inspection, web search and reading pages. label prefixes
// progress lines (the agent's name, when several run at once).
func (d *Designer) researchTool(ctx context.Context, label, tool string, input json.RawMessage) (string, bool, error) {
	say := func(format string, a ...any) { d.progress(label+format, a...) }
	switch tool {
	case "search_plugins":
		var in struct {
			Query, Marketplace string
			Limit              int
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "", true, err
		}
		say("Searching plugins: %q", in.Query)
		hits := d.Catalog.Search(in.Query, in.Marketplace, in.Limit)
		if len(hits) == 0 {
			return "No matching plugins.", true, nil
		}
		out, _ := json.Marshal(hits)
		return string(out), true, nil

	case "inspect_marketplace":
		var in struct{ Source string }
		if err := json.Unmarshal(input, &in); err != nil {
			return "", true, err
		}
		say("Inspecting marketplace %s", in.Source)
		m, err := d.Catalog.Load(ctx, in.Source)
		if err != nil {
			return "", true, err
		}
		var lines []string
		for i, p := range m.Plugins {
			if i == 60 {
				lines = append(lines, fmt.Sprintf("… and %d more (use search_plugins with marketplace=%q)", len(m.Plugins)-60, m.Name))
				break
			}
			lines = append(lines, fmt.Sprintf("%s@%s: %s", p.Name, m.Name, truncate(p.Description, 160)))
		}
		return fmt.Sprintf("Marketplace %q (source %s) loaded with %d plugins:\n%s", m.Name, m.Source, len(m.Plugins), strings.Join(lines, "\n")), true, nil

	case "web_search":
		var in struct {
			Query      string
			NumResults int `json:"num_results"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "", true, err
		}
		say("Exa search: %q", in.Query)
		res, err := d.Exa.Search(ctx, in.Query, in.NumResults)
		if err != nil {
			return "", true, err
		}
		for i := range res {
			res[i].Text = truncate(res[i].Text, 700)
		}
		out, _ := json.Marshal(res)
		return string(out), true, nil

	case "read_url":
		var in struct{ URL string }
		if err := json.Unmarshal(input, &in); err != nil {
			return "", true, err
		}
		say("Reading %s", in.URL)
		out, _, err := d.readURL(ctx, in.URL)
		return out, true, err
	}
	return "", false, nil
}

func (d *Designer) readURL(ctx context.Context, url string) (string, bool, error) {
	if d.Exa.Enabled() {
		if res, err := d.Exa.Contents(ctx, []string{url}, 12000); err == nil && len(res) > 0 && res[0].Text != "" {
			return res[0].Text, false, nil
		}
	}
	if strings.HasPrefix(url, "https://github.com/") && strings.Count(strings.TrimPrefix(url, "https://github.com/"), "/") == 1 {
		url = "https://raw.githubusercontent.com/" + strings.TrimPrefix(url, "https://github.com/") + "/HEAD/README.md"
	}
	b, err := httpGet(ctx, &http.Client{Timeout: 30 * time.Second}, url, 200<<10)
	if err != nil {
		return "", false, err
	}
	return truncate(string(b), 12000), false, nil
}

// toPersona validates a submission against the catalog and builds the spec.
func (d *Designer) toPersona(name, description string, s submission, prev *persona.Persona, baseMode bool) (*persona.Persona, error) {
	p := &persona.Persona{
		Name:            name,
		Description:     description,
		Summary:         strings.TrimSpace(s.Summary),
		Instructions:    strings.TrimSpace(s.Instructions),
		Model:           s.Model,
		MCPServers:      s.MCPServers,
		Skills:          s.Skills,
		Packages:        s.Packages,
		Toolchains:      s.Toolchains,
		Env:             s.Env,
		Harness:         s.Harness,
		HarnessPackages: s.HarnessPkgs,
	}
	if prev != nil {
		p.CreatedAt, p.FileHashes = prev.CreatedAt, prev.FileHashes
	}
	var problems []string
	usedMP := map[string]bool{}
	for _, pl := range s.Plugins {
		n, mp, ok := strings.Cut(pl.ID, "@")
		if !ok || n == "" || mp == "" {
			problems = append(problems, fmt.Sprintf("plugin %q must be name@marketplace", pl.ID))
			continue
		}
		if !d.Catalog.Has(n, mp) {
			problems = append(problems, fmt.Sprintf("plugin %q not found in any loaded marketplace (search or inspect first)", pl.ID))
			continue
		}
		p.Plugins = append(p.Plugins, persona.Plugin{Name: n, Marketplace: mp, Reason: pl.Reason})
		usedMP[mp] = true
	}
	for mp := range usedMP {
		m, _ := d.Catalog.Get(mp)
		p.Marketplaces = append(p.Marketplaces, persona.Marketplace{Name: m.Name, Source: m.Source})
	}
	for _, m := range p.MCPServers {
		switch m.Type {
		case "stdio":
			if m.Command == "" {
				problems = append(problems, fmt.Sprintf("stdio MCP server %q needs a command", m.Name))
			}
		case "http", "sse":
			if !strings.HasPrefix(m.URL, "http") {
				problems = append(problems, fmt.Sprintf("%s MCP server %q needs an http(s) url", m.Type, m.Name))
			}
		default:
			problems = append(problems, fmt.Sprintf("MCP server %q has invalid type %q", m.Name, m.Type))
		}
	}
	for _, sk := range p.Skills {
		if persona.ValidateName(sk.Name) != nil {
			problems = append(problems, fmt.Sprintf("skill name %q must be lowercase letters, digits, and dashes", sk.Name))
		}
	}
	for _, t := range p.Toolchains {
		if err := persona.ValidateToolchain(t); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if err := persona.ValidateHarness(p.Harness); err != nil {
		problems = append(problems, err.Error())
	}
	for _, pk := range p.HarnessPackages {
		if err := persona.ValidateHarnessPackage(pk); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if p.Instructions == "" && !baseMode {
		problems = append(problems, "instructions must not be empty")
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("fix these problems and call submit_persona again:\n- %s", strings.Join(problems, "\n- "))
	}
	p.Normalize()
	return p, nil
}
