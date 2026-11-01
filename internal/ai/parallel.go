package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/johndavisdev/persona/internal/persona"
)

// A persona is designed by three specialist agents working at the same time,
// each with its own tools and submit step, then one short call that writes
// the summary and instructions for the combined result:
//
//	runtimes    language toolchains, from their primary sources
//	extensions  skills, MCP servers, and (for Claude Code) plugins
//	tooling     the harness's setup: packages, LSP, linters, formatters, CLIs

const specialistIntro = `You are one of three specialists designing a "persona": a minimal container, built with Nix, running a coding agent (the harness) set up for a role. The three of you work at the same time on separate parts; stay within yours. Infer everything from the description: the user will not list components, and you never ask them. Put yourself in the persona's position on day one: what must already be there to do this job well?

`

const runtimesPrompt = specialistIntro + `YOUR PART: language runtimes and SDKs (toolchains). The others handle skills, MCP servers, plugins, linters and formatters.

Toolchains (from nixpkgs; persona picks the newest stable version of each):
- Every language, framework, or ecosystem the persona works in needs its toolchain: a .NET/C#/ASP.NET/Blazor expert gets dotnet; Rust gets rust; Go gets go; Java/Kotlin/Spring/Gradle gets java; Python/data/ML gets python; Node/TypeScript gets node (bun or deno only when mentioned). Frameworks imply their language.
- Anything web-oriented (a website, web app, frontend, React/Vue/Angular/Svelte/Next.js, HTML/CSS, full-stack, browser or end-to-end testing) gets the playwright toolchain (Node.js, the Playwright CLI with Chromium, its skills, and the playwright plugin).
- Leave version empty for the default (latest stable; LTS for node and java) unless the description names a version ("NET 8", "Python 3.11", "Go 1.22").
- Built-in toolchains (just give name, version and reason):
@TOOLCHAINS@
- Any other runtime (Zig, Elixir/Erlang, Haskell, OCaml, Julia, Swift, Dart/Flutter, …) is a custom toolchain: set nix to its nixpkgs attribute (e.g. zig, elixir, ghc, ocaml, julia). Images are built with Nix, so that is the install. Only for a runtime nixpkgs doesn't have, if web_search is available, find its primary source (official site or download page, not a blog) and write install: root shell commands from it for a Nix-based image with no apt (curl, tar, unzip and xz are there): detect the architecture with uname -m, download from the official URL, verify a published checksum when there is one, install under /opt or /usr/local, list any bin directory in path, and set source to the page you used.

Call submit_toolchains with your selection (an empty list if the role needs no language).`

const extensionsPrompt = specialistIntro + `YOUR PART: skills, MCP servers, and plugins. The others handle language toolchains and developer tooling (LSP, linters, formatters, CLIs).

- Use search_plugins with several focused queries (domain terms, tool names, task verbs) across the loaded marketplaces.
- If web_search is available, look for first- and third-party skills (GitHub repos with SKILL.md files, e.g. anthropics/skills), MCP servers for the domain's services, and niche Claude Code plugin marketplaces (repos with .claude-plugin/marketplace.json; verify with inspect_marketplace before using their plugins). Use read_url on a README to confirm an MCP server's exact launch command or a skill's path.
- Plugins work only with the claude harness: @PLUGINRULE@
- Be minimal and precise: every item must clearly serve the persona; prefer 3-10 high-signal items over long lists. Each needs a one-line reason.
- Plugins are name@marketplace and must exist in a marketplace you searched or inspected.
- Prefer remote (http) MCP servers when an official one exists; otherwise stdio launched via npx (Node) or uvx (Python). Never invent package names: only use commands you saw documented.
- Secrets are never hard-coded: reference them as ${VAR_NAME} in MCP env/headers and list VAR_NAME in env so the host forwards it.

Call submit_extensions with your selection.`

const toolingPrompt = specialistIntro + `YOUR PART: configure the harness for the role's daily work: language servers (LSP), linters, formatters, test runners, and the CLIs the role uses. The others handle language runtimes (toolchains), skills and MCP servers.

The harness is @HARNESS@.
@HARNESSRULE@

Developer tools go in tools, installed with a package manager: npm (e.g. eslint, prettier, typescript-language-server), uv (Python tools: ruff, basedpyright), go (gopls: golang.org/x/tools/gopls, golangci-lint: github.com/golangci/golangci-lint/v2/cmd/golangci-lint), cargo (Rust crates), rustup (rust-analyzer, clippy), dotnet (global tools such as csharp-ls, dotnet-format), or nix (nixpkgs attributes, for tools packaged there: shellcheck, shfmt, hadolint, golangci-lint, clang-tools). The installer's toolchain is added automatically. Give the exact package name the installer takes; only use packages you are confident exist (check npm with read_url on https://registry.npmjs.org/<name> when unsure).
- packages are other CLIs the role uses, as nixpkgs attribute names (postgresql for psql, sqlite, graphviz, ffmpeg, awscli2, kubectl, terraform, gh, …). Use exact attribute names; when unsure, check with read_url on https://search.nixos.org/packages?channel=unstable&query=<name>. Never put language runtimes there (they are toolchains), and don't repeat what every image has (git, curl, jq, ripgrep, tmux, make, gcc).
- If the description names a model, set model: @MODELRULE@
- Be practical: the formatter, linter and language server for each language the role uses, plus the CLIs its work needs. Each tool needs a one-line reason.

Call submit_tooling with your selection.`

const finalizePrompt = `You write the operating guide for a "persona": a coding agent (harness: @HARNESS@) set up for a role. You get the persona's description and its final components. Write:
- summary: one or two sentences describing the persona.
- instructions: a focused operating guide in second person, 150-400 words of Markdown, which becomes the agent's @INSTRUCTIONSFILE@: the role, its priorities and conventions, and how to use the installed toolchains, tools, skills and MCP servers. Tell it to run the linters and formatters before calling work done. If playwright is installed, say playwright-cli can drive a real browser to verify UI work.
Call submit_summary.`

func fill(s string, kv ...string) string {
	for i := 0; i+1 < len(kv); i += 2 {
		s = strings.ReplaceAll(s, kv[i], kv[i+1])
	}
	return s
}

type agentSpec struct {
	label  string
	system string
	tools  []Tool
	submit string
	// accept validates a submission; an error goes back to the agent.
	accept func(input json.RawMessage) error
}

// runAgent runs one specialist until it submits.
func (d *Designer) runAgent(ctx context.Context, a agentSpec, user string) error {
	submitted := false
	handler := func(ctx context.Context, tool string, input json.RawMessage) (string, bool, error) {
		if out, handled, err := d.researchTool(ctx, a.label, tool, input); handled {
			return out, false, err
		}
		if tool != a.submit {
			return "", false, fmt.Errorf("unknown tool %q", tool)
		}
		if err := a.accept(input); err != nil {
			d.progress("%sRejected submission: %v", a.label, err)
			return "", false, err
		}
		submitted = true
		return "Accepted.", true, nil
	}
	if err := d.Client.RunTools(ctx, a.system, user, a.tools, handler, 30); err != nil {
		return fmt.Errorf("%s: %w", strings.Trim(a.label, "[] "), err)
	}
	if !submitted {
		return fmt.Errorf("%s did not submit", strings.Trim(a.label, "[] "))
	}
	return nil
}

func (d *Designer) researchTools(plugins bool) []Tool {
	str := map[string]any{"type": "string"}
	tools := []Tool{{
		Name:        "read_url",
		Description: "Read the text of a web page or JSON endpoint (a README, docs, an npm registry entry) to confirm names, URLs and commands.",
		InputSchema: obj(map[string]any{"url": str}, "url"),
	}}
	if plugins {
		tools = append(tools,
			Tool{Name: "search_plugins", Description: "Keyword search over plugins in all loaded marketplaces (built-in plus any inspected). Returns plugin ids as name@marketplace with descriptions.",
				InputSchema: obj(map[string]any{"query": str, "marketplace": str, "limit": map[string]any{"type": "integer"}}, "query")},
			Tool{Name: "inspect_marketplace", Description: "Load a third-party marketplace (GitHub owner/repo, GitHub URL, or a direct marketplace.json URL). On success its plugins become searchable and usable.",
				InputSchema: obj(map[string]any{"source": str}, "source")},
		)
	}
	if d.Exa.Enabled() {
		tools = append(tools, Tool{Name: "web_search", Description: "Search the web with Exa.",
			InputSchema: obj(map[string]any{"query": str, "num_results": map[string]any{"type": "integer"}}, "query")})
	}
	return tools
}

// designParallel designs a persona with the three specialists and a final
// writer, then applies the guarantees (mentioned languages and harness).
func (d *Designer) designParallel(ctx context.Context, name, description string, prev *persona.Persona) (*persona.Persona, error) {
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

	harness := MentionedHarness(description)
	if harness == "" && prev != nil && prev.Harness != "" {
		harness = prev.Harness
	}
	if harness == "" {
		harness = d.Harness
	}
	if harness == "" {
		harness = persona.DefaultHarness
	}
	claude := harness == persona.HarnessClaude

	var ctxText strings.Builder
	fmt.Fprintf(&ctxText, "Persona name: %s\n\nPersona description:\n%s\n\nHarness: %s\n", name, description, harness)
	if !d.Exa.Enabled() {
		ctxText.WriteString("\nWeb search is unavailable for this run; rely on what you know and can verify with read_url.\n")
	}
	revision := func(part any) string {
		if prev == nil {
			return ""
		}
		y, _ := yaml.Marshal(part)
		return fmt.Sprintf("\nThis is a REVISION. Keep what still fits, change what the new description requires. Current selection for your part:\n```yaml\n%s```\n", y)
	}

	var (
		mu      sync.Mutex
		result  = &persona.Persona{Name: name, Description: description, Harness: harness}
		nudged  bool
		strArr  = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		str     = map[string]any{"type": "string"}
		strMap  = map[string]any{"type": "object", "additionalProperties": str}
		catalog []string
	)
	for _, t := range persona.ToolchainCatalog {
		catalog = append(catalog, fmt.Sprintf("  - %s: %s (default: %s)", t.Name, t.Summary, t.Default))
	}
	if prev != nil {
		result.CreatedAt, result.FileHashes = prev.CreatedAt, prev.FileHashes
		result.Settings = prev.Settings
	}

	runtimes := agentSpec{
		label:  "[runtimes] ",
		system: fill(runtimesPrompt, "@TOOLCHAINS@", strings.Join(catalog, "\n")),
		tools: append(d.researchTools(false), Tool{Name: "submit_toolchains", Description: "Submit the toolchains. Call once.",
			InputSchema: obj(map[string]any{"toolchains": map[string]any{"type": "array", "items": obj(map[string]any{
				"name":    map[string]any{"type": "string", "description": "A built-in toolchain name, or a lowercase name for a custom runtime."},
				"version": map[string]any{"type": "string", "description": "Only when the description names one."},
				"reason":  str,
				"source":  map[string]any{"type": "string", "description": "Custom installs: https URL of the primary source the install commands come from."},
				"install": map[string]any{"type": "string", "description": "Only for runtimes nixpkgs lacks: root shell commands from the primary source (Nix-based image, no apt). $VERSION holds the version."},
				"nix":     map[string]any{"type": "string", "description": "Custom toolchains: the nixpkgs attribute."},
				"path":    map[string]any{"type": "array", "items": str, "description": "Custom installs: directories to add to PATH."},
			}, "name", "reason")}}, "toolchains")}),
		submit: "submit_toolchains",
		accept: func(input json.RawMessage) error {
			var in struct{ Toolchains []persona.Toolchain }
			if err := json.Unmarshal(input, &in); err != nil {
				return err
			}
			var problems []string
			for _, t := range in.Toolchains {
				if err := persona.ValidateToolchain(t); err != nil {
					problems = append(problems, err.Error())
				}
			}
			if len(problems) > 0 {
				return fmt.Errorf("fix and submit again:\n- %s", strings.Join(problems, "\n- "))
			}
			if !nudged {
				nudged = true
				if missing := MissingToolchains(description, in.Toolchains); len(missing) > 0 {
					d.progress("[runtimes] Asking to reconsider: %s", strings.Join(missing, ", "))
					return fmt.Errorf("the description mentions %s, but toolchains doesn't include it. Add it and submit again; if it truly isn't needed, submit again unchanged", strings.Join(missing, "; "))
				}
			}
			mu.Lock()
			result.Toolchains = in.Toolchains
			mu.Unlock()
			d.progress("[runtimes] Chose %d toolchains", len(in.Toolchains))
			return nil
		},
	}

	pluginRule := "this persona uses Claude Code, so pick plugins where they help."
	if !claude {
		pluginRule = "this persona uses pi, so submit no plugins; skills and MCP servers work with pi."
	}
	extensions := agentSpec{
		label:  "[extensions] ",
		system: fill(extensionsPrompt, "@PLUGINRULE@", pluginRule),
		tools: append(d.researchTools(true), Tool{Name: "submit_extensions", Description: "Submit skills, MCP servers and plugins. Call once.",
			InputSchema: obj(map[string]any{
				"plugins": map[string]any{"type": "array", "items": obj(map[string]any{"id": map[string]any{"type": "string", "description": "name@marketplace"}, "reason": str}, "id", "reason")},
				"mcp_servers": map[string]any{"type": "array", "items": obj(map[string]any{
					"name": str, "type": map[string]any{"type": "string", "enum": []string{"stdio", "http", "sse"}},
					"command": str, "args": strArr, "url": str, "env": strMap, "headers": strMap, "reason": str,
				}, "name", "type", "reason")},
				"skills": map[string]any{"type": "array", "items": obj(map[string]any{
					"name": str, "repo": map[string]any{"type": "string", "description": "GitHub owner/repo or git URL"},
					"path": map[string]any{"type": "string", "description": "Directory of the skill (containing SKILL.md) within the repo"},
					"ref":  str, "reason": str,
				}, "name", "repo", "path", "reason")},
				"env": map[string]any{"type": "array", "items": str, "description": "Host environment variable names to forward."},
			}, "plugins", "mcp_servers", "skills")}),
		submit: "submit_extensions",
		accept: func(input json.RawMessage) error {
			var in struct {
				Plugins []struct{ ID, Reason string }
				MCP     []persona.MCPServer `json:"mcp_servers"`
				Skills  []persona.Skill
				Env     []string
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return err
			}
			var problems []string
			var plugins []persona.Plugin
			var markets []persona.Marketplace
			seenMP := map[string]bool{}
			for _, pl := range in.Plugins {
				p, mp, err := d.checkPlugin(pl.ID, pl.Reason)
				if err != nil {
					problems = append(problems, err.Error())
					continue
				}
				if !claude {
					problems = append(problems, fmt.Sprintf("plugin %s: plugins only work with the claude harness; this persona uses pi", pl.ID))
					continue
				}
				plugins = append(plugins, p)
				if !seenMP[mp.Name] {
					seenMP[mp.Name] = true
					markets = append(markets, mp)
				}
			}
			problems = append(problems, checkMCP(in.MCP)...)
			for _, sk := range in.Skills {
				if persona.ValidateName(sk.Name) != nil {
					problems = append(problems, fmt.Sprintf("skill name %q must be lowercase letters, digits, and dashes", sk.Name))
				}
			}
			if len(problems) > 0 {
				return fmt.Errorf("fix and submit again:\n- %s", strings.Join(problems, "\n- "))
			}
			mu.Lock()
			result.Plugins = append(result.Plugins, plugins...)
			result.Marketplaces = append(result.Marketplaces, markets...)
			result.MCPServers, result.Skills, result.Env = in.MCP, in.Skills, in.Env
			mu.Unlock()
			d.progress("[extensions] Chose %d plugins, %d MCP servers, %d skills", len(plugins), len(in.MCP), len(in.Skills))
			return nil
		},
	}

	harnessRule := fmt.Sprintf(`It is pi: language-server and editor features come from pi packages. It already gets: %s. Add pi packages in harness_packages ("npm:<name>") only after confirming they are pi packages: search with read_url on https://registry.npmjs.org/-/v1/search?text=keywords:pi-package+<term> (for example lsp), and check the result's keywords.`,
		strings.Join(persona.DefaultPiPackages, ", "))
	modelRule := "an OpenRouter model id (e.g. anthropic/claude-sonnet-4.5)."
	if claude {
		harnessRule = "It is Claude Code: language-server support comes from LSP plugins. Find them with search_plugins (try \"lsp\" and the language names) and add them to plugins; they also need the language server itself installed as a tool."
		modelRule = "a Claude Code alias (opus, sonnet, haiku)."
	}
	tooling := agentSpec{
		label:  "[tooling] ",
		system: fill(toolingPrompt, "@HARNESS@", harness, "@HARNESSRULE@", harnessRule, "@MODELRULE@", modelRule),
		tools: append(d.researchTools(claude), Tool{Name: "submit_tooling", Description: "Submit the harness setup and developer tools. Call once.",
			InputSchema: obj(map[string]any{
				"tools": map[string]any{"type": "array", "items": obj(map[string]any{
					"name":    str,
					"via":     map[string]any{"type": "string", "enum": persona.ToolInstallers()},
					"package": map[string]any{"type": "string", "description": "What the installer takes, e.g. eslint, golang.org/x/tools/gopls, rust-analyzer"},
					"reason":  str,
				}, "name", "via", "package", "reason")},
				"packages":         map[string]any{"type": "array", "items": str, "description": "nixpkgs attribute names of CLIs the role uses."},
				"harness_packages": map[string]any{"type": "array", "items": str, "description": "pi only: extra pi packages (npm:<name>)."},
				"plugins":          map[string]any{"type": "array", "items": obj(map[string]any{"id": map[string]any{"type": "string", "description": "name@marketplace"}, "reason": str}, "id", "reason"), "description": "Claude Code only: LSP plugins."},
				"model":            str,
			}, "tools")}),
		submit: "submit_tooling",
		accept: func(input json.RawMessage) error {
			var in struct {
				Tools           []persona.Tool
				Packages        []string `json:"packages"`
				HarnessPackages []string `json:"harness_packages"`
				Plugins         []struct{ ID, Reason string }
				Model           string
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return err
			}
			var problems []string
			for _, t := range in.Tools {
				if err := persona.ValidateTool(t); err != nil {
					problems = append(problems, err.Error())
				}
			}
			for _, pk := range in.Packages {
				if err := persona.ValidateNixPackage(pk); err != nil {
					problems = append(problems, err.Error())
				}
			}
			for _, pk := range in.HarnessPackages {
				if err := persona.ValidateHarnessPackage(pk); err != nil {
					problems = append(problems, err.Error())
				} else if claude {
					problems = append(problems, "harness_packages are for pi; this persona uses Claude Code")
				}
			}
			var plugins []persona.Plugin
			var markets []persona.Marketplace
			for _, pl := range in.Plugins {
				p, mp, err := d.checkPlugin(pl.ID, pl.Reason)
				if err != nil {
					problems = append(problems, err.Error())
					continue
				}
				if !claude {
					problems = append(problems, fmt.Sprintf("plugin %s: plugins only work with the claude harness", pl.ID))
					continue
				}
				plugins, markets = append(plugins, p), append(markets, mp)
			}
			if len(problems) > 0 {
				return fmt.Errorf("fix and submit again:\n- %s", strings.Join(problems, "\n- "))
			}
			mu.Lock()
			result.Tools, result.Packages, result.HarnessPackages, result.Model = in.Tools, in.Packages, in.HarnessPackages, in.Model
			result.AptPackages = nil
			result.Plugins = append(result.Plugins, plugins...)
			result.Marketplaces = append(result.Marketplaces, markets...)
			mu.Unlock()
			d.progress("[tooling] Chose %d tools, %d nixpkgs packages, %d harness packages, %d LSP plugins", len(in.Tools), len(in.Packages), len(in.HarnessPackages), len(plugins))
			return nil
		},
	}

	var prevRuntimes, prevExt, prevTooling any
	if prev != nil {
		prevRuntimes = map[string]any{"toolchains": prev.Toolchains}
		prevExt = map[string]any{"plugins": prev.Plugins, "mcp_servers": prev.MCPServers, "skills": prev.Skills, "env": prev.Env}
		prevTooling = map[string]any{"tools": prev.Tools, "packages": prev.Packages, "legacy_apt_packages": prev.AptPackages, "harness_packages": prev.HarnessPackages, "model": prev.Model}
	}
	extUser := ctxText.String() + fmt.Sprintf("\nLoaded marketplaces:\n- %s\n", strings.Join(d.Catalog.Names(), "\n- "))
	if len(d.Preinstalled) > 0 {
		extUser += fmt.Sprintf("\nEvery persona already gets these plugins (don't repeat them):\n- %s\n", strings.Join(d.Preinstalled, "\n- "))
	}
	jobs := []struct {
		spec agentSpec
		user string
	}{
		{runtimes, ctxText.String() + revision(prevRuntimes)},
		{extensions, extUser + revision(prevExt)},
		{tooling, ctxText.String() + revision(prevTooling)},
	}
	d.progress("Designing %s with three agents in parallel (%s)", name, d.Client.Model)
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = d.runAgent(ctx, j.spec, j.user)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	// The guarantees, before the guide is written so it covers them.
	if added := EnsureMentioned(description, result); len(added) > 0 {
		d.progress("Added what the description mentions: %s", strings.Join(added, ", "))
	}
	result.Normalize()

	instructionsFile := "CLAUDE.md"
	if !claude {
		instructionsFile = "AGENTS.md (pi)"
	}
	spec := *result
	spec.Description = ""
	y, _ := yaml.Marshal(&spec)
	final := agentSpec{
		label:  "[guide] ",
		system: fill(finalizePrompt, "@HARNESS@", harness, "@INSTRUCTIONSFILE@", instructionsFile),
		tools: []Tool{{Name: "submit_summary", Description: "Submit the summary and instructions.",
			InputSchema: obj(map[string]any{"summary": str, "instructions": str}, "summary", "instructions")}},
		submit: "submit_summary",
		accept: func(input json.RawMessage) error {
			var in struct{ Summary, Instructions string }
			if err := json.Unmarshal(input, &in); err != nil {
				return err
			}
			if strings.TrimSpace(in.Instructions) == "" {
				return errors.New("instructions must not be empty")
			}
			result.Summary, result.Instructions = strings.TrimSpace(in.Summary), strings.TrimSpace(in.Instructions)
			return nil
		},
	}
	d.progress("Writing the operating guide")
	if err := d.runAgent(ctx, final, fmt.Sprintf("Description:\n%s\n\nFinal components:\n```yaml\n%s```\n", description, y)); err != nil {
		return nil, err
	}
	result.Normalize()
	d.progress("Design complete: %d toolchains, %d tools, %d plugins, %d MCP servers, %d skills (%s harness)",
		len(result.Toolchains), len(result.Tools), len(result.Plugins), len(result.MCPServers), len(result.Skills), harness)
	return result, nil
}

// checkPlugin resolves name@marketplace against the catalog.
func (d *Designer) checkPlugin(id, reason string) (persona.Plugin, persona.Marketplace, error) {
	n, mp, ok := strings.Cut(id, "@")
	if !ok || n == "" || mp == "" {
		return persona.Plugin{}, persona.Marketplace{}, fmt.Errorf("plugin %q must be name@marketplace", id)
	}
	if !d.Catalog.Has(n, mp) {
		return persona.Plugin{}, persona.Marketplace{}, fmt.Errorf("plugin %q not found in any loaded marketplace (search or inspect first)", id)
	}
	m, _ := d.Catalog.Get(mp)
	return persona.Plugin{Name: n, Marketplace: mp, Reason: reason}, persona.Marketplace{Name: m.Name, Source: m.Source}, nil
}

func checkMCP(servers []persona.MCPServer) []string {
	var problems []string
	for _, m := range servers {
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
	return problems
}
