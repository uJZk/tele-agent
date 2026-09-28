package execsvc

import (
	"errors"
	"io"
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// chunkSize bounds the data of one output frame.
const chunkSize = 32 << 10

// pump forwards one output pipe (or the pty master) to the client.
//
// It counts the bytes it read and sent so that flush can wait until
// everything the main process wrote before exiting has been sent: the
// client lets its shim exit on ExecExit, and Claude may read a background
// task's output file as soon as the shim is gone.
type pump struct {
	op  proto.ExecOp
	f   *os.File
	pty bool // EIO means EOF: Linux reports a pty whose slave side is closed so

	mu   sync.Mutex
	cond *sync.Cond // signalled when sent or done changes
	read uint64     // guarded by mu; bytes read from f
	sent uint64     // guarded by mu; bytes whose frame was sent
	done bool       // guarded by mu; run returned
}

func newPump(f *os.File, op proto.ExecOp, pty bool) *pump {
	p := &pump{op: op, f: f, pty: pty}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// run forwards output until EOF, a read error, a failed send, or close.
func (p *pump) run(conn *proto.Conn) {
	defer p.finish()
	rc, err := p.f.SyscallConn()
	if err != nil {
		return
	}
	buf := make([]byte, chunkSize)
	for {
		n, err := p.readChunk(rc, buf)
		if n > 0 {
			if conn.Send(&proto.ExecFrame{Op: p.op, Data: buf[:n]}) != nil {
				return
			}
			p.mu.Lock()
			p.sent += uint64(n)
			p.cond.Broadcast()
			p.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// readChunk reads once, waiting in the runtime poller while the pipe is
// empty. The read itself happens under mu so that flush sees read and the
// pipe's remaining bytes consistently; it never blocks because Go keeps
// the parent's pipe ends and the pty master non-blocking.
func (p *pump) readChunk(rc syscall.RawConn, buf []byte) (int, error) {
	var (
		n    int
		rerr error
	)
	err := rc.Read(func(fd uintptr) bool {
		for {
			p.mu.Lock()
			n, rerr = unix.Read(int(fd), buf)
			if n > 0 {
				p.read += uint64(n)
			}
			p.mu.Unlock()
			if !errors.Is(rerr, unix.EINTR) {
				return !errors.Is(rerr, unix.EAGAIN)
			}
		}
	})
	switch {
	case err != nil:
		return 0, err
	case n > 0:
		return n, nil
	case rerr == nil:
		return 0, io.EOF
	case p.pty && errors.Is(rerr, unix.EIO):
		return 0, io.EOF
	default:
		return 0, rerr
	}
}

func (p *pump) finish() {
	_ = p.f.Close()
	p.mu.Lock()
	p.done = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// flush waits until every byte that is in the pipe now has been sent, or
// the pump stopped. For a pty this is best effort: the kernel moves slave
// output to the master asynchronously.
func (p *pump) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	target := p.read
	if rc, err := p.f.SyscallConn(); err == nil {
		// FIONREAD (TIOCINQ on Linux, for pipes and ttys alike) is a
		// non-blocking query; holding mu keeps it consistent with read.
		_ = rc.Control(func(fd uintptr) {
			if avail, err := unix.IoctlGetInt(int(fd), unix.TIOCINQ); err == nil && avail > 0 {
				target += uint64(avail)
			}
		})
	}
	for !p.done && p.sent < target {
		p.cond.Wait()
	}
}

// close stops run, which then closes the file.
func (p *pump) close() {
	_ = p.f.Close()
}
