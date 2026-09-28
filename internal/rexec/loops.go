package rexec

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// chunkSize bounds the data of one stdin frame.
const chunkSize = 32 << 10

// abandonTimeout bounds the SIGKILL frame sent by Abandon. The frame only
// speeds things up: the server kills the process group anyway when the
// stream ends before the exit status.
const abandonTimeout = 5 * time.Second

// sink is one local output file.
type sink struct {
	f      *os.File
	broken bool // a write failed; further output is dropped
}

// readLoop handles the server's frames until the stream ends.
func (p *Process) readLoop() {
	defer close(p.readerDone)
	defer func() { _ = p.raw.Close() }()
	for {
		var f proto.ExecFrame
		if err := p.conn.Recv(&f); err != nil {
			p.setExit(nil, p.endError(err))
			return
		}
		switch f.Op {
		case proto.ExecStarted:
			p.log.Debug("rexec: remote command started", "pid", f.PID)
		case proto.ExecStdout:
			p.write(&p.stdout, f.Data)
		case proto.ExecStderr:
			p.write(&p.stderr, f.Data)
		case proto.ExecExit:
			if f.Exit == nil {
				p.setExit(nil, errors.New("rexec: exit frame without status"))
				return
			}
			p.setExit(f.Exit, nil)
		default:
			p.setExit(nil, fmt.Errorf("rexec: unexpected exec frame op %d", f.Op))
			return
		}
	}
}

// endError explains why the stream ended; it matters only if no exit
// status arrived.
func (p *Process) endError(err error) error {
	switch {
	case p.abandoned.Load():
		return ErrAbandoned
	case errors.Is(err, io.EOF):
		return ErrNoExit
	default:
		return fmt.Errorf("rexec: exec stream: %w", err)
	}
}

// setExit records the first exit status or failure and stops stdin: input
// that arrives later belongs to whoever reads the shared stdin next.
func (p *Process) setExit(st *proto.ExecStatus, err error) {
	p.exitOnce.Do(func() {
		p.status, p.exitErr = st, err
		close(p.exited)
		p.stopStdin.wake()
	})
}

// write copies output to a local file. EPIPE means the local reader is
// gone, which is no reason to stop the command or its other output.
func (p *Process) write(s *sink, data []byte) {
	if s.f == nil || s.broken || len(data) == 0 {
		return
	}
	if err := writeAll(s.f, data, p.stopOut); err != nil {
		s.broken = true
		if !errors.Is(err, unix.EPIPE) && !errors.Is(err, errStopped) {
			p.log.Warn("rexec: write command output", "err", err)
		}
	}
}

// stdinLoop forwards local stdin until EOF, the command's exit, or
// Abandon.
func (p *Process) stdinLoop() {
	if p.stdin == nil {
		p.sendStdinEOF()
		return
	}
	buf := make([]byte, chunkSize)
	for {
		n, err := readSome(p.stdin, buf, p.stopStdin)
		if errors.Is(err, errStopped) {
			return
		}
		if n > 0 {
			if p.conn.Send(&proto.ExecFrame{Op: proto.ExecStdin, Data: buf[:n]}) != nil {
				return
			}
			continue
		}
		if !errors.Is(err, io.EOF) {
			// EIO from a hung-up terminal and the like end input too.
			p.log.Debug("rexec: read stdin", "err", err)
		}
		p.sendStdinEOF()
		return
	}
}

func (p *Process) sendStdinEOF() {
	if err := p.conn.Send(&proto.ExecFrame{Op: proto.ExecStdinEOF}); err != nil {
		p.log.Debug("rexec: send stdin EOF", "err", err)
	}
}

// abandonLoop carries out Abandon: it asks the server to kill the process
// group and ends the stream, which also ends readLoop.
func (p *Process) abandonLoop() {
	select {
	case <-p.readerDone:
		return
	case <-p.abandon:
	}
	_ = p.raw.SetWriteDeadline(time.Now().Add(abandonTimeout))
	if err := p.conn.Send(&proto.ExecFrame{Op: proto.ExecSignal, Signal: int(unix.SIGKILL)}); err != nil {
		p.log.Debug("rexec: send SIGKILL", "err", err)
	}
	_ = p.raw.Close()
	// Close only half-closes a multiplexed stream; the deadline ends
	// readLoop without waiting for the server.
	_ = p.raw.SetReadDeadline(time.Now())
}
