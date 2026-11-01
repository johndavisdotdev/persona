//go:build linux

package instance

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/johndavisdev/persona/internal/microvm"
)

var _ Namespacer = (*VM)(nil)

func (v *VM) Namespaces(ctx context.Context) ([]string, error) {
	ns, err := microvm.Namespaces(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range ns {
		names = append(names, n.Name)
	}
	if !slices.Contains(names, DefaultNamespace) {
		names = append([]string{DefaultNamespace}, names...)
	}
	return names, nil
}

func (v *VM) CreateNamespace(ctx context.Context, name string) error {
	if name == DefaultNamespace {
		return fmt.Errorf("the %s namespace always exists", DefaultNamespace)
	}
	return microvm.CreateNamespace(ctx, name)
}

// RemoveNamespace refuses while any of this user's spawns, running or
// stopped, is in the namespace (a stopped one couldn't start again);
// persona-vm refuses while anyone's VM runs in it.
func (v *VM) RemoveNamespace(ctx context.Context, name string) error {
	if name == DefaultNamespace {
		return fmt.Errorf("the %s namespace can't be removed: persona always needs one", DefaultNamespace)
	}
	if err := microvm.ValidNamespace(name); err != nil {
		return err
	}
	recs, err := allRecords()
	if err != nil {
		return err
	}
	var in []string
	for _, r := range recs {
		if r.namespace() == name {
			in = append(in, r.Instance)
		}
	}
	if len(in) > 0 {
		slices.Sort(in)
		return fmt.Errorf("namespace %q still has spawns: %s (kill them first with `persona kill`)", name, strings.Join(in, ", "))
	}
	return microvm.RemoveNamespace(ctx, name)
}

// checkNamespace fails unless namespace exists and is up.
func (v *VM) checkNamespace(ctx context.Context, name string) error {
	if err := microvm.ValidNamespace(name); err != nil {
		return err
	}
	ns, err := microvm.Namespaces(ctx)
	if err != nil {
		return err
	}
	for _, n := range ns {
		if n.Name == name {
			if !n.Up {
				return fmt.Errorf("namespace %q is down: sudo systemctl restart persona-netns", name)
			}
			return nil
		}
	}
	return fmt.Errorf("no namespace %q: create it with `persona ns create %s`", name, name)
}
