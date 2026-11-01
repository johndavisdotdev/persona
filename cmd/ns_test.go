package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/johndavisdev/persona/internal/instance"
)

func TestRenderSpawnGroups(t *testing.T) {
	cs := []instance.Container{
		{Instance: "a", Persona: "dev", State: "running", Namespace: "default"},
		{Instance: "b", Persona: "dev", State: "exited", Namespace: "lab"},
		{Instance: "c", Persona: "dev", State: "running", Namespace: "gone"}, // namespace since removed
	}
	var b bytes.Buffer
	renderSpawnGroups(&b, cs, []string{"default", "lab", "empty"}, "", true)
	out := b.String()
	for _, want := range []string{"namespace default", "namespace lab", "namespace empty", "no spawns", "namespace gone"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "namespace default") > strings.Index(out, "namespace lab") {
		t.Error("namespaces out of order")
	}

	b.Reset()
	renderSpawnGroups(&b, cs, []string{"default", "lab", "empty"}, "lab", false)
	if out := b.String(); strings.Contains(out, "namespace default") || !strings.Contains(out, "namespace lab") {
		t.Errorf("filtered output:\n%s", out)
	}
	b.Reset()
	renderSpawnGroups(&b, cs, []string{"default", "lab", "empty"}, "", false)
	if strings.Contains(b.String(), "namespace empty") {
		t.Error("ps shows a namespace without spawns")
	}
}
