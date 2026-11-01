VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/johndavisdev/persona/cmd.Version=$(VERSION)

.PHONY: build test install release persona-vm persona-agent persona-beacon deb host-setup web
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/persona .
persona-vm:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/persona-vm ./cmd/persona-vm
persona-agent:
	CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o bin/persona-agent ./cmd/persona-agent
# persona-beacon: the MCP server (needs the .NET SDK). Publishing writes the
# Claude Code marketplace "persona" (plugin "beacon") that every image gets to
# bin/beacon/marketplace; point beacon_marketplace in the persona config at it
# to use a dev build.
persona-beacon:
	dotnet publish beacon/Beacon.csproj -nologo -v q -c Release -r linux-x64 -o bin/beacon
	install -m 0755 bin/beacon/persona-beacon bin/persona-beacon
# deb: dist/persona_<version>_amd64.deb, for Debian 13 (trixie) or later on x86_64.
deb:
	packaging/deb/build.sh
# host-setup: build the .deb and install it on this machine (the newest build,
# even when its version sorts lower, e.g. 0.1.1+1+g… after 0.1.1+dirty).
host-setup: deb
	sudo apt-get install -y --reinstall --allow-downgrades "./$$(ls -t dist/persona_*_amd64.deb | head -1)"
test:
	go test ./...
	dotnet build beacon/Beacon.csproj -nologo -v q
install:
	go install -trimpath -ldflags "$(LDFLAGS)" .
release:
	@for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
	  os=$${t%/*}; arch=$${t#*/}; ext=$$( [ $$os = windows ] && echo .exe ); \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/persona-$$os-$$arch$$ext . || exit 1; \
	done

# web: build the web UI (web/, needs pnpm) into internal/server/dist, which
# persona server embeds in the binary. Rebuild persona afterwards.
web:
	cd web && pnpm install --frozen-lockfile && pnpm build
	rm -rf internal/server/dist
	cp -r web/dist internal/server/dist
