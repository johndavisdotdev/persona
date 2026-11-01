//go:build linux

package microvm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Persona namespaces are separate networks for microVMs: VMs in one reach
// each other (and see each other's beacon multicast), and nothing in any
// other. persona-vm creates them with persona-netns; "default" always exists.

// DefaultNamespace is the namespace created with the persona package.
const DefaultNamespace = "default"

var namespaceRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

// ValidNamespace checks a namespace name.
func ValidNamespace(name string) error {
	if !namespaceRE.MatchString(name) {
		return fmt.Errorf("invalid namespace %q: lowercase letters, digits and -, starting with a letter, at most 24", name)
	}
	return nil
}

// Namespace is one persona network namespace.
type Namespace struct {
	Name  string `json:"name"`
	Netns string `json:"netns"` // the Linux network namespace
	Up    bool   `json:"up"`
}

// Namespaces lists every persona namespace, default first.
func Namespaces(ctx context.Context) ([]Namespace, error) {
	var out bytes.Buffer
	if err := nsWrapper(ctx, &out, "ns-list"); err != nil {
		return nil, err
	}
	var ns []Namespace
	dec := json.NewDecoder(&out)
	for dec.More() {
		var n Namespace
		if err := dec.Decode(&n); err != nil {
			return nil, err
		}
		ns = append(ns, n)
	}
	return ns, nil
}

// CreateNamespace creates a namespace.
func CreateNamespace(ctx context.Context, name string) error {
	if err := ValidNamespace(name); err != nil {
		return err
	}
	return nsWrapper(ctx, nil, "ns-create", name)
}

// RemoveNamespace removes a namespace no VM is running in. It can't see
// stopped spawns: callers check those.
func RemoveNamespace(ctx context.Context, name string) error {
	if err := ValidNamespace(name); err != nil {
		return err
	}
	return nsWrapper(ctx, nil, "ns-remove", name)
}

// nsWrapper runs persona-vm, reporting its own error message as is.
func nsWrapper(ctx context.Context, stdout *bytes.Buffer, args ...string) error {
	var w io.Writer = io.Discard // a nil *bytes.Buffer would be a non-nil io.Writer
	if stdout != nil {
		w = stdout
	}
	if err := wrapper(ctx, w, args...); err != nil {
		msg := err.Error()
		if i := strings.LastIndex(msg, "persona-vm: "); i >= 0 {
			msg = msg[i+len("persona-vm: "):]
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}
