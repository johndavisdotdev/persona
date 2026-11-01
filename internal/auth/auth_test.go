package auth

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/johndavisdev/persona/internal/config"
)

func TestSaveLoadDelete(t *testing.T) {
	if err := config.Init("", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if s, err := Load(); s != nil || err != nil {
		t.Fatalf("empty Load = %v %v", s, err)
	}
	if _, err := Save("has space"); err == nil {
		t.Error("token with whitespace accepted")
	}
	s, err := Save("  sk-ant-oat01-abcdefghijkl\n")
	if err != nil {
		t.Fatal(err)
	}
	if s.Token != "sk-ant-oat01-abcdefghijkl" || s.Masked() != "…ghijkl" {
		t.Errorf("saved %+v masked %s", s, s.Masked())
	}
	for path, want := range map[string]os.FileMode{Path(): 0o600, filepath.Dir(Path()): 0o700} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v (%v)", path, fi.Mode().Perm(), want, err)
		}
	}
	got, err := Load()
	if err != nil || got.Token != s.Token || got.Expires().Sub(got.Created) != Lifetime {
		t.Errorf("Load = %+v %v", got, err)
	}
	if err := Delete(); err != nil {
		t.Fatal(err)
	}
	if s, _ := Load(); s != nil {
		t.Error("token survived Delete")
	}
	if err := Delete(); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func TestForInstance(t *testing.T) {
	forward := []string{APIKeyEnv, TokenEnv, "GITHUB_PAT"}
	stored := &Stored{Token: "tok", Created: time.Now()}
	cases := []struct {
		name        string
		host        map[string]string
		explicit    []string
		stored      *Stored
		wantForward []string
		wantInject  []string
	}{
		{"api key only", map[string]string{APIKeyEnv: "k"}, nil, nil, []string{APIKeyEnv}, nil},
		{"stored token beats exported key", map[string]string{APIKeyEnv: "k", "GITHUB_PAT": "p"}, nil, stored,
			[]string{"GITHUB_PAT"}, []string{TokenEnv + "=tok"}},
		{"explicit -e keeps the key", map[string]string{APIKeyEnv: "k"}, []string{APIKeyEnv}, stored,
			[]string{APIKeyEnv}, []string{TokenEnv + "=tok"}},
		{"exported token beats stored", map[string]string{TokenEnv: "host"}, nil, stored, []string{TokenEnv}, nil},
		{"nothing", map[string]string{}, nil, nil, nil, nil},
	}
	for _, c := range cases {
		p := ForInstance(env(c.host), forward, c.explicit, c.stored)
		if !slices.Equal(p.Forward, c.wantForward) || !slices.Equal(p.Inject, c.wantInject) {
			t.Errorf("%s: forward %v inject %v, want %v %v", c.name, p.Forward, p.Inject, c.wantForward, c.wantInject)
		}
		if p.HasCredentials() != (c.name != "nothing") {
			t.Errorf("%s: HasCredentials = %v", c.name, p.HasCredentials())
		}
	}
	old := &Stored{Token: "tok", Created: time.Now().Add(-Lifetime + 5*24*time.Hour)}
	if p := ForInstance(env(nil), forward, nil, old); len(p.Notes) != 1 {
		t.Errorf("no expiry warning: %v", p.Notes)
	}
}
