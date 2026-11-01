//go:build linux

package microvm

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// exportTar is an image export: a root with a few store paths.
func exportTar(t *testing.T, storePaths ...string) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	add := func(name string, typ byte, body string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: typ})
		tw.Write([]byte(body))
	}
	add("etc/", tar.TypeDir, "")
	add("etc/passwd", tar.TypeReg, "root:x:0:0::/root:/bin/sh\n")
	add("nix/", tar.TypeDir, "")
	add("nix/store/", tar.TypeDir, "")
	for _, p := range storePaths {
		add("nix/store/"+p+"/", tar.TypeDir, "")
		add("nix/store/"+p+"/bin/", tar.TypeDir, "")
		add("nix/store/"+p+"/bin/tool", tar.TypeReg, p)
	}
	tw.Close()
	return &b
}

func names(t *testing.T, r io.Reader) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
}

func TestRewriteTarSplitsStore(t *testing.T) {
	var root, store bytes.Buffer
	split := &storeSplit{tw: tar.NewWriter(&store), paths: map[string]bool{}}
	if err := rewriteTar(&root, exportTar(t, "aaa-glibc", "bbb-git"), nil, nil, split); err != nil {
		t.Fatal(err)
	}
	split.tw.Close()
	r := names(t, &root)
	if !slices.Contains(r, "nix/store/") || slices.Contains(r, "nix/store/aaa-glibc/bin/tool") {
		t.Errorf("root disk: %v", r)
	}
	s := names(t, &store)
	if !slices.Contains(s, "aaa-glibc/bin/tool") || !slices.Contains(s, "bbb-git/") || len(split.paths) != 2 {
		t.Errorf("store: %v %v", s, split.paths)
	}
}

func storeTar(t *testing.T, dir string, paths ...string) string {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, p := range paths {
		tw.WriteHeader(&tar.Header{Name: p + "/", Mode: 0o755, Typeflag: tar.TypeDir})
		tw.WriteHeader(&tar.Header{Name: p + "/f", Mode: 0o644, Size: int64(len(p)), Typeflag: tar.TypeReg})
		tw.Write([]byte(p))
	}
	tw.Close()
	f := filepath.Join(dir, "in.tar")
	if err := os.WriteFile(f, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func set(ps ...string) map[string]bool {
	m := map[string]bool{}
	for _, p := range ps {
		m[p] = true
	}
	return m
}

func TestStoreSharesLayers(t *testing.T) {
	if _, err := os.Stat(mkfs); err != nil {
		t.Skip("no mkfs.ext4")
	}
	ctx, tmp := context.Background(), t.TempDir()
	s := Store{Dir: filepath.Join(tmp, "store")}

	first, err := s.add(ctx, set("a", "b"), storeTar(t, tmp, "a", "b"))
	if err != nil || len(first) != 1 {
		t.Fatalf("first image: %v %v", first, err)
	}
	// Everything already stored: no new layer.
	same, err := s.add(ctx, set("a"), storeTar(t, tmp, "a"))
	if err != nil || !slices.Equal(same, first) {
		t.Fatalf("subset: %v %v", same, err)
	}
	// One new path: the old layer plus a layer with just it.
	second, err := s.add(ctx, set("a", "c"), storeTar(t, tmp, "a", "c"))
	if err != nil || len(second) != 2 || second[0] != first[0] {
		t.Fatalf("second image: %v %v", second, err)
	}
	ls, _ := s.layers()
	for _, l := range ls {
		if l.id == second[1] && (len(l.paths) != 1 || !l.paths["c"]) {
			t.Errorf("new layer holds %v, want only c", l.paths)
		}
	}
	// GC keeps what's in use, and fresh layers (an export may not have listed them yet).
	if err := s.GC(set(first[0])); err != nil {
		t.Fatal(err)
	}
	if ls, _ := s.layers(); len(ls) != 2 {
		t.Fatalf("GC removed a fresh layer: %d left", len(ls))
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(s.manifest(second[1]), old, old)
	if err := s.GC(set(first[0])); err != nil {
		t.Fatal(err)
	}
	if ls, _ := s.layers(); len(ls) != 1 || ls[0].id != first[0] {
		t.Fatalf("after GC: %+v", ls)
	}
	if _, err := os.Stat(s.Disk(second[1])); !os.IsNotExist(err) {
		t.Error("unused layer disk not deleted")
	}
}

func TestStoreCapsLayersPerImage(t *testing.T) {
	if _, err := os.Stat(mkfs); err != nil {
		t.Skip("no mkfs.ext4")
	}
	ctx, tmp := context.Background(), t.TempDir()
	s := Store{Dir: filepath.Join(tmp, "store")}
	var all []string
	for i := 0; i < MaxStoreLayers+2; i++ {
		p := string(rune('a' + i))
		all = append(all, p)
		if _, err := s.add(ctx, set(p), storeTar(t, tmp, p)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.add(ctx, set(all...), storeTar(t, tmp, all...))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxStoreLayers {
		t.Errorf("image mounts %d layers, want %d", len(got), MaxStoreLayers)
	}
}
