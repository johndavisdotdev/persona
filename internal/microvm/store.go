//go:build linux

package microvm

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// A Store holds /nix/store paths shared by every microVM, as read-only layer
// disks: each layer is an ext4 image of a set of store paths, written once
// and never modified (running VMs mount it). ExportSplit leaves an image's
// /nix/store out of its root disk and adds only the paths no layer has yet as
// one new layer; the guest mounts the image's layers together on /nix/store.
//
// Store paths are named by the hash of how they were built, so a path in a
// layer is the same as that path in any other image built by Nix. (An image
// whose /nix/store was changed by hand must not be split: see ExportRootfs.)
//
// Files: layer-<id>.ext4 and its manifest layer-<id>.paths (one store path
// name per line), in Dir.
type Store struct{ Dir string }

// StoreLabel is the ext4 label of store layers; the guest finds them by it.
const StoreLabel = "persona-store"

// MaxStoreLayers caps the layers one VM mounts (each is a virtio drive). An
// image that would need more gets one layer with everything outside its
// largest MaxStoreLayers-1.
const MaxStoreLayers = 12

const storePrefix = "nix/store/"

// layer is one store layer: its disk and the store paths in it.
type layer struct {
	id    string
	paths map[string]bool
}

func (s Store) disk(id string) string     { return filepath.Join(s.Dir, "layer-"+id+".ext4") }
func (s Store) manifest(id string) string { return filepath.Join(s.Dir, "layer-"+id+".paths") }

// Disk is the host path of layer id.
func (s Store) Disk(id string) string { return s.disk(id) }

// lock serializes changes to the store (concurrent builds).
func (s Store) lock() (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	_ = os.Chmod(s.Dir, 0o755)
	f, err := os.OpenFile(filepath.Join(s.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}

// layers reads every complete layer (disk and manifest both present).
func (s Store) layers() ([]layer, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []layer
	for _, e := range entries {
		id, ok := strings.CutSuffix(strings.TrimPrefix(e.Name(), "layer-"), ".paths")
		if !ok || !strings.HasPrefix(e.Name(), "layer-") {
			continue
		}
		if _, err := os.Stat(s.disk(id)); err != nil {
			continue
		}
		f, err := os.Open(s.manifest(id))
		if err != nil {
			return nil, err
		}
		l := layer{id: id, paths: map[string]bool{}}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if p := strings.TrimSpace(sc.Text()); p != "" {
				l.paths[p] = true
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// add makes sure every path in want is in some layer, writing one new layer
// from storeTar (a tar of store paths, named <path>[/...]) if needed. It
// returns the layers an image with exactly these paths mounts.
func (s Store) add(ctx context.Context, want map[string]bool, storeTar string) ([]string, error) {
	if len(want) == 0 {
		return nil, nil
	}
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	existing, err := s.layers()
	if err != nil {
		return nil, err
	}
	// The layers holding any wanted path, by how many they hold.
	type use struct {
		id string
		n  int
	}
	var used []use
	for _, l := range existing {
		n := 0
		for p := range want {
			if l.paths[p] {
				n++
			}
		}
		if n > 0 {
			used = append(used, use{l.id, n})
		}
	}
	sort.Slice(used, func(i, j int) bool { return used[i].n > used[j].n || used[i].n == used[j].n && used[i].id < used[j].id })
	if len(used) > MaxStoreLayers-1 {
		used = used[:MaxStoreLayers-1] // the rest go into this image's new layer
	}
	have := map[string]bool{}
	var ids []string
	for _, u := range used {
		ids = append(ids, u.id)
		for _, l := range existing {
			if l.id == u.id {
				for p := range l.paths {
					have[p] = true
				}
			}
		}
	}
	var missing []string
	for p := range want {
		if !have[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return ids, nil
	}
	sort.Strings(missing)
	h := sha256.New()
	for _, p := range missing {
		h.Write([]byte(p + "\n"))
	}
	id := hex.EncodeToString(h.Sum(nil))[:16]
	if err := s.writeLayer(ctx, id, missing, storeTar); err != nil {
		return nil, err
	}
	return append(ids, id), nil
}

// writeLayer writes layer id with the given paths, filtered from storeTar.
func (s Store) writeLayer(ctx context.Context, id string, paths []string, storeTar string) error {
	in := map[string]bool{}
	for _, p := range paths {
		in[p] = true
	}
	src, err := os.Open(storeTar)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(s.Dir, ".layer-*.tar")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	tr, tw := tar.NewReader(src), tar.NewWriter(tmp)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		top, _, _ := strings.Cut(hdr.Name, "/")
		if !in[top] {
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	fi, err := tmp.Stat()
	if err != nil {
		return err
	}
	disk := s.disk(id)
	part := disk + ".partial"
	if err := os.WriteFile(part, nil, 0o644); err != nil {
		return err
	}
	defer os.Remove(part)
	if err := os.Truncate(part, fi.Size()*5/4+64<<20); err != nil {
		return err
	}
	if b, err := exec.CommandContext(ctx, mkfs, "-q", "-F", "-L", StoreLabel, "-d", tmp.Name(), part).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 (store layer): %v: %s", err, b)
	}
	if err := os.Chmod(part, 0o644); err != nil {
		return err
	}
	// The manifest goes last: a layer without one doesn't exist yet.
	if err := os.Rename(part, disk); err != nil {
		return err
	}
	return writeFileAtomic(s.manifest(id), []byte(strings.Join(paths, "\n")+"\n"))
}

// GC deletes the layers no listed image uses. VMs that are running, or
// stopped with their own hard links, keep their layers' contents.
func (s Store) GC(inUse map[string]bool) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	ls, err := s.layers()
	if err != nil {
		return err
	}
	for _, l := range ls {
		if inUse[l.id] {
			continue
		}
		// A new layer isn't listed by its image until that export finishes.
		if fi, err := os.Stat(s.manifest(l.id)); err == nil && time.Since(fi.ModTime()) < 10*time.Minute {
			continue
		}
		if err := os.Remove(s.manifest(l.id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(s.disk(l.id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Size is the store's total size on disk, in bytes.
func (s Store) Size() int64 {
	var n int64
	entries, _ := os.ReadDir(s.Dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), ".ext4") {
			n += fi.Size()
		}
	}
	return n
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// storePathOf returns the store path an exported tar entry belongs to
// ("nix/store/<path>[/...]"), or "".
func storePathOf(name string) string {
	rest, ok := strings.CutPrefix(name, storePrefix)
	if !ok || rest == "" {
		return ""
	}
	top, _, _ := strings.Cut(rest, "/")
	return top
}
