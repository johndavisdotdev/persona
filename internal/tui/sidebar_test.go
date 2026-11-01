package tui

import (
	"strings"
	"testing"

	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/persona"
)

func sidebarModel(filter string) *model {
	return &model{
		filter:     filter,
		personas:   []*persona.Persona{{Name: "dev"}, {Name: "idle"}},
		namespaces: []string{"default", "empty", "lab"},
		containers: []instance.Container{
			{Name: "persona-dev-a", Persona: "dev", State: "running", Namespace: "default"},
			{Name: "persona-dev-z", Persona: "dev", State: "exited", Namespace: "default"},
			{Name: "persona-dev-b", Persona: "dev", State: "running", Namespace: "lab"},
			{Name: "persona-idle-c", Persona: "idle", State: "exited", Namespace: "lab"},
		},
	}
}

func describe(items []sideItem) string {
	var got []string
	for _, it := range items {
		switch {
		case it.header:
			got = append(got, "# "+it.namespace)
		case it.container == "":
			got = append(got, it.namespace+"/"+it.persona)
		default:
			got = append(got, it.namespace+"/"+it.container)
		}
	}
	return strings.Join(got, " ")
}

// Only running spawns, and no namespace or persona without one.
func TestSidebarGroupsRunningSpawnsByNamespace(t *testing.T) {
	m := sidebarModel("")
	m.rebuildItems()
	want := "# default default/dev default/persona-dev-a # lab lab/dev lab/persona-dev-b"
	if got := describe(m.items); got != want {
		t.Fatalf("items = %s\nwant    %s", got, want)
	}
	if m.items[m.cursor].header {
		t.Error("cursor starts on a heading")
	}
	m.cursor = 2
	m.moveCursor(1) // over "# lab"
	if m.cursor != 4 {
		t.Errorf("down from the last default row went to %d, want 4", m.cursor)
	}
	m.moveCursor(-1)
	if m.cursor != 2 {
		t.Errorf("up went to %d, want 2", m.cursor)
	}
}

func TestSidebarFilter(t *testing.T) {
	m := sidebarModel("lab")
	m.rebuildItems()
	if got, want := describe(m.items), "lab/dev lab/persona-dev-b"; got != want {
		t.Errorf("items = %s, want %s", got, want)
	}
}
