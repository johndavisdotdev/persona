//go:build linux

package microvm

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

func TestResolveMergedUsr(t *testing.T) {
	links := map[string]string{"sbin": "usr/sbin", "lib": "/usr/lib", "usr/local/x": "../y"}
	for in, want := range map[string]string{
		"/sbin/persona-agent": "usr/sbin/persona-agent",
		"/lib/a/b":            "usr/lib/a/b",
		"/etc/x":              "etc/x",
		"/sbin":               "sbin", // the final component itself is not followed
	} {
		if got := resolve(in, links); got != want {
			t.Errorf("resolve(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRewriteTarEdits(t *testing.T) {
	var in bytes.Buffer
	tw := tar.NewWriter(&in)
	for name, body := range map[string]string{"home/claude/.claude.json": `{"a":1}`, "etc/os-release": "debian"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Uid: 1000, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()

	edits := map[string]Edit{
		"/home/claude/.claude.json": func(old []byte) ([]byte, error) { return append(old, " edited"...), nil },
		"/absent":                   func([]byte) ([]byte, error) { return []byte("x"), nil },
	}
	var out bytes.Buffer
	if err := rewriteTar(&out, &in, nil, edits, nil); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	tr := tar.NewReader(&out)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[hdr.Name] = string(b)
		if hdr.Name == "home/claude/.claude.json" && (hdr.Uid != 1000 || hdr.Mode != 0o600) {
			t.Errorf("edit lost ownership or mode: uid %d mode %o", hdr.Uid, hdr.Mode)
		}
	}
	want := map[string]string{"home/claude/.claude.json": `{"a":1} edited`, "etc/os-release": "debian"}
	if len(got) != len(want) {
		t.Errorf("entries %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
