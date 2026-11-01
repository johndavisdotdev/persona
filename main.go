// Command persona builds, spawns, and multiplexes Claude Code personas, as
// containers or as Firecracker microVMs.
package main

import (
	_ "embed"
	"os"

	"github.com/johndavisdev/persona/cmd"
	"github.com/johndavisdev/persona/internal/agent/guest"
	"github.com/johndavisdev/persona/internal/persona"
)

// baseSettings is the default ~/.claude/settings.json content every persona
// starts from. Users can override it with `persona edit-base --settings`.
//
//go:embed base-settings.json
var baseSettings []byte

func main() {
	// Inside a microVM this binary is /sbin/persona-agent: init and guest CLI.
	if guest.IsAgent() {
		guest.Main()
	}
	persona.DefaultBaseSettings = baseSettings
	os.Exit(cmd.Execute())
}
