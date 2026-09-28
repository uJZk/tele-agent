package rexec

import (
	"context"
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

// errStreamEnded is returned by send when the stream ended first.
var errStreamEnded = errors.New("rexec: exec stream ended")

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
			if !p.sawStarted {
				p.sawStarted = true
				close(p.started)
			}
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
			if p.sendInput(&proto.ExecFrame{Op: proto.ExecStdin, Data: buf[:n]}) != nil {
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
	if err := p.sendInput(&proto.ExecFrame{Op: proto.ExecStdinEOF}); err != nil {
		p.log.Debug("rexec: send stdin EOF", "err", err)
	}
}

// sendInput sends a stdin frame once sendLoop has no control frame to
// send. It waits as long as the frame takes, since stdinLoop reads no more
// input meanwhile; the end of the stream or Abandon ends the wait.
func (p *Process) sendInput(f *proto.ExecFrame) error {
	return p.send(context.Background(), p.input, f)
}

// sendRequest asks sendLoop to write one frame.
type sendRequest struct {
	f    *proto.ExecFrame
	done chan error // buffered; receives the result of the write
}

// send has sendLoop write f and waits for the result, until ctx is done
// or the stream ends. A frame that sendLoop already took is written even
// if send gave up.
func (p *Process) send(ctx context.Context, ch chan<- sendRequest, f *proto.ExecFrame) error {
	r := sendRequest{f: f, done: make(chan error, 1)}
	select {
	case ch <- r:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.readerDone:
		return errStreamEnded
	case <-p.abandon:
		return ErrAbandoned
	}
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendLoop writes the frames handed to send, control frames first, until
// the stream ends or the command is abandoned. Both close the stream,
// which also ends a write in progress.
func (p *Process) sendLoop() {
	for {
		var r sendRequest
		select {
		case r = <-p.control:
		default:
			select {
			case r = <-p.control:
			case r = <-p.input:
			case <-p.readerDone:
				return
			case <-p.abandon:
				return
			}
		}
		r.done <- p.conn.Send(r.f)
	}
}

// abandonLoop carries out Abandon: it ends the stream, which also ends
// readLoop. The server kills the process group when the stream ends
// before the exit status (docs/exec.md section 2). The FIN is sent at
// once, even while a stdin frame waits for the stream window.
func (p *Process) abandonLoop() {
	select {
	case <-p.readerDone:
		return
	case <-p.abandon:
	}
	_ = p.raw.Close()
	// Close only half-closes a multiplexed stream; the deadline ends
	// readLoop without waiting for the server.
	_ = p.raw.SetReadDeadline(time.Now())
}
