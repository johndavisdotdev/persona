//go:build !linux

// Package guest is persona-agent, which only runs inside Linux microVMs.
package guest

// IsAgent is always false off Linux.
func IsAgent() bool { return false }

// Main is never called off Linux.
func Main() { panic("persona-agent runs only on Linux") }
