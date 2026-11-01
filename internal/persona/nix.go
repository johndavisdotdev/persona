package persona

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

// A persona's image starts as a Nix flake: dockerTools.buildLayeredImage with
// every package from nixpkgs. Setup that Nix can't express (the harness,
// plugins, npm/uv/go/cargo tools, browsers) runs afterwards in a container
// (see setup.go), which is then committed.

// NixpkgsDefault is the nixpkgs branch flakes follow unless configured
// (config key nixpkgs); each persona's flake.lock pins the revision.
const NixpkgsDefault = "nixos-unstable"

// basePackages are in every image: a shell and the tools persona, the
// harnesses and the status stream rely on.
var basePackages = []string{
	"bashInteractive", "coreutils", "findutils", "gnugrep", "gnused", "gawk", "gnutar", "gzip", "xz", "bzip2",
	"unzip", "which", "procps", "diffutils", "patch", "file", "less", "curl", "cacert", "gitMinimal", "openssh", "jq",
	"ripgrep", "rsync", "tmux", "ncurses", "gnumake", "iana-etc", "tzdata", "iproute2", "util-linux",
}

// fhsLibs let prebuilt (non-Nix) Linux binaries run: the glibc loader is
// linked at /lib64, and these libraries appear in /lib.
var fhsLibs = []string{"glibc", "stdenv.cc.cc.lib", "zlib", "openssl", "icu"}

// browserLibs are what Playwright's downloaded Chromium headless shell
// needs; it renders with its bundled SwiftShader, so not mesa (whose LLVM is
// half a gigabyte), only libgbm. "a|b" tries
// a, then b (nixpkgs renamed the xorg.* libraries).
var browserLibs = []string{
	"nss", "nspr", "atk", "at-spi2-atk", "at-spi2-core", "cups", "libdrm", "dbus", "expat", "libxkbcommon",
	"pango", "cairo", "alsa-lib", "glib", "fontconfig", "freetype", "liberation_ttf", "udev|systemd",
	"libx11|xorg.libX11", "libxcomposite|xorg.libXcomposite", "libxdamage|xorg.libXdamage", "libxext|xorg.libXext",
	"libxfixes|xorg.libXfixes", "libxrandr|xorg.libXrandr", "libxcb|xorg.libxcb", "libxi|xorg.libXi",
	"libxtst|xorg.libXtst", "libxcursor|xorg.libXcursor", "libxshmfence|xorg.libxshmfence", "libgbm",
}

// aptToNix maps the Debian package names older specs used to nixpkgs
// attributes ("" drops a package Nix doesn't need). Others keep their name.
var aptToNix = map[string][]string{
	"build-essential": {"gcc", "gnumake"}, "postgresql-client": {"postgresql"}, "sqlite3": {"sqlite"},
	"python3-pip": {"python3Packages.pip"}, "python3-venv": nil, "pkg-config": {"pkg-config"},
	// -dev packages: headers and pkg-config files (see CPATH in imageEnv).
	"libssl-dev": {"openssl.dev"}, "libncurses-dev": {"ncurses.dev"}, "libncurses5-dev": {"ncurses.dev"},
	"zlib1g-dev": {"zlib.dev"}, "libreadline-dev": {"readline.dev"}, "libffi-dev": {"libffi.dev"},
	"libyaml-dev": {"libyaml.dev"}, "libsqlite3-dev": {"sqlite.dev"}, "libpq-dev": {"libpq.dev|postgresql.dev"},
	"libxml2-dev": {"libxml2.dev"}, "libcurl4-openssl-dev": {"curl.dev"}, "libbz2-dev": {"bzip2.dev"},
	"liblzma-dev": {"xz.dev"}, "libgmp-dev": {"gmp.dev"}, "libsodium-dev": {"libsodium.dev"},
	"git": nil, "tshark": {"wireshark-cli"}, "iproute2": nil,
	"xz-utils": {"xz"}, "libicu-dev": {"icu"}, "libatomic1": nil, "ca-certificates": {"cacert"}, "openssh-client": {"openssh"},
	"default-mysql-client": {"mariadb.client"}, "mysql-client": {"mariadb.client"}, "redis-tools": {"redis"},
	"netcat-openbsd": {"netcat-openbsd"}, "dnsutils": {"dnsutils"}, "iputils-ping": {"iputils"}, "awscli": {"awscli2"},
	"golang": nil, "nodejs": nil, "npm": nil, "python3": nil, // language runtimes come from toolchains
}

// AptToNix converts a Debian package name to nixpkgs attributes.
func AptToNix(name string) []string {
	if v, ok := aptToNix[name]; ok {
		return v
	}
	return []string{name}
}

var nixAttrPathRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.+-]{0,127}$`)

// ValidateNixPackage checks a nixpkgs attribute path such as "jq" or
// "python3Packages.ruff".
func ValidateNixPackage(s string) error {
	if !nixAttrPathRe.MatchString(s) {
		return fmt.Errorf("invalid nixpkgs package %q", s)
	}
	return nil
}

// candidates lists attribute names from hi down to lo, e.g. dotnet-sdk_20 ..
// dotnet-sdk_8; step 2 or 4 skips non-LTS releases.
func candidates(format string, hi, lo, step int) []string {
	var out []string
	for v := hi; v >= lo; v -= step {
		out = append(out, fmt.Sprintf(format, v))
	}
	return out
}

func nixList(names []string) string {
	var q []string
	for _, n := range names {
		q = append(q, fmt.Sprintf("%q", n))
	}
	return "[ " + strings.Join(q, " ") + " ]"
}

// toolchainNix is the Nix expression (a list of packages) for a toolchain.
// "Latest" picks the newest versioned attribute that evaluates, because
// nixpkgs' default attributes (dotnet-sdk, go, jdk, nodejs) often lag.
func toolchainNix(t Toolchain) string {
	v := strings.TrimPrefix(t.Version, "v")
	under := strings.ReplaceAll(v, ".", "_")
	switch t.Name {
	case "dotnet":
		switch v {
		case "", "latest":
			return fmt.Sprintf(`[ (newest "dotnet" stable %s) ]`, nixList(candidates("dotnet-sdk_%d", 20, 8, 1)))
		case "lts":
			return fmt.Sprintf(`[ (newest "dotnet" stable %s) ]`, nixList(candidates("dotnet-sdk_%d", 20, 8, 2)))
		}
		return fmt.Sprintf(`[ (attr "dotnet-sdk_%s") ]`, strings.Split(v, ".")[0])
	case "go":
		if v == "" || v == "latest" {
			return fmt.Sprintf(`[ (newest "go" any %s) ]`, nixList(append(candidates("go_1_%d", 40, 20, 1), "go")))
		}
		return fmt.Sprintf(`[ (attr "go_%s") ]`, strings.Join(strings.Split(under, "_")[:min(2, len(strings.Split(under, "_")))], "_"))
	case "rust":
		return `[ (attr "rustup") ]` // the toolchain itself: rustup default, in setup
	case "node":
		switch v {
		case "", "lts":
			return fmt.Sprintf(`[ (newest "node" nodeLTS %s) ]`, nixList(append(candidates("nodejs_%d", 40, 18, 2), "nodejs")))
		case "latest", "current":
			return `[ (attr "nodejs_latest") ]`
		}
		return fmt.Sprintf(`[ (attr "nodejs_%s") ]`, strings.Split(v, ".")[0])
	case "java":
		switch v {
		case "", "lts":
			return fmt.Sprintf(`[ (newest "java" any %s) ]`, nixList(append(candidates("jdk%d", 41, 17, 4), "jdk")))
		case "latest":
			return fmt.Sprintf(`[ (newest "java" any %s) ]`, nixList(append(candidates("jdk%d", 44, 17, 1), "jdk")))
		}
		return fmt.Sprintf(`[ (attr "jdk%s") ]`, strings.Split(v, ".")[0])
	case "python":
		py := `(attr "python3")`
		if v != "" {
			py = fmt.Sprintf(`(attr "python%s")`, strings.ReplaceAll(strings.Join(strings.Split(v, ".")[:min(2, len(strings.Split(v, ".")))], ""), "_", ""))
		}
		return fmt.Sprintf(`[ (withPip %s) (attr "uv") ]`, py)
	case "bun", "deno":
		return fmt.Sprintf(`[ (attr %q) ]`, t.Name)
	case "playwright":
		return "(libsOf browserLibs ++ browserLibs)" // Node comes from the node toolchain; the CLI and Chromium from setup
	}
	if t.Nix != "" {
		return fmt.Sprintf(`[ (attr %q) ]`, t.Nix)
	}
	return "[ ]" // a primary-source install, run in setup
}

// packageSet is every nixpkgs attribute for p: the base set, its packages,
// and tools installed via apt or nix.
func packageSet(p *Persona) []string {
	seen := map[string]bool{}
	var out []string
	add := func(names ...string) {
		for _, n := range names {
			valid := n != ""
			for _, alt := range strings.Split(n, "|") { // "a|b": a, else b (internal mappings)
				valid = valid && ValidateNixPackage(alt) == nil
			}
			if valid && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	add(basePackages...)
	// A C compiler only where the toolchain needs one: Rust links with it, Go
	// uses it for cgo, and Python builds native extensions. It is a third of a
	// gigabyte, so the rest go without.
	for _, tc := range []string{"rust", "go", "python"} {
		if hasToolchain(p, tc) {
			add("gcc")
			break
		}
	}
	for _, a := range p.AptPackages {
		add(AptToNix(a)...)
	}
	add(p.Packages...)
	for _, t := range p.Tools {
		if t.Via == "apt" {
			add(AptToNix(t.Package)...)
		} else if t.Via == "nix" {
			add(t.Package)
		}
	}
	sort.Strings(out[len(basePackages):])
	return out
}

type flakeData struct {
	Name, Nixpkgs, System    string
	Packages                 []string
	Toolchains               []flakeToolchain
	BrowserLibs, FHSLibs     []string
	Env                      []string
	Playwright, Dotnet, Rust bool
}

type flakeToolchain struct{ Name, Expr string }

var flakeTmpl = template.Must(template.New("flake.nix").Parse(`# persona: {{.Name}}
# Generated by persona: the persona's base image, built with Nix
# (dockerTools.buildLayeredImage). Edit freely with ` + "`persona edit {{.Name}}`" + `; once
# hand-edited, ` + "`persona update`" + ` won't overwrite it without --force.
# Setup Nix can't express runs afterwards: see setup-root.sh and setup-user.sh.
{
  description = "persona {{.Name}}";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/{{.Nixpkgs}}";

  outputs = { self, nixpkgs }:
    let
      system = "{{.System}}";
      pkgs = import nixpkgs { inherit system; config.allowUnfree = true; };
      lib = pkgs.lib;

      # A package by attribute path ("a|b" tries a, then b), or null (with a
      # warning) if nixpkgs lacks it or has removed it. Builds never fail over
      # one missing package.
      quiet = name: let r = builtins.tryEval (lib.attrByPath (lib.splitString "." name) null pkgs);
        in if r.success then r.value else null;
      attr = name:
        let found = builtins.filter (p: p != null) (map quiet (lib.splitString "|" name));
        in if found != [ ] then builtins.head found
           else lib.warn "persona: nixpkgs has no package ${name}; skipped" null;
      # The library output, so its .so files land in /lib.
      libsOf = ps: map (p: if p == null then null else lib.getLib p) ps;
      # Only the given output of each package: buildEnv would otherwise also
      # link (and the image carry) its man and doc outputs.
      one = p: if p == null then null else p // { meta = (p.meta or { }) // { outputsToInstall = [ (p.outputName or "out") ]; }; };
      try = p: let r = builtins.tryEval p; in r.success && r.value;
      any = p: true;
      stable = p: try (!(lib.hasInfix "-" (p.version or "")));
      # Node becomes LTS in October of its even major, around minor 9.
      nodeLTS = p: try (lib.mod (lib.toInt (lib.versions.major p.version)) 2 == 0
        && lib.toInt (lib.versions.minor p.version) >= 9);
      # The first of names that exists and satisfies ok (newest first).
      newest = what: ok: names:
        let found = builtins.filter (p: p != null && ok p)
          (map (n: let r = builtins.tryEval (lib.attrByPath [ n ] null pkgs); in if r.success then r.value else null) names);
        in if found == [ ] then lib.warn "persona: no ${what} package found in nixpkgs" null else builtins.head found;

      withPip = py: if py == null then null else py.withPackages (ps: [ ps.pip ]);

      fhsLibs = libsOf (map attr [ {{range .FHSLibs}}"{{.}}" {{end}}]);
      browserLibs = map attr [ {{range .BrowserLibs}}"{{.}}" {{end}}];

      packages = map attr [
{{- range .Packages}}
        "{{.}}"
{{- end}}
      ];

      toolchains = lib.concatLists [
{{- range .Toolchains}}
        {{.Expr}} # {{.Name}}
{{- end}}
      ];

      root = pkgs.buildEnv {
        name = "persona-{{.Name}}-root";
        paths = map one (builtins.filter (p: p != null) (packages ++ fhsLibs ++ toolchains));
        pathsToLink = [ "/bin" "/etc" "/lib" "/libexec" "/share" "/include" ];
        ignoreCollisions = true;
      };
    in {
      packages.${system}.default = pkgs.dockerTools.buildLayeredImage {
        name = "persona/{{.Name}}";
        tag = "base";
        contents = [ root pkgs.dockerTools.usrBinEnv pkgs.dockerTools.binSh pkgs.dockerTools.caCertificates ];
        maxLayers = 100;
        fakeRootCommands = ''
          # Directories that are a single package's symlink become real ones.
          for d in etc lib share bin; do
            if [ -L "$d" ]; then t=$(readlink -f "$d"); rm "$d"; mkdir "$d"; cp -rs "$t"/. "$d"/; chmod -R u+w "$d"; fi
          done
          mkdir -p etc tmp run proc sys dev workspace home/claude root lib64 sbin usr/local/bin var/tmp
          chmod 1777 tmp var/tmp
          rm -f etc/passwd etc/group etc/shadow
          printf 'root:x:0:0:root:/root:/bin/bash\nclaude:x:1000:1000:claude:/home/claude:/bin/bash\n' > etc/passwd
          printf 'root:x:0:\nclaude:x:1000:\n' > etc/group
          printf 'root:!:1::::::\nclaude:!:1::::::\n' > etc/shadow
          chmod 0640 etc/shadow
          ln -sf ${pkgs.glibc}/lib/ld-linux-x86-64.so.2 lib64/ld-linux-x86-64.so.2
          chown -R 1000:1000 home/claude workspace
        '';
        config = {
          User = "root";
          WorkingDir = "/workspace";
          Env = [
{{- range .Env}}
            "{{.}}"
{{- end}}
          ];
          Labels = { "persona.managed" = "true"; "persona.name" = "{{.Name}}"; };
        };
      };
    };
}
`))

// imageEnv is the environment every image has, base and final.
func imageEnv(p *Persona, harness string, playwright, dotnet bool) []string {
	home := ImageHome
	var dirs []string
	for _, t := range p.Toolchains {
		dirs = append(dirs, t.Path...)
	}
	dirs = append(dirs, home+"/.local/bin", home+"/.npm-global/bin", home+"/.cargo/bin", home+"/go/bin",
		home+"/.dotnet/tools", "/usr/local/bin", "/bin", "/usr/bin", "/usr/local/sbin", "/sbin", "/usr/sbin")
	env := []string{
		"PATH=" + strings.Join(dirs, ":"),
		"LANG=C.UTF-8", "TERM=xterm-256color",
		"SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt", "NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt",
		"GIT_SSL_CAINFO=/etc/ssl/certs/ca-bundle.crt",
		"NPM_CONFIG_PREFIX=" + home + "/.npm-global",
		"PERSONA_NAME=" + p.Name, "PERSONA_HARNESS=" + harness,
		"TZDIR=/share/zoneinfo",
		// Compilers and pkg-config look here for the image's headers and libraries
		// (a Nix toolchain doesn't search /include and /lib by itself).
		"CPATH=/include", "LIBRARY_PATH=/lib", "PKG_CONFIG_PATH=/lib/pkgconfig:/share/pkgconfig",
	}
	if dotnet {
		env = append(env, "DOTNET_ROOT=/share/dotnet", "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1")
	}
	if hasToolchain(p, "java") {
		env = append(env, "JAVA_HOME=/lib/openjdk")
	}
	if playwright {
		env = append(env, "LD_LIBRARY_PATH=/lib", "FONTCONFIG_FILE=/etc/fonts/fonts.conf")
	}
	return env
}

// mcpRuntimes are the toolchains every image gets: node (npx) and python
// (uv, uvx).
var mcpRuntimes = []string{"node", "python"}

// MCPLaunchers are the commands every image has for launching MCP servers.
var MCPLaunchers = []string{"node", "npx", "uv", "uvx", "python3"}

func hasToolchain(p *Persona, name string) bool {
	for _, t := range p.Toolchains {
		if t.Name == name {
			return true
		}
	}
	return false
}

// renderFlake produces flake.nix for an effective persona.
func renderFlake(p *Persona, nixpkgs, system string) (string, error) {
	harness := p.Harness
	if harness == "" {
		harness = DefaultHarness
	}
	if nixpkgs == "" {
		nixpkgs = NixpkgsDefault
	}
	d := flakeData{Name: p.Name, Nixpkgs: nixpkgs, System: system, Packages: packageSet(p), BrowserLibs: browserLibs, FHSLibs: fhsLibs,
		Playwright: hasToolchain(p, "playwright"), Dotnet: hasToolchain(p, "dotnet"), Rust: hasToolchain(p, "rust")}
	for _, t := range p.Toolchains {
		d.Toolchains = append(d.Toolchains, flakeToolchain{Name: t.String(), Expr: toolchainNix(t)})
	}
	// npx and uvx launch most stdio MCP servers, so every image has them: the
	// node and python toolchains' defaults, which share their store paths.
	for _, tc := range mcpRuntimes {
		if !hasToolchain(p, tc) {
			d.Toolchains = append(d.Toolchains, flakeToolchain{Name: tc + " (npx/uvx for MCP servers)", Expr: toolchainNix(Toolchain{Name: tc})})
		}
	}
	d.Env = imageEnv(p, harness, d.Playwright, d.Dotnet)
	var b bytes.Buffer
	err := flakeTmpl.Execute(&b, d)
	return b.String(), err
}
