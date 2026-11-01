//go:build linux

package microvm

import "testing"

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
