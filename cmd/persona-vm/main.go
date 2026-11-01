//go:build linux

// persona-vm is the only program persona runs as root. It is installed at
// /usr/libexec/persona/persona-vm, root-owned, and allowed via a narrow sudoers
// rule. It starts each microVM under Firecracker's jailer (chroot, per-VM uid,
// shared "persona" network namespace), supervises it, and cleans up after it.
//
//	persona-vm start --id ID [--file ro|rw:SRC:NAME]...   prints JSON, returns once the API is up
//	persona-vm stop ID
//	persona-vm list
//	persona-vm collect ID --to DIR NAME...                copy files out of the jail (e.g. a snapshot)
//
// Every path the caller passes must be a regular file or directory owned by
// the invoking user (SUDO_UID), or, for read-only files, root-owned and
// world-readable. It is opened without following symlinks, so the caller
// can't use root to read or write anything they couldn't already.
// The unprivileged caller drives Firecracker through the exposed API socket.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	chrootBase = "/srv/jailer"
	fcBin      = "/usr/libexec/persona/firecracker"
	jailerBin  = "/usr/libexec/persona/jailer"
	ipBin      = "/usr/sbin/ip"
	netnsName  = "persona"
	netnsPath  = "/run/netns/persona"
	bridge     = "fcbr0"
	uidBase    = 1_500_000
	uidCount   = 65_536
)

var (
	stateDir = filepath.Join(chrootBase, "persona-state") // root-only: one JSON file per VM
	userBase = filepath.Join(chrootBase, "persona-users") // <owner uid>/<id>/{api.sock,vsock.sock,console.log}
	idRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	// Sockets Firecracker creates inside the jail that are exposed to the owner.
	exposed = map[string]string{"run/api.sock": "api.sock", "run/vsock.sock": "vsock.sock"}
)

type state struct {
	ID      string `json:"id"`
	Owner   int    `json:"owner"`
	OwnerG  int    `json:"owner_gid"`
	JailUID int    `json:"jail_uid"`
	Tap     string `json:"tap"`
	Pid     int    `json:"pid"` // supervisor
}

// Info is what start and list print for the caller.
type Info struct {
	ID      string `json:"id"`
	Tap     string `json:"tap"`
	API     string `json:"api_sock"`
	Vsock   string `json:"vsock_sock"`
	Console string `json:"console"`
	Root    string `json:"root"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	if os.Geteuid() != 0 {
		fatal(errors.New("must run as root (via sudo)"))
	}
	unix.Umask(0o022)
	var err error
	switch os.Args[1] {
	case "start":
		err = cmdStart(os.Args[2:])
	case "stop":
		err = cmdStop(os.Args[2:])
	case "list":
		err = cmdList()
	case "collect":
		err = cmdCollect(os.Args[2:])
	case "supervise": // internal: the detached per-VM supervisor
		err = cmdSupervise(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: persona-vm start --id ID [--file ro|rw:SRC:NAME]... | stop ID | list | collect ID --to DIR NAME...")
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "persona-vm:", err)
	os.Exit(1)
}

// caller returns the uid/gid of the user who invoked sudo.
func caller() (uid, gid int, err error) {
	u, g := os.Getenv("SUDO_UID"), os.Getenv("SUDO_GID")
	if u == "" {
		return os.Getuid(), os.Getgid(), nil
	}
	if uid, err = strconv.Atoi(u); err != nil {
		return 0, 0, fmt.Errorf("bad SUDO_UID")
	}
	if gid, err = strconv.Atoi(g); err != nil {
		return 0, 0, fmt.Errorf("bad SUDO_GID")
	}
	return uid, gid, nil
}

func jailDir(id string) string   { return filepath.Join(chrootBase, filepath.Base(fcBin), id) }
func rootDir(id string) string   { return filepath.Join(jailDir(id), "root") }
func userDir(s state) string     { return filepath.Join(userBase, strconv.Itoa(s.Owner), s.ID) }
func statePath(id string) string { return filepath.Join(stateDir, id+".json") }

func info(s state) Info {
	d := userDir(s)
	return Info{ID: s.ID, Tap: s.Tap, API: filepath.Join(d, "api.sock"), Vsock: filepath.Join(d, "vsock.sock"),
		Console: filepath.Join(d, "console.log"), Root: rootDir(s.ID)}
}

type fileArg struct{ mode, src, name string }

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func parseFile(v string) (fileArg, error) {
	mode, rest, ok := strings.Cut(v, ":")
	i := strings.LastIndex(rest, ":")
	if !ok || i < 0 || (mode != "ro" && mode != "rw") {
		return fileArg{}, fmt.Errorf("--file %q: want ro|rw:SRC:NAME", v)
	}
	f := fileArg{mode: mode, src: rest[:i], name: rest[i+1:]}
	if !nameRE.MatchString(f.name) || exposedName(f.name) {
		return fileArg{}, fmt.Errorf("--file %q: invalid NAME", v)
	}
	if !filepath.IsAbs(f.src) {
		return fileArg{}, fmt.Errorf("--file %q: SRC must be absolute", v)
	}
	return f, nil
}

func exposedName(n string) bool { return n == "run" || n == "dev" || n == "firecracker" }

func cmdStart(args []string) error {
	owner, ownerG, err := caller()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	id := fs.String("id", "", "VM id")
	var files multiFlag
	fs.Var(&files, "file", "ro|rw:SRC:NAME (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !idRE.MatchString(*id) {
		return fmt.Errorf("invalid --id %q", *id)
	}
	var fargs []fileArg
	for _, v := range files {
		f, err := parseFile(v)
		if err != nil {
			return err
		}
		fargs = append(fargs, f)
	}

	for _, d := range []string{chrootBase, stateDir, userBase} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	_ = os.Chmod(stateDir, 0o700)
	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()

	if old, err := readState(*id); err == nil {
		if alive(old) {
			return fmt.Errorf("%s is already running", *id)
		}
		cleanup(old)
	}
	used, err := usedUIDs()
	if err != nil {
		return err
	}
	s := state{ID: *id, Owner: owner, OwnerG: ownerG, JailUID: allocUID(*id, used), Tap: TapName(*id)}
	if err := os.RemoveAll(jailDir(s.ID)); err != nil {
		return err
	}
	if err := writeState(s); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			cleanup(s)
		}
	}()

	root := rootDir(s.ID)
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
		return err
	}
	if err := os.Chown(filepath.Join(root, "run"), s.JailUID, s.JailUID); err != nil {
		return err
	}
	for _, f := range fargs {
		if err := place(f, root, owner, s.JailUID); err != nil {
			return err
		}
	}
	ud := userDir(s)
	if err := os.MkdirAll(filepath.Dir(ud), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(ud, 0o755); err != nil {
		return err
	}
	con, err := os.OpenFile(filepath.Join(ud, "console.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := con.Chown(owner, ownerG); err != nil {
		return err
	}
	con.Close()

	if err := run(ipBin, "-n", netnsName, "tuntap", "add", "dev", s.Tap, "mode", "tap",
		"user", strconv.Itoa(s.JailUID), "group", strconv.Itoa(s.JailUID)); err != nil {
		return err
	}
	if err := run(ipBin, "-n", netnsName, "link", "set", s.Tap, "master", bridge, "up"); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	sup := exec.Command(self, "supervise", s.ID)
	sup.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	sup.Stdin, sup.Stdout, sup.Stderr = devnull, devnull, devnull
	sup.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := sup.Start(); err != nil {
		return err
	}
	s.Pid = sup.Process.Pid
	if err := writeState(s); err != nil {
		sup.Process.Kill()
		return err
	}
	_ = sup.Process.Release()
	ok = true
	unlock()

	// Return once the API socket is exposed, or report why the VM died.
	in := info(s)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if fi, err := os.Lstat(in.API); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return json.NewEncoder(os.Stdout).Encode(in)
		}
		if _, err := readState(s.ID); err != nil || time.Now().After(deadline) {
			log, _ := os.ReadFile(in.Console)
			return fmt.Errorf("%s failed to start\n%s", s.ID, log)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// place puts one caller-owned file into the jail: read-only files are hard
// linked (shared page cache, e.g. a base rootfs or snapshot memory), writable
// ones are copied sparsely and owned by the jail uid.
func place(f fileArg, root string, owner, jailUID int) error {
	open := openOwned
	if f.mode == "ro" {
		open = func(p string, o int, _ bool) (*os.File, error) { return openReadable(p, o) }
	}
	src, err := open(f.src, owner, false)
	if err != nil {
		return err
	}
	defer src.Close()
	dst := filepath.Join(root, f.name)
	if f.mode == "ro" {
		fi, _ := src.Stat()
		if fi.Mode().Perm()&0o004 == 0 {
			return fmt.Errorf("%s: read-only files must be world-readable (the VM runs as its own uid)", f.src)
		}
		err := unix.Linkat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", src.Fd()), unix.AT_FDCWD, dst, unix.AT_SYMLINK_FOLLOW)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("link %s: %w", f.src, err)
		}
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := SparseCopy(out, src); err != nil {
		return fmt.Errorf("copy %s: %w", f.src, err)
	}
	if f.mode == "ro" {
		return out.Chmod(0o444)
	}
	return out.Chown(jailUID, jailUID)
}

func cmdSupervise(args []string) error {
	if len(args) != 1 {
		return errors.New("supervise ID")
	}
	s, err := readState(args[0])
	if err != nil {
		return err
	}
	defer cleanup(s)
	in := info(s)
	con, err := os.OpenFile(in.Console, os.O_WRONLY|os.O_APPEND|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer con.Close()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	uid := strconv.Itoa(s.JailUID)
	cmd := exec.Command(jailerBin, "--id", s.ID, "--exec-file", fcBin, "--uid", uid, "--gid", uid,
		"--chroot-base-dir", chrootBase, "--netns", netnsPath,
		"--", "--api-sock", "/run/api.sock")
	cmd.Env = []string{}
	cmd.Stdout, cmd.Stderr = con, con
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	linked := map[string]uint64{}
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	slow := time.After(5 * time.Second)
	for {
		select {
		case err := <-done:
			fmt.Fprintf(con, "persona-vm: firecracker exited: %v\n", err)
			return nil
		case <-sigs:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			return nil
		case <-slow:
			tick.Reset(50 * time.Millisecond)
		case <-tick.C:
			for inJail, name := range exposed {
				exposeSocket(filepath.Join(rootDir(s.ID), inJail), filepath.Join(userDir(s), name), s, linked)
			}
		}
	}
}

// exposeSocket hard links a socket Firecracker created in the jail into the
// owner's directory, owned by them. Re-links if Firecracker recreated it.
func exposeSocket(src, dst string, s state, linked map[string]uint64) {
	fi, err := os.Lstat(src)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return
	}
	ino := fi.Sys().(*syscall.Stat_t).Ino
	if linked[dst] == ino {
		return
	}
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err != nil {
		return
	}
	if fi, err := os.Lstat(dst); err != nil || fi.Mode()&os.ModeSocket == 0 {
		_ = os.Remove(dst)
		return
	}
	_ = os.Lchown(dst, s.Owner, s.OwnerG)
	_ = os.Chmod(dst, 0o600)
	linked[dst] = ino
}

func cmdStop(args []string) error {
	if len(args) != 1 {
		return errors.New("stop ID")
	}
	s, err := ownedState(args[0])
	if err != nil {
		return err
	}
	if !alive(s) {
		unlock, err := lock()
		if err != nil {
			return err
		}
		defer unlock()
		cleanup(s)
		return nil
	}
	if err := syscall.Kill(s.Pid, syscall.SIGTERM); err != nil {
		return err
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(statePath(s.ID)); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	return fmt.Errorf("%s did not stop", s.ID)
}

func cmdList() error {
	owner, _, err := caller()
	if err != nil {
		return err
	}
	states, err := allStates()
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for _, s := range states {
		if s.Owner == owner && alive(s) {
			if err := enc.Encode(info(s)); err != nil {
				return err
			}
		}
	}
	return nil
}

func cmdCollect(args []string) error {
	if len(args) < 1 {
		return errors.New("collect ID --to DIR NAME...")
	}
	s, err := ownedState(args[0])
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	to := fs.String("to", "", "destination directory, owned by you")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	dir, err := openOwned(*to, s.Owner, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	for _, name := range fs.Args() {
		if !nameRE.MatchString(name) || exposedName(name) {
			return fmt.Errorf("invalid NAME %q", name)
		}
		src, err := os.OpenFile(filepath.Join(rootDir(s.ID), name), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		if fi, err := src.Stat(); err != nil || !fi.Mode().IsRegular() {
			src.Close()
			return fmt.Errorf("%s: not a regular file", name)
		}
		fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
		if err != nil {
			src.Close()
			return fmt.Errorf("%s/%s: %w", *to, name, err)
		}
		out := os.NewFile(uintptr(fd), name)
		err = SparseCopy(out, src)
		if err == nil {
			err = out.Chown(s.Owner, s.OwnerG)
		}
		out.Close()
		src.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// --- state ---

func lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(stateDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	var once bool
	return func() {
		if !once {
			once = true
			f.Close()
		}
	}, nil
}

func readState(id string) (state, error) {
	var s state
	b, err := os.ReadFile(statePath(id))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func writeState(s state) error {
	b, _ := json.Marshal(s)
	tmp := statePath(s.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(s.ID))
}

func ownedState(id string) (state, error) {
	owner, _, err := caller()
	if err != nil {
		return state{}, err
	}
	if !idRE.MatchString(id) {
		return state{}, fmt.Errorf("invalid id %q", id)
	}
	s, err := readState(id)
	if err != nil {
		return s, fmt.Errorf("%s: no such VM", id)
	}
	if s.Owner != owner && owner != 0 {
		return s, fmt.Errorf("%s: no such VM", id)
	}
	return s, nil
}

func allStates() ([]state, error) {
	paths, err := filepath.Glob(filepath.Join(stateDir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []state
	for _, p := range paths {
		if s, err := readState(strings.TrimSuffix(filepath.Base(p), ".json")); err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

func usedUIDs() (map[int]bool, error) {
	states, err := allStates()
	used := map[int]bool{}
	for _, s := range states {
		used[s.JailUID] = true
	}
	return used, err
}

// alive reports whether s's supervisor process is still running.
func alive(s state) bool {
	if s.Pid <= 0 {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", s.Pid))
	return err == nil && strings.HasSuffix(string(cmdline), "\x00supervise\x00"+s.ID+"\x00")
}

func cleanup(s state) {
	_ = run(ipBin, "-n", netnsName, "link", "del", s.Tap)
	_ = os.RemoveAll(jailDir(s.ID))
	_ = os.RemoveAll(userDir(s))
	_ = os.Remove(statePath(s.ID))
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = []string{}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", filepath.Base(name), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
