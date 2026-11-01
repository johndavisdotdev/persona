//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// TapName derives a tap device name (max 15 chars) from a VM id.
func TapName(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "fc" + hex.EncodeToString(sum[:])[:12]
}

// allocUID picks a jail uid for id that no other VM is using.
func allocUID(id string, used map[int]bool) int {
	sum := sha256.Sum256([]byte(id))
	start := int(binary.BigEndian.Uint32(sum[:4]) % uidCount)
	for i := 0; i < uidCount; i++ {
		uid := uidBase + (start+i)%uidCount
		if !used[uid] {
			return uid
		}
	}
	panic("jail uid range exhausted")
}

// openOwned opens path without following a symlink in its last component and
// checks that it is a regular file (or a directory, if dir) owned by owner.
// Root (owner 0) may open anything.
func openOwned(path string, owner int, dir bool) (*os.File, error) {
	return openChecked(path, owner, dir, false)
}

// openReadable is openOwned that also accepts root-owned, world-readable
// files (e.g. a system-wide guest kernel), which are only ever shared
// read-only: the caller could read them anyway.
func openReadable(path string, owner int) (*os.File, error) {
	return openChecked(path, owner, false, true)
}

func openChecked(path string, owner int, dir, rootReadable bool) (*os.File, error) {
	flags := os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if dir {
		flags |= unix.O_DIRECTORY
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	switch st := fi.Sys().(*syscall.Stat_t); {
	case dir != fi.IsDir() || (!dir && !fi.Mode().IsRegular()):
		err = fmt.Errorf("%s: wrong file type", path)
	case owner != 0 && int(st.Uid) != owner && !(rootReadable && st.Uid == 0 && fi.Mode().Perm()&0o004 != 0):
		err = fmt.Errorf("%s: not owned by you", path)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// SparseCopy copies src to dst, skipping holes so sparse disk images stay sparse.
func SparseCopy(dst, src *os.File) error {
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	sfd := int(src.Fd())
	for off := int64(0); off < size; {
		data, err := unix.Seek(sfd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		} else if err != nil {
			return err
		}
		hole, err := unix.Seek(sfd, data, unix.SEEK_HOLE)
		if err != nil {
			return err
		}
		if err := copyRange(dst, src, data, hole); err != nil {
			return err
		}
		off = hole
	}
	return dst.Truncate(size)
}

// copyRange copies [from, to) at the same offsets, in the kernel when the
// filesystems allow it.
func copyRange(dst, src *os.File, from, to int64) error {
	roff, woff := from, from
	for roff < to {
		n, err := unix.CopyFileRange(int(src.Fd()), &roff, int(dst.Fd()), &woff, int(min(to-roff, 1<<30)), 0)
		if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
			break // e.g. across filesystems: fall back to read/write below
		} else if err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("short copy at %d", roff)
		}
	}
	buf := make([]byte, 1<<20)
	for roff < to {
		n, err := src.ReadAt(buf[:min(to-roff, int64(len(buf)))], roff)
		if n > 0 {
			if _, werr := dst.WriteAt(buf[:n], roff); werr != nil {
				return werr
			}
			roff += int64(n)
		}
		if err != nil && roff < to {
			return err
		}
	}
	return nil
}
