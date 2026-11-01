//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestTapNameAndIDs(t *testing.T) {
	if n := TapName("reviewer-abc123"); len(n) > 15 || n != TapName("reviewer-abc123") {
		t.Errorf("TapName = %q", n)
	}
	for id, ok := range map[string]bool{"a": true, "reviewer-1": true, "-x": false, "A": false, "a/b": false, "..": false} {
		if idRE.MatchString(id) != ok {
			t.Errorf("idRE(%q) != %v", id, ok)
		}
	}
}

func TestAllocUID(t *testing.T) {
	a := allocUID("x", nil)
	b := allocUID("x", map[int]bool{a: true})
	if a == b || a < uidBase || b >= uidBase+uidCount {
		t.Errorf("a=%d b=%d", a, b)
	}
}

func TestParseFile(t *testing.T) {
	f, err := parseFile("ro:/a/b:c:vmlinux")
	if err != nil || f.src != "/a/b:c" || f.name != "vmlinux" {
		t.Fatalf("%+v %v", f, err)
	}
	for _, bad := range []string{"rx:/a:b", "ro:rel:b", "ro:/a:../b", "ro:/a:run", "ro:/a:x/y", "ro:/a"} {
		if _, err := parseFile(bad); err == nil {
			t.Errorf("parseFile(%q) succeeded", bad)
		}
	}
}

func TestOpenOwnedRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	os.WriteFile(real, []byte("x"), 0o644)
	link := filepath.Join(dir, "link")
	os.Symlink("/etc/passwd", link)
	if f, err := openOwned(real, os.Getuid(), false); err != nil {
		t.Errorf("real: %v", err)
	} else {
		f.Close()
	}
	if _, err := openOwned(link, os.Getuid(), false); err == nil {
		t.Error("symlink accepted")
	}
	if _, err := openOwned(real, os.Getuid()+1, false); err == nil {
		t.Error("foreign owner accepted")
	}
	if _, err := openOwned(dir, os.Getuid(), false); err == nil {
		t.Error("directory accepted as file")
	}
}

func TestSparseCopy(t *testing.T) {
	dir := t.TempDir()
	src, _ := os.Create(filepath.Join(dir, "src"))
	src.Truncate(64 << 20)
	src.WriteAt([]byte("head"), 0)
	src.WriteAt([]byte("tail"), 64<<20-4)
	dst, _ := os.Create(filepath.Join(dir, "dst"))
	if err := SparseCopy(dst, src); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(src.Name())
	b, _ := os.ReadFile(dst.Name())
	if !bytes.Equal(a, b) {
		t.Fatal("contents differ")
	}
	var st unix.Stat_t
	unix.Fstat(int(dst.Fd()), &st)
	if st.Blocks*512 > 4<<20 {
		t.Errorf("dst not sparse: %d bytes allocated", st.Blocks*512)
	}
}

func TestSparseCopyAcrossFilesystems(t *testing.T) {
	other := "/dev/shm"
	if _, err := os.Stat(other); err != nil {
		t.Skip("no /dev/shm")
	}
	src, _ := os.Create(filepath.Join(t.TempDir(), "src"))
	src.Truncate(8 << 20)
	src.WriteAt([]byte("data"), 4<<20)
	d, err := os.MkdirTemp(other, "sparse")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(d)
	dst, _ := os.Create(filepath.Join(d, "dst"))
	if err := SparseCopy(dst, src); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(src.Name())
	b, _ := os.ReadFile(dst.Name())
	if !bytes.Equal(a, b) {
		t.Fatal("contents differ")
	}
}

func TestOpenReadableRootFiles(t *testing.T) {
	// /etc/hostname-like files: root-owned and world-readable.
	if f, err := openReadable("/etc/passwd", 4242); err != nil {
		t.Errorf("root-owned world-readable file refused: %v", err)
	} else {
		f.Close()
	}
	if _, err := openOwned("/etc/passwd", 4242, false); err == nil {
		t.Error("openOwned accepted a root-owned file")
	}
	if _, err := openReadable("/etc/shadow", 4242); err == nil {
		t.Error("openReadable accepted a non-world-readable file")
	}
}
