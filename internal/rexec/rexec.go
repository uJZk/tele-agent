// Package rexec runs commands on the target host for session main, one
// exec stream per command (docs/exec.md).
//
// A command's stdio are file descriptors that session main received from a
// shim (docs/exec.md "shim 与会话主进程"). Their open file descriptions are shared
// with Claude and possibly other processes, so rexec never changes their
// flags: it waits with poll(2), reads no more than is available, and uses
// non-blocking socket calls, instead of setting O_NONBLOCK. This also lets
// it stop reading stdin the moment the command exits, leaving later input
// to whoever reads it next.
package rexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/ujzk/tele-agent/internal/proto"
)

// maxSignal is the highest Linux signal number (SIGRTMAX).
const maxSignal = 64

var (
	// ErrFinished is returned by Signal after the command exited.
	ErrFinished = errors.New("rexec: command already exited")
	// ErrAbandoned is returned by Wait and Signal after Abandon.
	ErrAbandoned = errors.New("rexec: command abandoned")
	// ErrNoExit is returned by Wait when the stream ended without an exit
	// status, for example because the server shut down.
	ErrNoExit = errors.New("rexec: exec stream ended without exit status")
)

// Opener opens multiplexed streams to tele server; *mux.Session
// implements it.
type Opener interface {
	Open(kind proto.StreamKind) (net.Conn, error)
}

// Barrier waits until the file-change events up to a sequence number
// have been applied locally (docs/exec.md "exec 屏障").
type Barrier interface {
	WaitApplied(ctx context.Context, seq uint64) error
}

// Client starts remote commands.
type Client struct {
	Opener Opener
	// Barrier is waited on before Wait reports an exit; nil means none.
	Barrier Barrier
	// Logger receives diagnostics; nil discards them. It must not write
	// to any shim's stdio (docs/coding-standards.md "日志与输出").
	Logger *slog.Logger
}

// Command describes a remote command.
type Command struct {
	// Argv is the command; an Argv[0] without a slash is looked up in the
	// target user's login PATH.
	Argv []string
	// Dir is the working directory on the target.
	Dir string
	// Env holds KEY=VALUE entries applied over the target's base
	// environment.
	Env []string
	// TTY requests a pseudo-terminal of this size; its output arrives on
	// Stdout.
	TTY *proto.TTYSize
	// Stdin, Stdout and Stderr are the local ends. A nil Stdin is empty
	// input; output to a nil Stdout or Stderr is discarded. The files are
	// borrowed: the caller closes them after Done.
	Stdin, Stdout, Stderr *os.File
	// Scratch holds files to write into the scratch areas first; the
	// server has written them once Started is closed. Their encoded size
	// must stay within ScratchBudget.
	Scratch []proto.ScratchFile
}

// Bounds of the CBOR encoding of an ExecStart besides its strings: a
// string or array header takes at most 9 bytes, and the map header, keys
// and TTY field fit in startOverhead.
const (
	headerMax     = 9
	startOverhead = 64
)

// ScratchBudget returns how many bytes of encoded scratch files fit into
// the ExecStart of cmd next to its argv, directory and environment
// (scratch.Mapper.Uploads takes it as its budget). proto.MaxExecStart
// leaves room for proto.ScratchTotalMax next to any command line a shim
// can send, so only a larger one reduces it. The size of the rest is
// bounded from the string lengths rather than measured, which would
// encode a command line of up to proto.MaxShimRequest twice.
func ScratchBudget(cmd Command) int {
	n := startOverhead + 3*headerMax + len(cmd.Dir)
	for _, s := range cmd.Argv {
		n += headerMax + len(s)
	}
	for _, s := range cmd.Env {
		n += headerMax + len(s)
	}
	return max(0, proto.MaxExecStart-n)
}

// Result is how a remote command ended.
type Result struct {
	// Code is the exit code, when Signal is 0.
	Code int
	// Signal is the signal that killed the command, or 0.
	Signal int
	// Scratch lists the scratch files the command changed.
	Scratch []proto.ScratchFile
	// StartErr is set when the command could not be started; the other
	// fields are then meaningless.
	StartErr *proto.Error
}

// Process is a started remote command.
type Process struct {
	log     *slog.Logger
	barrier Barrier
	raw     net.Conn
	conn    *proto.Conn // every frame write goes through its Send

	stdin          *os.File
	stdout, stderr sink // owned by readLoop

	stopStdin *wakeFD // signalled when the command exited or was abandoned
	stopOut   *wakeFD // signalled when the command was abandoned

	// credit is the stdin window left: readLoop adds what the server
	// acknowledges, stdinLoop spends what it sends.
	credit *stdinCredit

	// Frames for sendLoop, which writes every frame. Control frames go
	// first, so that a signal waits for at most the stdin frame being
	// written, never for the input behind it.
	control chan sendRequest
	input   chan sendRequest

	started    chan struct{} // closed by readLoop on ExecStarted
	sawStarted bool          // owned by readLoop

	exitOnce sync.Once
	exited   chan struct{}     // closed by setExit
	status   *proto.ExecStatus // written before exited is closed
	exitErr  error             // written before exited is closed

	abandonOnce sync.Once
	abandoned   atomic.Bool
	abandon     chan struct{} // closed by Abandon

	readerDone chan struct{} // closed when readLoop returns
	done       chan struct{} // closed after every goroutine of the process ended
}

// Start opens an exec stream and sends the command. ctx bounds only the
// start itself; use Abandon to give up on a running command. The result,
// including a failure to start the program, is reported by Wait.
func (c *Client) Start(ctx context.Context, cmd Command) (*Process, error) {
	if len(cmd.Argv) == 0 {
		return nil, errors.New("rexec: empty command")
	}
	log := c.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	stopStdin, err := newWakeFD()
	if err != nil {
		return nil, err
	}
	stopOut, err := newWakeFD()
	if err != nil {
		stopStdin.close()
		return nil, err
	}
	raw, err := c.startStream(ctx, &cmd)
	if err != nil {
		stopStdin.close()
		stopOut.close()
		return nil, err
	}

	p := &Process{
		log:        log,
		barrier:    c.Barrier,
		raw:        raw,
		conn:       proto.NewConn(raw, proto.MaxDataFrame),
		stdin:      cmd.Stdin,
		stdout:     sink{f: cmd.Stdout},
		stderr:     sink{f: cmd.Stderr},
		stopStdin:  stopStdin,
		stopOut:    stopOut,
		credit:     newStdinCredit(),
		control:    make(chan sendRequest),
		input:      make(chan sendRequest),
		started:    make(chan struct{}),
		exited:     make(chan struct{}),
		abandon:    make(chan struct{}),
		readerDone: make(chan struct{}),
		done:       make(chan struct{}),
	}
	var wg sync.WaitGroup
	wg.Go(p.readLoop)
	wg.Go(p.sendLoop)
	wg.Go(p.stdinLoop)
	wg.Go(p.abandonLoop)
	go func() {
		wg.Wait()
		p.stopStdin.close()
		p.stopOut.close()
		close(p.done)
	}()
	return p, nil
}

func (c *Client) startStream(ctx context.Context, cmd *Command) (net.Conn, error) {
	raw, err := c.Opener.Open(proto.StreamExec)
	if err != nil {
		return nil, fmt.Errorf("rexec: %w", err)
	}
	// ExecStart may carry megabytes of scratch files; closing the stream
	// interrupts a send stalled by flow control.
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	err = proto.WriteFrameLimit(raw, &proto.ExecStart{
		Argv:    cmd.Argv,
		Dir:     cmd.Dir,
		Env:     cmd.Env,
		TTY:     cmd.TTY,
		Scratch: cmd.Scratch,
	}, proto.MaxExecStart)
	if !stop() {
		err = ctx.Err()
	}
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("rexec: send exec start: %w", err)
	}
	return raw, nil
}

// Signal delivers sig to the command's process group. It returns once the
// request was written to the stream, or when ctx is done; a request that
// was being written when ctx ended may still arrive.
//
// Signals travel behind stdin data, but never wait for the command to
// read it: the stdin window bounds what the server holds for the command
// (proto.ExecStdinWindow), so it always reads on to the signal.
func (p *Process) Signal(ctx context.Context, sig int) error {
	if sig < 1 || sig > maxSignal {
		return fmt.Errorf("rexec: invalid signal %d", sig)
	}
	if err := p.checkRunning(); err != nil {
		return err
	}
	if err := p.send(ctx, p.control, &proto.ExecFrame{Op: proto.ExecSignal, Signal: sig}); err != nil {
		return fmt.Errorf("rexec: send signal: %w", err)
	}
	return nil
}

// Resize changes the size of the command's pseudo-terminal. It waits like
// Signal.
func (p *Process) Resize(ctx context.Context, size proto.TTYSize) error {
	if err := p.checkRunning(); err != nil {
		return err
	}
	if err := p.send(ctx, p.control, &proto.ExecFrame{Op: proto.ExecResize, TTY: &size}); err != nil {
		return fmt.Errorf("rexec: send resize: %w", err)
	}
	return nil
}

func (p *Process) checkRunning() error {
	if p.abandoned.Load() {
		return ErrAbandoned
	}
	select {
	case <-p.exited:
		return ErrFinished
	default:
		return nil
	}
}

// Wait waits for the command's main process to exit and for the exec
// barrier, so that file changes made by the command are visible locally
// once it returns. By then the output the main process wrote before
// exiting has also been written to Stdout and Stderr, because the server
// sends the exit status after it (on a pty, as far as the kernel had
// passed it to the master). Output of background children may still
// arrive until Done is closed. Cancelling ctx stops waiting but leaves the
// command running.
func (p *Process) Wait(ctx context.Context) (Result, error) {
	select {
	case <-p.exited:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	if p.exitErr != nil {
		return Result{}, p.exitErr
	}
	st := p.status
	// A command that did not start changed nothing: its WatchSeq is 0.
	if p.barrier != nil && st.WatchSeq != 0 {
		if err := p.barrier.WaitApplied(ctx, st.WatchSeq); err != nil {
			return Result{}, fmt.Errorf("rexec: exec barrier: %w", err)
		}
	}
	return Result{Code: st.Code, Signal: st.Signal, Scratch: st.Scratch, StartErr: st.Err}, nil
}

// Started is closed when the server reports that the command started,
// which it does after writing Command.Scratch. It stays open for a command
// that failed to start (see Result.StartErr).
func (p *Process) Started() <-chan struct{} {
	return p.started
}

// Done is closed when the stream has ended and all output was written, or
// the process was abandoned; the caller may then close the stdio files.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// Abandon gives up on the command because its shim is gone: the remote
// process group is killed if the command is still running, and further
// output is dropped. It does not wait; see Done. Abandon is idempotent.
func (p *Process) Abandon() {
	p.abandonOnce.Do(func() {
		p.abandoned.Store(true)
		p.stopStdin.wake()
		p.stopOut.wake()
		close(p.abandon)
	})
}
