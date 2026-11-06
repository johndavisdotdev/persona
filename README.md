# persona

<video src="[https://github.com](https://github.com/johndavisdotdev/persona/raw/refs/heads/main/promo.mp4)" controls width="100%"></video>

Build, spawn, and manage **coding-agent personas**: minimal [Nix](https://nixos.org)-built images running a coding agent (Claude
Code by default, or the [pi](https://pi.dev) harness), with the packages, skills, MCP servers and language
toolchains a role needs already set up.

```
persona new                      # AI wizard: describe a persona → Nix flake → image
persona spawn reviewer -a        # start a spawn (a microVM on Linux) and attach to its agent
persona manage                   # dashboard: what every spawn is doing, and what it needs from you
persona control-center           # TUI: split panes + tabs across running spawns
```

## Requirements

- Docker (Docker Desktop, OrbStack, Colima, …)
- Optional: [Nix](https://nixos.org/download) with flakes. Without it, images are built with Nix in a
  `nixos/nix` container whose store is kept in the `persona-nix` Docker volume.
- On Linux, KVM and the persona `.deb` for the default [microVM runtime](#microvm-runtime-linux),
  or `runtime: docker` to use containers instead
- An Anthropic API key for the persona designer (`persona new`, `update --redesign`, `base`): run `persona configure`
  once, or export `ANTHROPIC_API_KEY`.
- For spawns, credentials for their harness (see [Credentials](#credentials)): your Claude subscription for Claude Code
  (`persona auth login`), or an OpenRouter API key for pi (`persona configure`, or `OPENROUTER_API_KEY`).
- Optional: an Exa API key (`persona configure`, or `EXA_API_KEY`) lets the designer search the web for niche
  third-party marketplaces and MCP servers.

## Commands

**Personas** are definitions plus an image:

| Command | What it does |
|---|---|
| `new [name]` | Claude designs the persona from your description, with three agents working in parallel. **runtimes** picks the language toolchains, looking up primary-source downloads with Exa. **extensions** picks skills, MCP servers and plugins, first- or third-party, from the built-in marketplaces (`anthropics/claude-plugins-official`, `anthropics/claude-code`, `anthropics/skills`) and the web. **tooling** sets up the harness: its packages, LSP, linters, formatters and CLIs. Then one call writes the summary and instructions. Every language the description mentions is installed. The image definition is generated and built without further questions; `--ask` lets you review the selection and confirm the build first. `--from-spec file.yaml` imports a definition without AI. |
| `list` (`--json`) | Personas, with build state and running spawns |
| `edit <name>` | Opens **flake.nix** in `$EDITOR`. `--file setup-root.sh` / `setup-user.sh` edits a setup script, `--settings` the persona's `settings.json`, and `--spec` `persona.yaml`. |
| `customize <name>` | Opens a shell in the persona's image (`--root` for root, `--run CMD` for one command), shows what changed when you exit, and commits it as the persona's image. For interactive configuration and software Nix can't provide. The persona is frozen afterwards. |
| `update <name>` | `--description`/`--redesign` asks Claude to revise the persona. `--add-plugin`, `--add-toolchain`, `--remove-mcp`, `--model`, `--add-env`, `--add-package`, … make precise edits. Rebuilds afterwards. |
| `delete <name>` | Removes the definition and images. Its spawns are not affected. |
| `build <name>` | Rebuilds the image. `--refresh` first updates `flake.lock` to the newest nixpkgs. |
| `base ["requirement"]` | Shows or changes the **base requirements** that every persona gets, e.g. `persona base "All personas should have the Linear plugin from the default anthropic marketplace installed."`. Also takes the precise `--add-*/--remove-*` flags and `--clear`. |
| `edit-base` | Hand-edit `base.yaml`. `--settings` edits `base-settings.json`, the settings every persona starts from. |

**Spawns** are running instances of a persona: microVMs on Linux, containers elsewhere. Commands take the spawn's
name (random unless `--name` is given, e.g. `keen-yak-89`):

| Command | What it does |
|---|---|
| `spawn <persona>` | Starts a new spawn with Claude Code in tmux (see [Workspaces](#microvm-runtime-linux) for how your directory gets in). Flags: `-a` attach, `-p` initial prompt, `-w` workdir, `--no-mount`, `--skip-permissions`, `--name`, `--namespace`. |
| `ps` (`-a`, `--json`) | Running spawns, one table per [namespace](#namespaces); `-a` includes stopped ones, `--namespace` shows one namespace |
| `exec [-i] <spawn> <command> [args…]` | Runs a command in a running spawn, outside Claude Code: as the session user, in its working directory. Prints the command's stdout and stderr and exits with its status (127 not found, 126 not runnable, 128+N on signal N). `-i` passes your stdin. |
| `attach [spawn]` | Attaches to a spawn's Claude Code session (detach: `Ctrl-b d`). Alias: `join`. |
| `stop <spawn>… / --all` | Stops spawns but keeps them. A microVM spawn saves its work to its branch and worktree first, and keeps its disk. |
| `start <spawn>…` | Resumes stopped spawns with their files intact and a fresh Claude Code session (`-a` to attach) |
| `save <spawn> <new-persona>` | Saves a running spawn's state (everything but `/workspace`) as a new persona; the spawn keeps running. Saved personas can't be changed with `update`, `edit` or `build`. Change one with `customize`, or by spawning it and saving again. |
| `kill <spawn>… / --all` | Stops and removes spawns. A microVM spawn saves its work to its branch first. |
| `manage` | Dashboard driven by every spawn's status stream, described [below](#persona-manage) |
| `control-center` | TUI multiplexer, described below |
| `ns create <name>` / `ns remove <name>` / `ns list [name]` | Create, remove, and list [namespaces](#namespaces) with their spawns |
| `fw allow-from <host>:<port>[:udp] --to <spawn>:<port>` | Forwards a host port, or with `cgnat` as the host an address other spawns reach, to a spawn's port ([port forwards](#port-forwards)). `fw list-rules [spawn]` (or `--namespace`) lists them, `fw rm-rule <id>` removes one. |
| `message <text>` | Sends a message into running spawns' Claude Code sessions: all of them, or `--name`, `--persona`, `--namespace` |

**More**: `configure` stores API keys (mode 0600): Anthropic and Exa for the designer, OpenRouter for pi spawns.
Exported `ANTHROPIC_API_KEY` / `EXA_API_KEY` / `OPENROUTER_API_KEY` take precedence. `auth login / status / logout`
stores the Claude subscription token Claude Code spawns sign in with (see [Credentials](#credentials)).

## How a persona is built

An image is built in two stages:

1. **Nix** builds `flake.nix` into `persona/<name>:base` with `dockerTools.buildLayeredImage`: every system package
   and language toolchain, from [nixpkgs](https://search.nixos.org/packages). It uses the host's Nix when there is
   one, otherwise a `nixos/nix` container (config `nix_builder: auto | host | container`). `flake.lock` pins the
   nixpkgs revision; `persona build --refresh` moves it to the newest.
2. **Setup** runs `setup-root.sh` (as root) and `setup-user.sh` (as `claude`) in a container from that image, for
   what Nix can't express: the harness, its plugins and settings, npm/uv/go/cargo/dotnet tools, rustup's toolchain
   and Playwright's browsers. The container is committed as `persona/<name>:latest`, the image spawns use (and the
   one the microVM disk is made from).

If the first build in `persona new` fails and an Anthropic API key is configured, a Claude agent takes over. It
reads the build log, checks nixpkgs attribute names against the persona's locked nixpkgs (with host Nix), edits
`flake.nix` and rebuilds, at most 5 times in one conversation. It gives up early on failures `flake.nix` can't fix,
such as Docker or the network being down. If it doesn't succeed, the generated `flake.nix` is restored and its last
attempt is kept as `flake.nix.attempt`. Pass `--no-fix` to skip this.

To change an image by hand after that (configure something interactively, install a package Nix lacks), run
`persona customize <name>`. It commits your changes as the new image. A customized persona is **frozen**:
rebuilding from `flake.nix` would lose the hand changes, so `update`, `edit` and `build` refuse it from then on.

`$PERSONA_HOME/personas/<name>/` holds:

- `persona.yaml`: the structured definition (plugins, MCP servers, skills, language toolchains, nixpkgs
  `packages`, forwarded env vars, instructions, and an optional `settings:` map of overrides). `apt_packages` from
  older definitions are mapped to nixpkgs names (`build-essential` → `gcc` + `gnumake`, `sqlite3` → `sqlite`, …).
- `flake.nix`: the Nix base image. Each toolchain resolves at build time to the newest stable package nixpkgs has
  (nixpkgs' default `dotnet-sdk`, `go` and `nodejs` often lag; `dotnet-sdk_10` beats `dotnet-sdk` 8). A package
  nixpkgs doesn't have is skipped with a warning naming it, instead of failing the build. Prebuilt Linux binaries
  run too: the glibc loader is linked at `/lib64`, with common libraries in `/lib`.
- `setup-root.sh`, `setup-user.sh`: the setup stage. Files they install are in `files/`.
- `settings.json`: Claude Code's `~/.claude/settings.json`. It is deep-merged from these layers, lowest to highest:
  1. `base-settings.json`. The default is [`base-settings.json`](base-settings.json) in this repo, embedded into the
     binary at build time. Running `persona edit-base --settings` creates `$PERSONA_HOME/base-settings.json`, which
     then replaces the default.
  2. `base.yaml` (plugins, marketplaces, and model shared by all personas)
  3. the persona's own plugins, marketplaces, and model
  4. the persona's `settings:` overrides from `persona.yaml` (a `null` value deletes a key)

  Nested objects merge key by key. Arrays and scalars are replaced by the higher layer. Setup **pre-installs every
  marketplace in `extraKnownMarketplaces` and every enabled plugin in `enabledPlugins`**, so a plugin added by
  hand-editing settings.json is installed on the next build.

The generated files are protected. Once you hand-edit one, `update` and base changes leave it alone (with a warning)
unless you pass `--force`. If you want overrides that survive regeneration, put them in `persona.yaml`'s `settings:` map.

Every image gets the **beacon** and **persona** plugins from the local `persona` marketplace (published from
[`beacon/`](beacon), installed at `/opt/persona/marketplace`; config `beacon_marketplace`). `persona@persona` holds
skills for working inside a spawn. So far that's one, `persona:git`
([SKILL.md](beacon/Plugin/plugins/persona/skills/git/SKILL.md)), on the spawn's git workspace and
`persona-agent`. pi personas get its skills copied into `~/.pi/agent/skills`. Claude Code personas also get
[`managed-settings.json`](managed-settings.json) at `/etc/claude-code/managed-settings.json` (config
`claude_managed_settings`), which approve beacon as a channel, and every session starts with
`--channels plugin:beacon@persona`. pi can't load Claude Code plugins, so pi personas get the beacon's MCP server
registered directly.

The beacons find each other by UDP multicast on the bridge the microVMs of a namespace share, using the messages in
[`protos/shapes.proto`](protos/shapes.proto) (Go types in `shapes/`, regenerated with `go generate ./shapes`).
`persona message <text>` delivers a message into running spawns' Claude Code sessions through their beacons. With no
filter it goes to every running spawn (one multicast per namespace). `--name` (one or more spawns) and `--persona`
(every spawn of a persona) send it only to the matching spawns, by unicast, and `--namespace` narrows any of these to
one namespace. `persona message --discover` lists the agents that answer a discovery request. The CLI can't enter a
network namespace itself, so `persona-vm mcast-socket` creates a UDP socket on that namespace's bridge as root and
passes it back over the CLI's stdin.

Claude Code is installed with the native installer. Setup registers MCP servers at user scope, skips onboarding,
and pre-trusts `/workspace`. Secrets are never baked into the image: MCP configs reference `${VAR}`, and `spawn`
forwards those vars from your shell.

**Private marketplaces and skills:** setup gets a GitHub token as `GITHUB_TOKEN`, passed by name to that one run.
It takes the first one it finds: `$GITHUB_TOKEN`, `$GH_TOKEN`, then `gh auth token`. It is never written into the
image.

### Language toolchains

The designer works out which languages a persona needs from its description; you don't list them. It reads "a .NET
expert" as the .NET SDK, and "maintains our React storefront" as Node.js plus the web testing suite. Toolchains come
from nixpkgs, at the newest stable version it has:

| Toolchain | nixpkgs | Version unless the description names one |
|---|---|---|
| `dotnet` | `dotnet-sdk_N` | newest GA SDK (`lts`: newest even major) |
| `go` | `go_1_N` | newest |
| `rust` | `rustup` (minimal profile), with clippy and rustfmt | stable (a version is a rustup toolchain) |
| `node` | `nodejs_N` | newest LTS (`latest` for current) |
| `java` | `jdkN` | newest LTS |
| `bun`, `deno` | `bun`, `deno` | nixpkgs' |
| `python` | `pythonXY` with pip, plus `uv` | nixpkgs' `python3` |
| `playwright` | Chromium's libraries; then `@playwright/cli` with Chromium's headless shell, the Playwright CLI skills, and the `playwright` plugin; brings `node` | latest |

The designer gives every web-oriented persona the `playwright` toolchain, so it can check its own UI work in a real
browser.

Any other runtime (Zig, Elixir, Haskell, …) names its [nixpkgs](https://search.nixos.org/packages) package. Only for a
runtime nixpkgs lacks does the designer (with Exa configured) look up its official install instructions and write
install commands from them, citing the source page. `persona new` and `update` show any such commands before
building, and ask for confirmation when run in a terminal: they come from a web page and run as root during setup.

`persona update <name> --add-toolchain dotnet@8` and `--remove-toolchain go` edit the list directly.

**Developer tools** (`tools:` in `persona.yaml`) are linters, formatters, language servers and CLIs. Each is installed
from nixpkgs (`nix`) or with a package manager: `npm`, `uv`, `go`, `cargo`, `rustup` or `dotnet`, e.g.
`{name: gopls, via: go, package: golang.org/x/tools/gopls}` or `{name: shellcheck, via: nix, package: shellcheck}`. Each tool brings in its toolchain, and a failed install
only warns.

### Harness

The harness is the coding agent a persona's spawns run. It's the designer's choice, guided by the description:

- **claude** (default): Claude Code. Claude Code plugins only work here.
- **pi**: the [pi coding agent](https://pi.dev) (`@earendil-works/pi-coding-agent`), reaching models through
  OpenRouter. Every pi persona gets these pi packages unless the base requirements change the list:
  `pi-markdown-preview`, `@companion-ai/feynman`, `@dietrichgebert/ponytail`, `bigpowers`, `projectops`,
  `@agimon-ai/po-skill`, `@agimon-ai/po`, `@agimon-ai/po-domain`, `pi-code`, `pi-docparser`. A failing package
  install warns rather than failing the build. Instructions go to `~/.pi/agent/AGENTS.md`, and skills to
  `~/.pi/agent/skills`. `pi-code` makes pi read Claude Code's configuration, so MCP servers apply too. Used only
  when the description asks for pi; its packages add about 800 MB to each persona.

Set it explicitly with `harness:` in `persona.yaml`, `persona update <name> --harness pi`, or for every persona
with `persona base --harness pi`. Add or drop pi packages with `--add-harness-package npm:<name>` /
`--remove-harness-package npm:<name>`; removing a default from one persona leaves the others alone.

### Base requirements

`$PERSONA_HOME/base.yaml` is merged into every persona when its files are generated. If a persona's own entry has
the same name as a base entry, the persona's entry wins. Base instructions are appended to every CLAUDE.md, and the
base model applies to personas that don't set one. Changing the base regenerates every persona's files that haven't been
hand-edited and offers to rebuild. Running containers keep their old image until you respawn them.

## Credentials

**pi spawns** reach models through OpenRouter: store a key with `persona configure` (or export `OPENROUTER_API_KEY`).
It's forwarded to every spawn by name. Pi spawns never get the Claude subscription token: Anthropic only allows
subscription credentials in its own apps.

**Claude Code spawns** sign in with your Claude subscription (Pro, Max, Team or Enterprise), not an API key:

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

## persona manage

`persona manage` is a dashboard of every spawn, driven by the status stream each agent produces rather than by
mirroring terminals. Each spawn shows as working, idle, needs input, failed or stopped, with its latest activity and
recent history.

- Questions the agent asks you (with their options) and permission prompts are highlighted, and ring the terminal
  bell. `enter` types a reply into the spawn's session; for a multiple-choice question, reply with the option's
  number. `a` attaches to the session in full.
- **The status stream.** Every image includes `persona-status`, fed by the harness's hooks.
  - Claude Code reports `SessionStart`, `UserPromptSubmit`, `Stop`, `StopFailure`, `Notification`, `TaskCompleted`,
    and questions asked through `AskUserQuestion`. These run as asynchronous hooks, added alongside any of your own.
  - pi reports session start, prompts, turn ends and UI prompts through an extension. pi has no notification or
    task events.
- Events from microVMs travel through `persona-agent` and the broker over vsock. Docker spawns write to a mounted
  directory. Either way they land in `$PERSONA_HOME/status/<spawn>/events.jsonl`.

## Web UI (`persona server`)

```sh
persona server                                  # http://127.0.0.1:7443/#token=…
persona server --web web/dist                   # serve a fresh build from disk instead of the embedded one
persona server --allow-origin http://172.16.1.100:5173   # let the Vite dev server's page connect
```

The web app lives in [`web/`](web) (React, Vite, JointJS, Golden Layout, xterm.js):

```sh
make web          # builds web/ into internal/server/dist, which the persona binary embeds
make build        # (or make deb / host-setup) to rebuild persona with it
```

Its home page is a Golden Layout workbench. You can drag any tab to split the screen, stack it with others, or dock
it anywhere:
- **Network** is a JointJS diagram: each namespace is a box with its spawns inside, the host is a box along the
  bottom, and port forwards are links (host → spawn, or spawn → spawn for CGNAT forwards). It redraws from the state
  the server pushes, so new and removed spawns, namespaces and rules show up on their own.
- **The persona menu**, docked over the diagram's left edge, lists the personas. Drag one onto a namespace, confirm
  the prefilled spawn name, and it is spawned there, with its output in a new tab.
- **Spawns:** click one for a terminal on its Claude Code session in a new tab. Right-click a spawn or a namespace for
  stop/start/kill, exposing a port, messages and removing a namespace.
- **Connecting nodes:** running spawns and the host have a handle. Drag from one handle onto a spawn in another
  namespace, or between a spawn and the host, then give the port and protocol (and, with the host, the address to
  listen on). That creates the forward: a CGNAT one onto the spawn you dropped on, or one on the host. Its line is
  labelled with the address the other side uses: `<spawn>.<namespace>.persona:<port>` for spawns, `localhost:<port>`
  or `<host-ip>:<port>` for the host.
- **Port forwards** lists, adds and removes rules.
- **Notifications:** when an agent calls the beacon's `report_work_finished` or `request_input`, the page shows a
  browser notification (the question and its choices, for `request_input`). Clicking it opens that spawn's terminal.
  The page asks for permission on your first click; the bell in the corner shows the permission and asks again.

For `vite dev`, point the app at a running server with `VITE_PERSONA_SERVER=http://host:7443`. Start the server with
`--allow-origin` set to the dev server's origin. WebTransport only works from a secure context, so open the dev
server as `localhost` or over HTTPS.

Without `--web`, the server serves the build embedded in the binary.

The server is an HTTP/3 server whose page talks to it over WebTransport. The page shows:
- every spawn, drawn inside its namespace;
- personas, which you can create, build and spawn, with the CLI's output live in a terminal;
- namespaces and port forwards, which you can manage;
- a terminal (xterm.js) on any spawn's Claude Code session.

The same port serves the page over TCP and HTTP/3 over UDP. It listens on `127.0.0.1` by default. Every session needs
the token from `$PERSONA_HOME/server/token`, which the printed URL carries. `--new-token` replaces it.

TLS is your certificate if you pass `--cert` and `--key`. Otherwise it's a self-signed ECDSA certificate that is
renewed every 13 days and trusted by hash (`serverCertificateHashes`). On loopback, the page itself is plain HTTP,
which browsers treat as secure.

**Protocol** (for a client such as the app in [`web/`](web)):

1. `GET /config.json` returns `{"url": "https://host:port/wt", "certHash": "<base64 sha-256>"}`. `certHash` is only
   present with the self-signed certificate.
2. `new WebTransport(url + "?token=" + token, {serverCertificateHashes: [{algorithm: "sha-256", value: <certHash
   bytes>}]})`.
3. Every bidirectional stream starts with one JSON line saying what it is:
   - `{"type":"control"}` carries newline-delimited JSON (state events, plus `{"event":"notice","data":{spawn,
     persona, namespace, kind: "finished"|"input", question, options, ts}}` when an agent reports through its
     beacon). Requests are `{"id", "op", "args"}`, answered by
     `{"id", "ok", "result"|"error"}`. The server pushes `{"event":"state","data":{runtime, namespaces, spawns,
     personas, forwards, features, errors}}` on connect and whenever the state changes. The ops are:
     - `ns.create {name}`, `ns.remove {name}`;
     - `fw.add {from: "<localhost|ip|cgnat>:<port>[:udp]", spawn, port}`, `fw.remove {id}`;
     - `spawn.stop|spawn.start|spawn.kill {spawn}`;
     - `message {text, names?, persona?, namespace?}`.
   - `{"type":"attach","spawn","cols","rows"}` is a terminal on the spawn's session.
   - `{"type":"job","kind":"new|build|spawn","args":{…},"cols","rows"}` runs `persona new` (`name`,
     `description`), `build` (`name`) or `spawn` (`persona`, `namespace`, `name`, `prompt`, `workdir`) and streams its
     output.

   After that header, terminal and job streams carry frames in both directions. Each frame is a type byte, a
   big-endian `uint32` length, then the payload. Type `0` is terminal bytes, type `1` is a resize sent by the client
   (`{"cols","rows"}`), and type `2` is the exit status sent by the server (`{"code","error"}`), after which the stream
   ends. Closing an attach stream detaches; the spawn's session keeps running.

## Control center

The sidebar lists running spawns under their persona, under a heading per [namespace](#namespaces). Namespaces and
personas without running spawns are left out. Selecting a persona row spawns another of it in that namespace.
`--namespace NAME` shows, and spawns into, only that namespace. Each pane mirrors a
spawn's Claude Code tmux session.

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
| `/usr/libexec/persona/persona-netns` | sets up the network namespaces, run by `persona-netns.service` and `persona-vm` |
| `/usr/share/persona/vmlinux` | guest kernel (Firecracker CI build, or `KERNEL=/path` at build time) |
| `/etc/sudoers.d/persona-vm` | lets the `persona-vm` group run the wrapper, and nothing else |

Installing it creates the `persona-vm` group and adds the user who ran `sudo apt install` to it (and to `kvm`). It
also starts the network service. Docker is recommended rather than required, since it's only needed to build images.
`apt remove` stops all microVMs and the network service.

**Isolation.** Each VM runs under the jailer: in a chroot, as its own uid. VMs in the same [namespace](#namespaces)
share one bridge, so they can reach each other. Traffic leaving the bridge is NATed to the internet. The host itself,
and every private, link-local and CGNAT address range (your LAN), is unreachable from the VMs.

**DNS.** VMs resolve through `persona-dns` (`persona-vm dns`, run by `persona-dns.service`), one server for every
namespace. It listens on the bridge address `10.200.0.1` inside each namespace, so it knows which namespace a query came
from. It answers a spawn's name (`jill`, `jill.persona`, or its VM id) with its address only for VMs in the same
namespace; names under `.persona` that don't match get NXDOMAIN. Everything else is forwarded to public resolvers,
since VMs can't reach your LAN's DNS. VMs list `1.1.1.1` second, for the moment the service is down.

### Namespaces

A namespace is a separate network for spawns. Spawns in one reach each other and hear each other's beacon
multicast; they can't reach anything in another namespace. The `default` namespace always exists and can't be
removed.

```sh
persona ns create lab                      # a new namespace
persona spawn reviewer --namespace lab     # spawn into it (default: default)
persona ns list lab                        # its spawns, running or stopped
persona ns remove lab                      # fails while any spawn is in it
```

Each namespace is a Linux network namespace (`persona` for `default`, `persona-<name>` for the others) with the same
layout inside: bridge `fcbr0` at `10.200.0.1/16`. So a VM's network config doesn't depend on its namespace, and VM
addresses stay unique across all of them. Traffic is NATed inside the namespace to its end of a veth pair to the host
(`10.199.N.2`), then out of the uplink by the host, with the same blocked ranges for every namespace. The extra
namespaces are listed in `/var/lib/persona/namespaces`, and `persona-netns.service` recreates them at boot. Creating
and removing them goes through `persona-vm ns-create` / `ns-remove`. Namespaces need the firecracker runtime; Docker
spawns are all in `default`.

### Port forwards

`persona fw` exposes a spawn's port on the host, on `localhost` or on one of the host's own IPv4 addresses:

```sh
persona fw allow-from localhost:8080 --to jill:8080          # tcp unless :udp is given
persona fw allow-from 172.16.1.100:80:tcp --to jill:8080     # reachable from your LAN
persona fw list-rules jill                                   # or --namespace lab, or all of yours
persona fw rm-rule 2                                         # by the id allow-from printed
```

The spawn must be running when the rule is made. Rules last until removed or until the spawn is killed; stopping
keeps them. Every namespace has the same subnet, so the host can't route to a VM directly. The host DNATs the address
to the namespace's veth address on port `20000+id`, and the namespace DNATs that to the VM. Only those packets are
let past the rules that keep VMs off your LAN. `localhost` rules also SNAT to the host's veth address, with
`route_localnet` on that veth. Rules belong to whoever made them and can only point at their own VMs.

**To other spawns, across namespaces.** `cgnat` as the host exposes the port to every spawn, in any namespace, on
the spawn's own address in `100.64.0.0/10` (carrier-grade NAT space). A spawn at `10.200.a.b` is `100.64.a.b`, and
DNS answers `<spawn>.<namespace>.persona` with it from every namespace:

```sh
persona fw allow-from cgnat:80 --to jill:8080   # other spawns: http://jill.default.persona/ (100.64.a.b:80)
```

These rules only match traffic coming from the namespaces, never the host's own, so they can't collide with
anything on the host that uses CGNAT addresses, such as Tailscale. Every other `100.64.0.0/10` address stays
blocked. Traffic is SNATed to the host's veth address into the target namespace, so spawns in the same namespace
can use the address too. They are kept in
`/var/lib/persona/forwards` and applied by `persona-netns ports`, so they come back after a reboot. IPv4 only.

**Workspaces.** Spawned inside a git repository, an instance gets its own branch, `persona/<instance>`, starting from
your current `HEAD`, plus a worktree for that branch under `$PERSONA_HOME/worktrees`. The VM receives a single-branch
clone of it, so none of your other branches and none of your `.git` reach the VM. Uncommitted changes in your checkout
are not copied. Outside a repository nothing is copied (it could be your whole home directory): `/workspace`
starts empty.

**Pushing without credentials.** VMs never get your SSH keys or GitHub tokens. Instead, Claude Code runs
`persona-agent push`, or `persona-agent pr --title … --body …`. A broker on the host, started on demand, receives the
branch as a git bundle, verifies it, and fast-forwards the worktree. It then pushes with your own `git` (and opens the
PR with your own `gh`). It only ever pushes the instance's own branch. After a rebase, `push --force` (or
`pr --force`) takes the spawn's history instead. The worktree is reset to it, refusing if that would overwrite
uncommitted changes there, and the push uses `--force-with-lease`. The broker restarts itself when an upgrade replaces
the `persona` binary.

**Fetching from the host.** The spawn's clone starts with the PR base as `host/<base>` (usually `host/main`).
`persona-agent fetch [branch…]` brings in more, or refreshes them: host branches (`main`) or the host's
remote-tracking branches (`origin/main`), as `host/<branch>`. The broker pushes them into the spawn's repository over
git's own protocol through `persona __vm-rsh`, so only missing objects travel, and the spawn sees only the branches
it asks for. Then `git rebase host/main` works as usual.

**Stopping** saves everything. Commits land on the branch (a rebased branch replaces the worktree's history, which
stays in its reflog), and uncommitted changes are copied into the worktree
(files ignored by `.gitignore` are skipped). Removing the instance deletes the worktree only if it is clean; the branch
is always kept. A new spawn with the same name picks them up again: it reuses the branch's worktree if it is still
there, or checks the branch out into a new one, so it continues from the branch rather than your current `HEAD`. `persona stop --force` skips saving, for a VM that no longer responds.

Each VM's root filesystem is the image, flattened to a read-only disk that every instance of that persona shares. A
small, per-VM disk is layered over it with overlayfs.

**The Nix store is shared by every persona.** An image's `/nix/store` is left out of its root disk and goes into
`$PERSONA_HOME/vm/store` as read-only *layers*: each build adds one layer holding only the store paths no layer has
yet, so .NET, Node, glibc and the rest are on disk once however many personas use them. The guest mounts its image's
layers (at most 12) together on `/nix/store`. A persona's own disk is then just its home directory and setup files
(the harness, npm tools, browsers): typically a few hundred MB. Layers no persona uses are deleted when personas
are rebuilt or deleted; spawns keep theirs through hard links. An image whose `/nix/store` was changed with
`persona customize` keeps its own copy instead. The guest's init is `persona-agent`: the persona binary itself,
copied into the image (it must be statically linked: `make build` and the `.deb` build it with
`CGO_ENABLED=0`). Over vsock it handles identity, file delivery (secrets go to tmpfs only), file transfer and
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
nix_builder: auto                 # host Nix if present, else a container (host | container)
nixpkgs: nixos-unstable           # branch or revision new flakes follow
nix_image: nixos/nix:latest       # the container builder
beacon_marketplace: /usr/share/persona/marketplace
claude_managed_settings: /usr/share/persona/managed-settings.json
forward_env: [ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN]
editor: "code --wait"
runtime: firecracker              # default on Linux; docker elsewhere (see MicroVM runtime)
vm_kernel: /usr/local/share/persona/vmlinux
vm_vcpus: 2
vm_memory_mib: 4096
vm_disk_gib: 32                   # each VM's writable layer (sparse)
```
