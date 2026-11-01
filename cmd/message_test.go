package cmd

import (
	"strings"
	"testing"

	"github.com/johndavisdev/persona/internal/instance"
)

func TestMessageTargets(t *testing.T) {
	cs := []instance.Container{
		{Instance: "jill", Persona: "dev", State: "running", Namespace: "default", IP: "10.200.1.1"},
		{Instance: "jack", Persona: "dev", State: "running", Namespace: "lab", IP: "10.200.1.2"},
		{Instance: "rev", Persona: "reviewer", State: "running", Namespace: "lab", IP: "10.200.1.3"},
		{Instance: "old", Persona: "dev", State: "exited", Namespace: "default", IP: "10.200.1.4"},
	}
	names := func(got []instance.Container) string {
		var n []string
		for _, c := range got {
			n = append(n, c.Instance)
		}
		return strings.Join(n, ",")
	}
	for _, c := range []struct {
		names            []string
		persona, ns, out string
		err              bool
	}{
		{out: "jill,jack,rev"},
		{ns: "lab", out: "jack,rev"},
		{persona: "dev", out: "jill,jack"},
		{persona: "dev", ns: "lab", out: "jack"},
		{names: []string{"jill", "rev"}, out: "jill,rev"},
		{names: []string{"jill"}, ns: "lab", err: true},
		{names: []string{"old"}, err: true}, // stopped
		{names: []string{"nobody"}, err: true},
		{persona: "nobody", err: true},
	} {
		got, err := messageTargets(cs, c.names, c.persona, c.ns)
		if c.err {
			if err == nil {
				t.Errorf("%+v: no error, got %s", c, names(got))
			}
			continue
		}
		if err != nil || names(got) != c.out {
			t.Errorf("%+v: got %s, %v; want %s", c, names(got), err, c.out)
		}
	}
}
