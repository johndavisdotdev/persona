//go:build linux

package instance

import (
	"context"
	"strings"
	"testing"

	"github.com/johndavisdev/persona/internal/config"
)

func TestRemoveNamespaceRefusesWithSpawns(t *testing.T) {
	if err := config.Init("", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*record{
		{Name: "persona-p-a", Persona: "p", Instance: "a", Namespace: "lab"},
		{Name: "persona-p-b", Persona: "p", Instance: "b"}, // default
	} {
		if err := r.save(); err != nil {
			t.Fatal(err)
		}
	}
	v := &VM{}
	err := v.RemoveNamespace(context.Background(), "lab")
	if err == nil || !strings.Contains(err.Error(), "still has spawns: a") {
		t.Errorf("RemoveNamespace(lab) = %v, want a refusal naming spawn a", err)
	}
	if err := v.RemoveNamespace(context.Background(), DefaultNamespace); err == nil || !strings.Contains(err.Error(), "can't be removed") {
		t.Errorf("RemoveNamespace(default) = %v", err)
	}
	if err := v.RemoveNamespace(context.Background(), "Bad_Name"); err == nil {
		t.Error("invalid name accepted")
	}
}

func TestListReportsNamespace(t *testing.T) {
	r := &record{Namespace: ""}
	if r.namespace() != DefaultNamespace {
		t.Errorf("record without a namespace is in %q", r.namespace())
	}
	if NamespaceOf(Container{}) != DefaultNamespace || NamespaceOf(Container{Namespace: "lab"}) != "lab" {
		t.Error("NamespaceOf")
	}
}
