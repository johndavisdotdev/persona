// Command persona builds, spawns, and multiplexes containerized Claude Code personas.
package main

import (
	_ "embed"
	"os"

	"github.com/johndavisdev/persona/cmd"
	"github.com/johndavisdev/persona/internal/persona"
)

// baseSettings is the default ~/.claude/settings.json content every persona
// starts from. Users can override it with `persona edit-base --settings`.
//
//go:embed base-settings.json
var baseSettings []byte

func main() {
	persona.DefaultBaseSettings = baseSettings
	os.Exit(cmd.Execute())
}
