package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// Terminal and job streams carry frames both ways after the header line:
// one type byte, a big-endian uint32 length, then the payload.
const (
	frameData   = 0 // terminal bytes
	frameResize = 1 // client → server: {"cols":N,"rows":N}
	frameExit   = 2 // server → client: {"code":N,"error":"..."}, then the stream ends
	maxFrame    = 1 << 20
)

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *frameWriter) write(typ byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := f.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := f.w.Write(payload)
	return err
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, errors.New("frame too large")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return hdr[0], b, err
}

func writeExit(w io.Writer, code int, msg string) {
	b, _ := json.Marshal(map[string]any{"code": code, "error": msg})
	_ = (&frameWriter{w: w}).write(frameExit, b)
}

// runPTY runs cmd under a pty of cols x rows and connects it to the stream.
// With interactive, the client's keystrokes go to it; otherwise its input is
// ignored (a job's output is only watched). When the stream ends first, the
// command is stopped (a terminal detaches: the spawn's session lives on).
func runPTY(ctx context.Context, rw io.ReadWriter, cmd *exec.Cmd, cols, rows uint16, interactive bool) {
	if cols == 0 || rows == 0 {
		cols, rows = 120, 32
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "TERM=xterm-256color", "COLORTERM=truecolor")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	fw := &frameWriter{w: rw}
	if err != nil {
		writeExit(rw, -1, err.Error())
		return
	}
	defer f.Close()

	done := make(chan struct{})
	go func() { // stream → pty
		for {
			typ, b, err := readFrame(rw)
			if err != nil {
				// The page closed the terminal: stop the command (for an
				// attach, that only detaches).
				select {
				case <-done:
				default:
					_ = cmd.Process.Signal(syscall.SIGHUP)
				}
				return
			}
			switch typ {
			case frameData:
				if interactive {
					_, _ = f.Write(b)
				}
			case frameResize:
				var sz struct{ Cols, Rows uint16 }
				if json.Unmarshal(b, &sz) == nil && sz.Cols > 0 && sz.Rows > 0 {
					_ = pty.Setsize(f, &pty.Winsize{Cols: sz.Cols, Rows: sz.Rows})
				}
			}
		}
	}()
	go func() { // stop with the server
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGHUP)
		case <-done:
		}
	}()

	buf := make([]byte, 32<<10)
	for { // pty → stream
		n, err := f.Read(buf)
		if n > 0 {
			if werr := fw.write(frameData, buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break // EIO once the command (and everything on the pty) exits
		}
	}
	werr := cmd.Wait()
	close(done)
	code := 0
	msg := ""
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		code = ee.ExitCode()
	} else if werr != nil {
		code, msg = -1, werr.Error()
	}
	b, _ := json.Marshal(map[string]any{"code": code, "error": msg})
	_ = fw.write(frameExit, b)
}
