//go:build linux

package guest

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/johndavisdev/persona/internal/agent"
)

// As PID 1 the agent inherits every orphan, so it must be the only caller of
// wait. Children it starts itself register for their exit status; everything
// else is reaped and discarded. procMu is held across start+register so a
// fast-exiting child can't be reaped before its waiter exists.
var (
	procMu  sync.Mutex
	waiters = map[int]chan syscall.WaitStatus{}
)

func reap() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGCHLD)
	tick := time.NewTicker(time.Second) // in case a coalesced signal is missed
	for {
		select {
		case <-sigs:
		case <-tick.C:
		}
		for {
			var ws syscall.WaitStatus
			procMu.Lock()
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid > 0 {
				if ch, ok := waiters[pid]; ok {
					ch <- ws
					delete(waiters, pid)
				}
			}
			procMu.Unlock()
			if pid <= 0 || err != nil {
				break
			}
		}
	}
}

// account is a guest user a program runs as.
type account struct {
	uid, gid int
	groups   []uint32
	home     string
	name     string
}

func lookupUser(name string) (account, error) {
	if name == "" || name == "root" {
		return account{home: "/root", name: "root"}, nil
	}
	name, _, _ = strings.Cut(name, ":") // "user:group" as in Docker's USER
	u, err := user.Lookup(name)
	if err != nil {
		if _, nerr := strconv.Atoi(name); nerr != nil {
			return account{}, err
		}
		if u, err = user.LookupId(name); err != nil {
			return account{}, err
		}
	}
	a := account{home: u.HomeDir, name: u.Username}
	a.uid, _ = strconv.Atoi(u.Uid)
	a.gid, _ = strconv.Atoi(u.Gid)
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if n, err := strconv.Atoi(g); err == nil {
				a.groups = append(a.groups, uint32(n))
			}
		}
	}
	return a, nil
}

// start launches a program; files are its fds 0, 1, 2. With ctty, fd 0 must
// be a terminal and becomes the controlling terminal of a new session.
func start(e *agent.Exec, files []*os.File, setsid, ctty bool) (int, <-chan syscall.WaitStatus, error) {
	if len(e.Argv) == 0 {
		return 0, nil, fmt.Errorf("empty argv")
	}
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C.UTF-8", "TERM=xterm-256color"}
	attr := &syscall.SysProcAttr{Setsid: setsid || ctty, Setctty: ctty}
	acct, err := lookupUser(e.User)
	if err != nil {
		return 0, nil, err
	}
	if acct.uid != 0 {
		attr.Credential = &syscall.Credential{Uid: uint32(acct.uid), Gid: uint32(acct.gid), Groups: acct.groups}
	}
	home := acct.home
	env = mergeEnv(append(env, "USER="+acct.name, "LOGNAME="+acct.name, "HOME="+home), e.Env)
	dir := e.Dir
	if dir == "" {
		dir = home
	}
	path := e.Argv[0]
	if !bytes.ContainsRune([]byte(path), '/') {
		p, err := lookPath(path, env)
		if err != nil {
			return 0, nil, err
		}
		path = p
	}
	procMu.Lock()
	defer procMu.Unlock()
	p, err := os.StartProcess(path, e.Argv, &os.ProcAttr{Dir: dir, Env: env, Files: files, Sys: attr})
	if err != nil {
		return 0, nil, err
	}
	ch := make(chan syscall.WaitStatus, 1)
	waiters[p.Pid] = ch
	return p.Pid, ch, nil
}

// lookPath finds name on the PATH in env.
func lookPath(name string, env []string) (string, error) {
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found", name)
}

// run executes e and collects its output.
func run(e *agent.Exec) agent.Response {
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	if e.Detach {
		pid, _, err := start(e, []*os.File{devnull, os.Stdout, os.Stdout}, true, false)
		if err != nil {
			return agent.Response{Error: err.Error()}
		}
		return agent.Response{OK: true, Pid: pid}
	}
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	pid, done, err := start(e, []*os.File{inR, outW, errW}, false, false)
	inR.Close()
	outW.Close()
	errW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		return agent.Response{Error: err.Error()}
	}
	go func() { inW.Write(e.Stdin); inW.Close() }()
	var stdout, stderr bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { io.Copy(&stdout, io.LimitReader(outR, 64<<20)); io.Copy(io.Discard, outR); wg.Done() }()
	go func() { io.Copy(&stderr, io.LimitReader(errR, 16<<20)); io.Copy(io.Discard, errR); wg.Done() }()
	ws := <-done
	wg.Wait()
	outR.Close()
	errR.Close()
	exit := ws.ExitStatus()
	if ws.Signaled() {
		exit = 128 + int(ws.Signal())
	}
	return agent.Response{OK: true, Pid: pid, Exit: exit, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
}

// mergeEnv appends extra to base; a later NAME=value replaces an earlier one.
func mergeEnv(base, extra []string) []string {
	idx := map[string]int{}
	var out []string
	for _, kv := range append(base, extra...) {
		k, _, _ := strings.Cut(kv, "=")
		if i, ok := idx[k]; ok {
			out[i] = kv
			continue
		}
		idx[k] = len(out)
		out = append(out, kv)
	}
	return out
}
