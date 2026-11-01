// persona-agent builds the guest agent on its own. The persona binary contains
// the same agent (see internal/agent/guest) and is what images normally ship.
package main

import "github.com/johndavisdev/persona/internal/agent/guest"

func main() { guest.Main() }
