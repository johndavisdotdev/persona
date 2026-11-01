//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// Persona namespaces are Linux network namespaces made by persona-netns:
// "default" is netns "persona" (created at boot by persona-netns.service),
// any other NAME is netns "persona-NAME". Every one has the same bridge and
// subnet inside, so a VM's network config doesn't depend on its namespace.
const defaultNamespace = "default"

var nsRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

func namespaceOrDefault(name string) string {
	if name == "" {
		return defaultNamespace
	}
	return name
}

// netnsOf is the Linux netns name of a persona namespace.
func netnsOf(name string) string {
	if name = namespaceOrDefault(name); name == defaultNamespace {
		return "persona"
	}
	return "persona-" + name
}

func netnsPath(name string) string { return "/run/netns/" + netnsOf(name) }

func validNamespace(name string) error {
	if !nsRE.MatchString(name) {
		return fmt.Errorf("invalid namespace %q (lowercase letters, digits and -, starting with a letter, at most 24)", name)
	}
	return nil
}

// namespaceUp checks that a namespace exists and is up.
func namespaceUp(name string) error {
	if err := validNamespace(name); err != nil {
		return err
	}
	if _, err := os.Stat(netnsPath(name)); err != nil {
		if name == defaultNamespace {
			return errors.New("the persona network namespace is down: sudo systemctl start persona-netns")
		}
		return fmt.Errorf("no namespace %q (create it with `persona ns create %s`)", name, name)
	}
	return nil
}

func netns(args ...string) error {
	cmd := exec.Command(netnsScript, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(strings.TrimSpace(string(out)))
	}
	return nil
}

func cmdNSCreate(args []string) error {
	if len(args) != 1 {
		usage()
	}
	if err := validNamespace(args[0]); err != nil {
		return err
	}
	if args[0] == defaultNamespace {
		return errors.New("the default namespace always exists")
	}
	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()
	return netns("add", args[0])
}

// cmdNSRemove removes a namespace that no VM (of any user) is running in.
// Callers check their own stopped spawns, which this can't see.
func cmdNSRemove(args []string) error {
	if len(args) != 1 {
		usage()
	}
	name := args[0]
	if err := validNamespace(name); err != nil {
		return err
	}
	if name == defaultNamespace {
		return errors.New("the default namespace can't be removed")
	}
	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()
	states, err := allStates()
	if err != nil {
		return err
	}
	var running []string
	for _, s := range states {
		if namespaceOrDefault(s.Namespace) == name && alive(s) {
			running = append(running, s.ID)
		}
	}
	if len(running) > 0 {
		sort.Strings(running)
		return fmt.Errorf("namespace %q has running microVMs: %s", name, strings.Join(running, ", "))
	}
	return netns("del", name)
}

// NamespaceInfo is what ns-list prints, one JSON object per line.
type NamespaceInfo struct {
	Name  string `json:"name"`
	Netns string `json:"netns"`
	Up    bool   `json:"up"`
}

func cmdNSList(args []string) error {
	if len(args) != 0 {
		usage()
	}
	cmd := exec.Command(netnsScript, "list")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("persona-netns list: %w", err)
	}
	enc := json.NewEncoder(os.Stdout)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		_, err := os.Stat("/run/netns/" + f[1])
		if err := enc.Encode(NamespaceInfo{Name: f[0], Netns: f[1], Up: err == nil}); err != nil {
			return err
		}
	}
	return nil
}
