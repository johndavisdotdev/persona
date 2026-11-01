//go:build linux

package microvm

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// AgentPath is where persona-agent lives in every rootfs; the kernel runs it as init.
const AgentPath = "/sbin/persona-agent"

// mkfs is called by absolute path: /usr/sbin is often not on a user's PATH.
var mkfs = "/usr/sbin/mkfs.ext4"

// File is extra content to place in an exported rootfs.
type File struct {
	Data []byte
	Mode int64
}

// Edit rewrites a file of the image as it is exported. Ownership and mode are
// kept. Files the image doesn't have are left absent.
type Edit func(old []byte) ([]byte, error)

// ExportRootfs flattens a Docker image into an ext4 image at out, adding (or
// replacing) extra files and applying edits to existing ones. It needs no
// root: mkfs.ext4 populates the filesystem straight from the tar stream,
// ownership included.
func ExportRootfs(ctx context.Context, docker, image, out string, extra map[string]File, edits map[string]Edit) error {
	_, err := export(ctx, docker, image, out, extra, edits, nil)
	return err
}

// ExportSplit is ExportRootfs for an image built with Nix: its /nix/store is
// left out of the root disk (but for the empty directory) and goes into
// store, which writes a layer of the paths it doesn't have yet. It returns the
// layers the image mounts on /nix/store, in no particular order.
//
// Only images whose /nix/store is exactly what Nix built may be split: the
// store trusts a path's name to identify its contents.
func ExportSplit(ctx context.Context, docker, image, out string, extra map[string]File, edits map[string]Edit, store Store) ([]string, error) {
	return export(ctx, docker, image, out, extra, edits, &store)
}

func export(ctx context.Context, docker, image, out string, extra map[string]File, edits map[string]Edit, store *Store) ([]string, error) {
	cid, err := exec.CommandContext(ctx, docker, "create", image, "/bin/true").Output()
	if err != nil {
		return nil, fmt.Errorf("docker create %s: %w", image, exitErr(err))
	}
	id := strings.TrimSpace(string(cid))
	defer exec.Command(docker, "rm", "-f", id).Run()

	tmp, err := os.CreateTemp(filepath.Dir(out), ".rootfs-*.tar")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	var split *storeSplit
	if store != nil {
		st, err := os.CreateTemp(filepath.Dir(out), ".store-*.tar")
		if err != nil {
			return nil, err
		}
		defer os.Remove(st.Name())
		defer st.Close()
		split = &storeSplit{tw: tar.NewWriter(st), file: st, paths: map[string]bool{}}
	}

	export := exec.CommandContext(ctx, docker, "export", id)
	var stderr bytes.Buffer
	export.Stderr = &stderr
	pipe, err := export.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := export.Start(); err != nil {
		return nil, err
	}
	if err := rewriteTar(tmp, pipe, extra, edits, split); err != nil {
		export.Process.Kill()
		export.Wait()
		return nil, err
	}
	if err := export.Wait(); err != nil {
		return nil, fmt.Errorf("docker export: %v: %s", err, stderr.String())
	}
	var layers []string
	if split != nil {
		if err := split.tw.Close(); err != nil {
			return nil, err
		}
		if layers, err = store.add(ctx, split.paths, split.file.Name()); err != nil {
			return nil, err
		}
	}
	fi, err := tmp.Stat()
	if err != nil {
		return nil, err
	}
	// The root is mounted read-only, so only a little slack for metadata.
	size := fi.Size()*5/4 + 128<<20
	_ = os.Remove(out)
	if err := os.WriteFile(out, nil, 0o644); err != nil {
		return nil, err
	}
	if err := os.Truncate(out, size); err != nil {
		return nil, err
	}
	if b, err := exec.CommandContext(ctx, mkfs, "-q", "-F", "-L", "persona-root", "-d", tmp.Name(), out).CombinedOutput(); err != nil {
		os.Remove(out)
		return nil, fmt.Errorf("mkfs.ext4: %v: %s", err, b)
	}
	return layers, os.Chmod(out, 0o644) // read-only images are shared with VM uids
}

// storeSplit collects an export's /nix/store entries, renamed to <path>[/...].
type storeSplit struct {
	tw    *tar.Writer
	file  *os.File
	paths map[string]bool
}

// take writes hdr (and its content) to the store tar if it is under
// /nix/store, and reports whether it did.
func (s *storeSplit) take(name string, hdr *tar.Header, r io.Reader) (bool, error) {
	if s == nil {
		return false, nil
	}
	p := storePathOf(name)
	if p == "" {
		return false, nil
	}
	s.paths[p] = true
	hdr.Name = strings.TrimPrefix(name, storePrefix)
	if hdr.Typeflag == tar.TypeDir {
		hdr.Name += "/"
	}
	if hdr.Typeflag == tar.TypeLink {
		hdr.Linkname = strings.TrimPrefix(path.Clean(strings.TrimPrefix(hdr.Linkname, "./")), storePrefix)
	}
	if err := s.tw.WriteHeader(hdr); err != nil {
		return true, err
	}
	_, err := io.Copy(s.tw, r)
	return true, err
}

func rewriteTar(w io.Writer, r io.Reader, extra map[string]File, edits map[string]Edit, split *storeSplit) error {
	tw := tar.NewWriter(w)
	tr := tar.NewReader(r)
	links := map[string]string{} // symlinks seen so far, e.g. sbin -> usr/sbin (merged /usr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("read export: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if hdr.Typeflag == tar.TypeSymlink {
			links[name] = hdr.Linkname
		}
		if replaced(name, extra, links) {
			continue
		}
		if took, err := split.take(name, hdr, tr); took || err != nil {
			if err != nil {
				return fmt.Errorf("store: %w", err)
			}
			continue
		}
		if edit := editFor(name, edits, links); edit != nil && hdr.Typeflag == tar.TypeReg {
			old, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			data, err := edit(old)
			if err != nil {
				return fmt.Errorf("edit /%s: %w", name, err)
			}
			hdr.Size = int64(len(data))
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if _, err := tw.Write(data); err != nil {
				return err
			}
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	for p, f := range extra {
		hdr := &tar.Header{Name: resolve(p, links), Mode: f.Mode, Size: int64(len(f.Data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return err
		}
	}
	return tw.Close()
}

func editFor(name string, edits map[string]Edit, links map[string]string) Edit {
	for p, e := range edits {
		if resolve(p, links) == name {
			return e
		}
	}
	return nil
}

func replaced(name string, extra map[string]File, links map[string]string) bool {
	for p := range extra {
		if resolve(p, links) == name {
			return true
		}
	}
	return false
}

// resolve maps an absolute path in the image to its tar name, following
// symlinked directories (a file can't be written "through" a symlink).
func resolve(p string, links map[string]string) string {
	parts := strings.Split(strings.Trim(path.Clean(p), "/"), "/")
	cur := ""
	for i, part := range parts {
		next := path.Join(cur, part)
		for hops := 0; i < len(parts)-1 && hops < 16; hops++ {
			target, ok := links[next]
			if !ok {
				break
			}
			if path.IsAbs(target) {
				next = strings.TrimPrefix(path.Clean(target), "/")
			} else {
				next = path.Join(path.Dir(next), target)
			}
		}
		cur = next
	}
	return cur
}

// NewOverlay creates a blank, sparse ext4 disk for a VM's writable layer.
// Lazy initialization keeps it a few MiB on disk, so copying it is cheap.
func NewOverlay(ctx context.Context, path string, sizeGiB int) error {
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return err
	}
	if err := os.Truncate(path, int64(sizeGiB)<<30); err != nil {
		return err
	}
	b, err := exec.CommandContext(ctx, mkfs, "-q", "-F", "-L", "persona-overlay",
		"-E", "lazy_itable_init=1,lazy_journal_init=1", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4: %v: %s", err, b)
	}
	return nil
}

func exitErr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(ee.Stderr))
	}
	return err
}
