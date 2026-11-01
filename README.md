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
- On Linux, KVM and the persona `.deb` for the default [microVM runtime](#microvm-runtime-linux),
  or `runtime: docker` to use containers instead
- An Anthropic API key for the persona designer (`persona new`, `update --redesign`, `base`): run `persona configure`
  once, or export `ANTHROPIC_API_KEY`.
- For instances, your Claude subscription: run `persona auth login` once (see [Credentials](#credentials)).
  Without it, instances use `ANTHROPIC_API_KEY` instead.
- Optional: an Exa API key (`persona configure`, or `EXA_API_KEY`) lets the designer search the web for niche
  third-party marketplaces and MCP servers.

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
| `configure` | Prompts for the Anthropic and Exa API keys the designer uses and stores them (mode 0600). Exported `ANTHROPIC_API_KEY` / `EXA_API_KEY` take precedence. |
| `auth login / status / logout` | Stores the Claude subscription token instances sign in with (see [Credentials](#credentials)) |
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

## Credentials

Instances sign in to Claude Code with your Claude subscription (Pro, Max, Team or Enterprise), not an API key:

```sh
persona auth login     # runs `claude setup-token`, then asks you to paste the token
persona auth status    # what new instances will get
persona auth logout    # delete the stored token
```

`claude setup-token` creates a one-year OAuth token for headless use. persona stores it in
`$PERSONA_HOME/credentials/claude-oauth-token.json` (mode 0600). Each instance receives it as
`CLAUDE_CODE_OAUTH_TOKEN`: in a microVM through a file on tmpfs, and in Docker forwarded by name, so the value
never appears on a command line. On another machine, pipe a token in instead with `persona auth login --stdin`.

- A `CLAUDE_CODE_OAUTH_TOKEN` exported in your shell takes precedence over the stored one.
- With a token, `ANTHROPIC_API_KEY` is not forwarded, because Claude Code would prefer it and bill the API.
  Pass `persona spawn -e ANTHROPIC_API_KEY` to forward it anyway.
- persona never copies `~/.claude` credentials into instances. Each one runs the unmodified Claude Code with that
  token, and all of them share your plan's usage limits.
- Running instances keep the credentials they started with. Respawn them after `login` or `logout`.

## Control center

The sidebar lists personas and their containers. Each pane mirrors a container's Claude Code tmux session.

- `enter`: spawn the selected persona or open the selected instance in the focused pane. `s` spawns into a new pane.
- `i`: INSERT mode, where your keys go to the focused pane. `ctrl+]` (or F12) returns to NORMAL mode.
- `n` / `x` / `X`: new pane / close pane / stop the pane's container. `tab` cycles panes, `o` rotates the grid.
- `t` / `w` / `[` `]` / `1-9`: tabs. `b`: toggle the sidebar (spring-animated). `a`: full-screen attach. `?`: help.

Panes talk to containers only through the `docker` CLI (`tmux capture-pane` / `send-keys`), so the TUI needs no PTY
layer and behaves the same on macOS, Linux, and Windows.

## MicroVM runtime (Linux)

On Linux, instances run as [Firecracker](https://firecracker-microvm.github.io/) microVMs by default, which needs KVM
and the host setup below. On macOS and Windows the default is Docker containers. To use containers on Linux, set
`runtime: docker` in `config.yaml` (or `PERSONA_RUNTIME=docker`). Every command works the same way on both
runtimes, and images are always built with Docker.

**Install** on Debian 13 (trixie) or later, x86_64, with one package:

```sh
make deb                                   # builds dist/persona_<version>_amd64.deb
sudo apt install ./dist/persona_*_amd64.deb
```

`make host-setup` does both. The package contains:

| Path | What |
|---|---|
| `/usr/bin/persona` | the CLI (and, copied into images, the guest agent) |
| `/usr/libexec/persona/firecracker`, `jailer` | Firecracker (version set by `FC_VERSION` at build time) |
| `/usr/libexec/persona/persona-vm` | the root wrapper: the only thing persona runs as root |
| `/usr/libexec/persona/persona-netns` | sets up the shared network namespace, run by `persona-netns.service` |
| `/usr/share/persona/vmlinux` | guest kernel (Firecracker CI build, or `KERNEL=/path` at build time) |
| `/etc/sudoers.d/persona-vm` | lets the `persona-vm` group run the wrapper, and nothing else |

Installing it creates the `persona-vm` group and adds the user who ran `sudo apt install` to it (and to `kvm`). It
also starts the network service. Docker is recommended rather than required, since it's only needed to build images.
`apt remove` stops all microVMs and the network service.

**Isolation.** Each VM runs under the jailer: in a chroot, as its own uid. All VMs share one bridge, so instances can
reach each other. Traffic leaving the bridge is NATed to the internet. The host itself, and every private, link-local
and CGNAT address range (your LAN), is unreachable from the VMs. DNS uses public resolvers for that reason.

**Workspaces.** Spawned inside a git repository, an instance gets its own branch, `persona/<instance>`, starting from
your current `HEAD`, plus a worktree for that branch under `$PERSONA_HOME/worktrees`. The VM receives a single-branch
clone of it, so none of your other branches and none of your `.git` reach the VM. Uncommitted changes in your checkout
are not copied. Outside a repository, the directory is copied in.

**Pushing without credentials.** VMs never get your SSH keys or GitHub tokens. Instead, Claude Code runs
`persona-agent push`, or `persona-agent pr --title … --body …`. A broker on the host, started on demand, receives the
branch as a git bundle, verifies it, and fast-forwards the worktree. It then pushes with your own `git` (and opens the
PR with your own `gh`). It only ever pushes the instance's own branch.

**Stopping** saves everything. Commits land on the branch, and uncommitted changes are copied into the worktree
(files ignored by `.gitignore` are skipped). Removing the instance deletes the worktree only if it is clean; the branch
is always kept. `persona stop --force` skips saving, for a VM that no longer responds.

Each VM's root filesystem is the image, flattened to a read-only disk that every instance of that persona shares. A
small, per-VM disk is layered over it with overlayfs. The guest's init is `persona-agent`: the persona binary itself,
copied into the image. Over vsock it handles identity, file delivery (secrets go to tmpfs only), file transfer and
terminals.

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
runtime: firecracker              # default on Linux; docker elsewhere (see MicroVM runtime)
vm_kernel: /usr/local/share/persona/vmlinux
vm_vcpus: 2
vm_memory_mib: 4096
vm_disk_gib: 32                   # each VM's writable layer (sparse)
```
