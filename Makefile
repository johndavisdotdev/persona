VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/johndavisdev/persona/cmd.Version=$(VERSION)

.PHONY: build test install release
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/persona .
test:
	go test ./...
install:
	go install -trimpath -ldflags "$(LDFLAGS)" .
release:
	@for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
	  os=$${t%/*}; arch=$${t#*/}; ext=$$( [ $$os = windows ] && echo .exe ); \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/persona-$$os-$$arch$$ext . || exit 1; \
	done
