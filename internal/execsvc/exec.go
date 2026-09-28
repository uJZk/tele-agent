package execsvc

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// maxSignal is the highest Linux signal number (SIGRTMAX).
const maxSignal = 64

// execution is one exec stream and the command it runs.
type execution struct {
	svc  *Service
	raw  net.Conn
	conn *proto.Conn // every frame write goes through its Send
	log  *slog.Logger

	// Set by start before the goroutines that use them exist.
	proc  *os.Process
	stdin *os.File // the command's stdin: a pipe's write end or the pty master
	pty   bool

	// stdinQ carries stdin data from readLoop to stdinLoop, so that
	// readLoop never waits for the command to read its input.
	stdinQ *stdinQueue

	// finishing is set when the server itself ends the stream, so that
	// the reader's failure is not taken for the client going away.
	finishing atomic.Bool

	// mainDone is closed once the main process exited and its exit status
	// was sent, or when it turned out that no command will run.
	mainDone     chan struct{}
	mainDoneOnce sync.Once

	mu    sync.Mutex
	pgid  int     // guarded by mu; 0 until the command started
	pumps []*pump // guarded by mu; set when the command started
	// exited is set while the main process is a zombie, before it is
	// reaped. Until then its pid, which is also the process group id,
	// cannot be reused, so signalling -pgid is safe exactly while exited
	// is false.
	exited  bool // guarded by mu
	aborted bool // guarded by mu
}

func newExecution(s *Service, c net.Conn) *execution {
	return &execution{
		svc:      s,
		raw:      c,
		conn:     proto.NewConn(c, proto.MaxDataFrame),
		log:      s.log,
		stdinQ:   newStdinQueue(),
		mainDone: make(chan struct{}),
	}
}

func (e *execution) markMainDone() {
	e.mainDoneOnce.Do(func() { close(e.mainDone) })
}

func (e *execution) run() error {
	defer e.markMainDone()
	var start proto.ExecStart
	if err := e.conn.Recv(&start); err != nil {
		_ = e.raw.Close()
		return fmt.Errorf("execsvc: read exec start: %w", err)
	}
	if err := checkStart(&start); err != nil {
		e.fail(&proto.Error{Errno: uint32(unix.EINVAL), Msg: err.Error()})
		return fmt.Errorf("execsvc: %w", err)
	}
	roots := e.svc.openAreas()
	e.svc.writeScratch(roots, start.Scratch)
	// Taken after the uploads, so that only changes made by commands are
	// reported back.
	before := scan(roots)
	roots.close()
	if perr := e.start(&start); perr != nil {
		e.fail(perr)
		return nil
	}
	e.supervise(before)
	return nil
}

// fail reports a command that could not be started and ends the stream.
func (e *execution) fail(perr *proto.Error) {
	_ = e.conn.Send(&proto.ExecFrame{Op: proto.ExecExit, Exit: &proto.ExecStatus{Err: perr}})
	_ = e.raw.Close()
}

// start starts the command. It returns the error to report when the
// command cannot be started.
func (e *execution) start(start *proto.ExecStart) *proto.Error {
	path, perr := lookPath(start.Argv[0], e.svc.target.LoginPath, start.Dir)
	if perr != nil {
		return perr
	}
	// Checked here because a failed chdir in the child is reported like a
	// failed exec, with the program's path.
	if fi, err := os.Stat(start.Dir); err != nil {
		return &proto.Error{Errno: errnoOf(err, unix.ENOENT), Msg: "chdir " + start.Dir}
	} else if !fi.IsDir() {
		return &proto.Error{Errno: uint32(unix.ENOTDIR), Msg: "chdir " + start.Dir}
	}

	attr := &os.ProcAttr{Dir: start.Dir, Env: mergeEnv(e.svc.env, start.Env)}
	child, pumps, err := e.setupStdio(start.TTY, attr)
	if err != nil {
		e.log.Warn("execsvc: set up stdio", "err", err)
		// The message leaves the errno to Errno, which proto.Error
		// appends itself.
		return &proto.Error{Errno: errnoOf(err, unix.EIO), Msg: "set up stdio"}
	}
	proc, err := os.StartProcess(path, start.Argv, attr)
	for _, f := range child {
		_ = f.Close()
	}
	if err != nil {
		_ = e.stdin.Close()
		for _, p := range pumps {
			p.close()
		}
		return &proto.Error{Errno: errnoOf(err, unix.EIO), Msg: start.Argv[0]}
	}
	e.proc = proc

	e.mu.Lock()
	e.pgid, e.pumps = proc.Pid, pumps
	aborted := e.aborted
	if aborted {
		e.killLocked(unix.SIGKILL)
	}
	e.mu.Unlock()
	if aborted {
		for _, p := range pumps {
			p.close()
		}
	}
	return nil
}

// setupStdio creates the command's stdio and fills in attr.Files and
// attr.Sys. It returns the child's ends, which the caller closes once the
// child started, and the output pumps; e.stdin gets the parent's input
// end.
func (e *execution) setupStdio(tty *proto.TTYSize, attr *os.ProcAttr) (child []*os.File, pumps []*pump, err error) {
	if tty != nil {
		master, slave, err := openPTY(*tty)
		if err != nil {
			return nil, nil, err
		}
		attr.Files = []*os.File{slave, slave, slave}
		// A new session with the pty as its controlling terminal, as with
		// ssh -t, so that the command gets terminal signals and job control.
		attr.Sys = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		e.stdin, e.pty = master, true
		return []*os.File{slave}, []*pump{newPump(master, proto.ExecStdout, true)}, nil
	}
	p, err := openPipes()
	if err != nil {
		return nil, nil, err
	}
	attr.Files = []*os.File{p.inR, p.outW, p.errW}
	attr.Sys = &syscall.SysProcAttr{Setpgid: true}
	e.stdin = p.inW
	return attr.Files, []*pump{newPump(p.outR, proto.ExecStdout, false), newPump(p.errR, proto.ExecStderr, false)}, nil
}

// stdioPipes are the pipes of a command without a pty.
type stdioPipes struct {
	inR, inW, outR, outW, errR, errW *os.File
}

func openPipes() (*stdioPipes, error) {
	var (
		p   stdioPipes
		err error
	)
	if p.inR, p.inW, err = os.Pipe(); err == nil {
		if p.outR, p.outW, err = os.Pipe(); err == nil {
			p.errR, p.errW, err = os.Pipe()
		}
	}
	if err != nil {
		for _, f := range []*os.File{p.inR, p.inW, p.outR, p.outW} {
			if f != nil {
				_ = f.Close()
			}
		}
		return nil, fmt.Errorf("pipe: %w", err)
	}
	return &p, nil
}

// supervise forwards frames until the command's main process exited and
// its output pipes reached EOF, or the stream ended.
func (e *execution) supervise(before *snapshot) {
	if err := e.conn.Send(&proto.ExecFrame{Op: proto.ExecStarted, PID: e.proc.Pid}); err != nil {
		e.abort()
	}
	var reader, writer, pumps sync.WaitGroup
	reader.Go(e.readLoop)
	writer.Go(e.stdinLoop)
	for _, p := range e.pumps {
		pumps.Go(func() { p.run(e.conn) })
	}

	ps, err := e.waitExit()
	e.endStdin()
	switch {
	case err != nil:
		e.log.Error("execsvc: wait for command", "err", err)
		e.abort()
	case !e.isAborted():
		e.reportExit(ps, before)
	}
	e.markMainDone()

	pumps.Wait()
	e.finishing.Store(true)
	_ = e.raw.Close()
	// Close only half-closes a multiplexed stream; the deadline ends the
	// reader even if the client never closes its side.
	_ = e.raw.SetReadDeadline(time.Now())
	reader.Wait()
	writer.Wait()
	_ = e.stdin.Close()
}

// waitExit waits for the main process to exit and reaps it.
func (e *execution) waitExit() (*os.ProcessState, error) {
	var info unix.Siginfo
	for {
		// WNOWAIT leaves the process a zombie, so that exited can be set
		// before its pid becomes reusable.
		err := unix.Waitid(unix.P_PID, e.proc.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	e.mu.Lock()
	e.exited = true
	e.mu.Unlock()
	return e.proc.Wait()
}

// reportExit sends ExecExit once everything the main process wrote before
// it exited has been sent.
func (e *execution) reportExit(ps *os.ProcessState, before *snapshot) {
	st := &proto.ExecStatus{}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		st.Signal = int(ws.Signal())
	} else {
		st.Code = ps.ExitCode()
	}
	// The process was reaped, so every file change it made is already
	// queued as an inotify event (docs/exec.md section 6).
	if e.svc.syncer != nil {
		st.WatchSeq = e.svc.syncer.Sync()
	}
	st.Scratch = e.svc.changedScratch(before)
	for _, p := range e.pumps {
		p.flush()
	}
	f := &proto.ExecFrame{Op: proto.ExecExit, Exit: st}
	err := e.conn.Send(f)
	var tooLarge *proto.FrameTooLargeError
	if errors.As(err, &tooLarge) {
		// changedScratch keeps within the limit, so this is a bug; the exit
		// status matters more than the files. WriteFrame checks the size
		// before writing anything, so the stream is intact.
		e.log.Error("execsvc: exit status too large, dropping scratch files", "size", tooLarge.Size, "files", len(st.Scratch))
		st.Scratch = nil
		err = e.conn.Send(f)
	}
	if err != nil {
		e.abort()
	}
}

// readLoop handles the client's frames until the stream ends.
func (e *execution) readLoop() {
	for {
		var f proto.ExecFrame
		if err := e.conn.Recv(&f); err != nil {
			if e.finishing.Load() {
				return
			}
			if !errors.Is(err, io.EOF) {
				e.log.Debug("execsvc: exec stream failed", "err", err)
			}
			// The client went away: kill the command if it is still
			// running, and stop forwarding output nobody reads.
			e.abort()
			return
		}
		if err := e.handle(&f); err != nil {
			e.log.Warn("execsvc: protocol violation", "err", err)
			e.abort()
			return
		}
	}
}

func (e *execution) handle(f *proto.ExecFrame) error {
	switch f.Op {
	case proto.ExecStdin:
		e.stdinQ.put(f.Data)
	case proto.ExecStdinEOF:
		e.stdinQ.end()
	case proto.ExecSignal:
		if f.Signal < 1 || f.Signal > maxSignal {
			e.log.Warn("execsvc: ignoring invalid signal", "signal", f.Signal)
			return nil
		}
		e.mu.Lock()
		e.killLocked(unix.Signal(f.Signal))
		e.mu.Unlock()
	case proto.ExecResize:
		if e.pty && f.TTY != nil {
			if err := setWinsize(e.stdin, *f.TTY); err != nil {
				e.log.Debug("execsvc: resize pty", "err", err)
			}
		}
	default:
		return fmt.Errorf("unexpected exec frame op %d", f.Op)
	}
	return nil
}

// stdinLoop writes the client's stdin data to the command until the
// client ends its input, the command stops reading (EPIPE), or the main
// process exits.
func (e *execution) stdinLoop() {
	for {
		data, eof, ok := e.stdinQ.next()
		if !ok {
			if eof {
				e.closeStdin()
			}
			return
		}
		if _, err := e.stdin.Write(data); err != nil {
			// EPIPE: the command closed its stdin. Closed or past the
			// deadline: the main process exited (endStdin).
			e.stdinQ.close()
			if !errors.Is(err, unix.EPIPE) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, os.ErrDeadlineExceeded) {
				e.log.Debug("execsvc: write stdin", "err", err)
			}
			return
		}
	}
}

// closeStdin delivers the client's end of input. A pty cannot be
// half-closed without hanging up the whole session, so it gets the
// terminal's EOF character instead, which ends input for a program
// reading in canonical mode at the start of a line.
func (e *execution) closeStdin() {
	if e.pty {
		_, _ = e.stdin.Write([]byte{eofChar(e.stdin)})
		return
	}
	_ = e.stdin.Close()
}

// endStdin stops stdin forwarding when the main process exited: nothing
// the client sends later is meant for background children, and a write
// blocked on a full pipe or pty must not keep stdinLoop forever.
func (e *execution) endStdin() {
	e.stdinQ.close()
	if e.pty {
		_ = e.stdin.SetWriteDeadline(time.Now())
		return
	}
	_ = e.stdin.Close()
}

// killLocked signals the command's process group while its main process
// is known not to be reaped. Callers hold e.mu.
func (e *execution) killLocked(sig unix.Signal) {
	if e.pgid == 0 || e.exited {
		return
	}
	if err := unix.Kill(-e.pgid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
		e.log.Warn("execsvc: signal process group", "signal", int(sig), "err", err)
	}
}

// terminate asks a running command to stop with SIGTERM to its process
// group; an execution whose command has not started is aborted.
func (e *execution) terminate() {
	e.mu.Lock()
	started := e.pgid != 0
	e.killLocked(unix.SIGTERM)
	e.mu.Unlock()
	if !started {
		e.abort()
	}
}

// abort ends the execution early: the stream failed or was closed by the
// client, the context was cancelled, or the service is closing. A running
// command is killed; after its exit only the output pipes are closed.
func (e *execution) abort() {
	e.mu.Lock()
	if e.aborted {
		e.mu.Unlock()
		return
	}
	e.aborted = true
	e.killLocked(unix.SIGKILL)
	pumps := e.pumps
	e.mu.Unlock()

	// readLoop may wait for room in the queue.
	e.stdinQ.close()
	for _, p := range pumps {
		p.close()
	}
	_ = e.raw.Close()
	// Close only half-closes a multiplexed stream; the deadline also ends
	// a read of ExecStart or of client frames.
	_ = e.raw.SetReadDeadline(time.Now())
}

func (e *execution) isAborted() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.aborted
}

// errnoOf returns the errno err wraps, or def.
func errnoOf(err error, def unix.Errno) uint32 {
	var errno unix.Errno
	if errors.As(err, &errno) {
		return uint32(errno)
	}
	return uint32(def)
}
