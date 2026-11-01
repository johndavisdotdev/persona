VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/johndavisdev/persona/cmd.Version=$(VERSION)

.PHONY: build test install release persona-vm persona-agent deb host-setup
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/persona .
persona-vm:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/persona-vm ./cmd/persona-vm
persona-agent:
	CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o bin/persona-agent ./cmd/persona-agent
# deb: dist/persona_<version>_amd64.deb, for Debian 13 (trixie) or later on x86_64.
deb:
	packaging/deb/build.sh
# host-setup: build the .deb and install it on this machine.
host-setup: deb
	sudo apt-get install -y --reinstall "./$$(ls -t dist/persona_*_amd64.deb | head -1)"
test:
	go test ./...
install:
	go install -trimpath -ldflags "$(LDFLAGS)" .
release:
	@for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
	  os=$${t%/*}; arch=$${t#*/}; ext=$$( [ $$os = windows ] && echo .exe ); \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/persona-$$os-$$arch$$ext . || exit 1; \
	done
