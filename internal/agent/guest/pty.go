//go:build linux

package guest

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/johndavisdev/persona/internal/agent"
	"golang.org/x/sys/unix"
)

var (
	ptys        sync.Map // session id -> pty master *os.File
	nextSession atomic.Int64
)

func openPty() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("unlockpt: %w", err)
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("ptsname: %w", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func setWinsize(f *os.File, rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		rows, cols = 50, 200
	}
	return unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
}

// ptyStream runs e on a new terminal and relays it over conn until the
// program exits or the host hangs up.
func ptyStream(conn *os.File, e *agent.Exec) {
	master, slave, err := openPty()
	if err != nil {
		fail(conn, err)
		return
	}
	defer master.Close()
	acct, err := lookupUser(e.User)
	if err == nil {
		err = slave.Chown(acct.uid, acct.gid)
	}
	if err == nil {
		err = setWinsize(master, e.Rows, e.Cols)
	}
	if err != nil {
		slave.Close()
		fail(conn, err)
		return
	}
	id := int(nextSession.Add(1))
	ptys.Store(id, master)
	defer ptys.Delete(id)
	_, done, err := start(e, []*os.File{slave, slave, slave}, true, true)
	slave.Close()
	if err != nil {
		fail(conn, err)
		return
	}
	reply(conn, agent.Response{OK: true, Session: id})
	go func() {
		io.Copy(master, conn) // host keystrokes; ends when the host hangs up
		// Hang up the terminal so the program sees SIGHUP, as with ssh.
		master.Close()
	}()
	io.Copy(conn, master) // returns with EIO once the program and its children close the terminal
	<-done
	log.Printf("pty session %d (%v) ended", id, e.Argv)
}

func resize(w agent.Winsize) error {
	m, ok := ptys.Load(w.Session)
	if !ok {
		return fmt.Errorf("no pty session %d", w.Session)
	}
	return setWinsize(m.(*os.File), w.Rows, w.Cols)
}
