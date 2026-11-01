# persona

Build, spawn, and multiplex containerized **Claude Code personas**: minimal Debian images that come with the
plugins, skills, and MCP servers a role needs already set up.

```
persona new                      # AI wizard: describe a persona → Dockerfile → image
persona spawn reviewer -a        # start a container and attach to Claude Code
persona control-center           # TUI: split panes + tabs across running personas
```

## Requirements

- Docker (Docker Desktop, OrbStack, Colima, …) with BuildKit (the default)
- `ANTHROPIC_API_KEY`, used by the persona designer and forwarded into containers.
  You can use `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token` for the containers instead.
- Optional: `EXA_API_KEY` lets the designer search the web for niche third-party marketplaces and MCP servers.

## Commands

| Command | What it does |
|---|---|
| `list` (`-i` for instances, `--json`) | Personas with build state and running count |
| `new [name]` | Guided wizard. Claude reads your description, searches the built-in marketplaces (`anthropics/claude-plugins-official`, `anthropics/claude-code`, `anthropics/skills`) and, with Exa, third-party ones, then generates a Dockerfile and builds it. `--from-spec file.yaml` imports a definition without AI. |
| `update <name>` | `--description`/`--redesign` asks Claude to revise the persona. `--add-plugin`, `--remove-mcp`, `--model`, `--add-env`, `--add-apt`, … make precise edits. Rebuilds afterwards. |
| `edit <name>` | Opens the **Dockerfile** in `$EDITOR`. `--settings` edits the persona's `settings.json`, and `--spec` edits `persona.yaml`. |
| `delete <name>` | Removes the definition, Dockerfile, and image |
| `build <name>` | Rebuilds the image |
| `base ["requirement"]` | Shows or changes the **base requirements** that every persona gets, e.g. `persona base "All personas should have the Linear plugin from the default anthropic marketplace installed."`. Also takes the precise `--add-*/--remove-*` flags and `--clear`. |
| `edit-base` | Hand-edit `base.yaml`. `--settings` edits `base-settings.json`, the settings every persona starts from. |
| `spawn <persona>` | Starts a detached container (mounts the current directory at `/workspace`). Claude Code runs inside tmux. Flags: `-a` attach, `-p` initial prompt, `-w` workdir, `--no-mount`, `--skip-permissions`. |
| `join [ref]` | Attaches to a running session (detach: `Ctrl-b d`) |
| `stop <ref>… / --all` | Stops and removes containers (`--keep` leaves them stopped) |
| `control-center` | TUI multiplexer, described below |

## How a persona is built

`$PERSONA_HOME/personas/<name>/` holds three files:

- `persona.yaml`: the structured definition (plugins, MCP servers, skills, apt packages, forwarded env vars,
  instructions, and an optional `settings:` map of overrides).
- `settings.json`: the container's `~/.claude/settings.json`. It is deep-merged from these layers, lowest to highest:
  1. `base-settings.json`. The default is [`base-settings.json`](base-settings.json) in this repo, embedded into the
     binary at build time. Running `persona edit-base --settings` creates `$PERSONA_HOME/base-settings.json`, which
     then replaces the default.
  2. `base.yaml` (plugins, marketplaces, and model shared by all personas)
  3. the persona's own plugins, marketplaces, and model
  4. the persona's `settings:` overrides from `persona.yaml` (a `null` value deletes a key)

  Nested objects merge key by key. Arrays and scalars are replaced by the higher layer.
- `Dockerfile`: the image definition. It copies in `settings.json`, then **pre-installs every marketplace in
  `extraKnownMarketplaces` and every enabled plugin in `enabledPlugins`**. That makes settings.json the source of
  truth for plugins: a plugin added by hand-editing it is installed on the next build.

Both generated files are protected. Once you hand-edit one, `update` and base changes leave it alone (with a warning)
unless you pass `--force`. If you want overrides that survive regeneration, put them in `persona.yaml`'s `settings:` map.

The image installs Claude Code with the native installer, registers MCP servers at user scope, skips onboarding,
and pre-trusts `/workspace`. Secrets are never baked into the image: MCP configs reference `${VAR}`, and `spawn`
forwards those vars from your shell.

**Private marketplaces and skills:** at build time, persona passes a GitHub token to Docker as a BuildKit secret.
It takes the first one it finds: `$GITHUB_TOKEN`, `$GH_TOKEN`, then `gh auth token`. The token is visible only while
the clone steps run, and is never written into an image layer.

### Base requirements

`$PERSONA_HOME/base.yaml` is merged into every persona when its files are generated. If a persona's own entry has
the same name as a base entry, the persona's entry wins. Base instructions are appended to every CLAUDE.md, and the
base model applies to personas that don't set one. Changing the base regenerates every Dockerfile that hasn't been
hand-edited and offers to rebuild. Running containers keep their old image until you respawn them.

## Control center

The sidebar lists personas and their containers. Each pane mirrors a container's Claude Code tmux session.

- `enter`: spawn the selected persona or open the selected instance in the focused pane. `s` spawns into a new pane.
- `i`: INSERT mode, where your keys go to the focused pane. `ctrl+]` (or F12) returns to NORMAL mode.
- `n` / `x` / `X`: new pane / close pane / stop the pane's container. `tab` cycles panes, `o` rotates the grid.
- `t` / `w` / `[` `]` / `1-9`: tabs. `b`: toggle the sidebar (spring-animated). `a`: full-screen attach. `?`: help.

Panes talk to containers only through the `docker` CLI (`tmux capture-pane` / `send-keys`), so the TUI needs no PTY
layer and behaves the same on macOS, Linux, and Windows.

## Configuration

Output is styled when it goes to a terminal. Set `NO_COLOR=1` or pass `--no-color` to turn styling off (this applies
to the TUI and prompts too). Piped output is always plain.


`$PERSONA_HOME/config.yaml` (default: `<OS config dir>/persona`). Any key can also be set as an env var with the
`PERSONA_` prefix.

```yaml
model: claude-opus-5-5            # model used by the designer
marketplaces:                     # built-in marketplaces the designer always searches
  - anthropics/claude-plugins-official
  - anthropics/claude-code
  - anthropics/skills
base_image: debian:trixie-slim
forward_env: [ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN]
editor: "code --wait"
```
