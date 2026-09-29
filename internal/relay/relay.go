// Package relay runs shim invocations for session main (docs/exec.md): it
// classifies each one (internal/dispatch), then runs it on the target host
// (internal/rexec) with scratch paths rewritten and scratch files synced
// both ways, or in session main's local view (internal/localexec), and
// reports how it ended for the shim to reproduce.
package relay

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/dispatch"
	"github.com/ujzk/tele-agent/internal/localexec"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/rexec"
	"github.com/ujzk/tele-agent/internal/scratch"
	"github.com/ujzk/tele-agent/internal/shimsrv"
)

// codeFailure is the shim's infrastructure failure code (docs/exec.md
// "shim").
const codeFailure = 255

// Config configures a Relay.
type Config struct {
	// SessDir is the session directory as Claude sees it.
	SessDir string
	// Baseline is the environment tele gave Claude; only what Claude added
	// or changed is forwarded (docs/exec.md "环境变量").
	Baseline []string
	// LocalProgs names the local exec proxies.
	LocalProgs map[string]bool
	// Exec starts remote commands; its Barrier makes file changes visible
	// before an exit is reported (docs/exec.md "exec 屏障").
	Exec *rexec.Client
	// Scratch rewrites scratch paths and syncs scratch files.
	Scratch *scratch.Mapper
	// Logger receives diagnostics; nil discards them. Never a shim's
	// stdio (docs/coding-standards.md "日志与输出").
	Logger *slog.Logger
}

// Relay implements shimsrv.Handler.
type Relay struct {
	cfg Config
	log *slog.Logger
	wg  sync.WaitGroup // output forwarding that outlives Serve
}

var _ shimsrv.Handler = (*Relay)(nil)

// New returns a Relay.
func New(cfg Config) *Relay {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Relay{cfg: cfg, log: log}
}

// Wait waits until the output of every command has been forwarded and its
// stdio closed. Call it after the shim server stopped.
func (r *Relay) Wait() {
	r.wg.Wait()
}

// Serve implements shimsrv.Handler.
func (r *Relay) Serve(ctx context.Context, req *shimsrv.Request, sigs <-chan int) proto.ShimStatus {
	act, err := dispatch.Classify(req.Name, req.Argv, r.cfg.SessDir, r.cfg.LocalProgs)
	if err != nil {
		closeAll(req)
		return proto.ShimStatus{Code: codeFailure, Msg: err.Error()}
	}
	if act.Local {
		defer closeAll(req)
		return localexec.Run(ctx, act.Argv, req.Dir, req.Env, req.Stdin, req.Stdout, req.Stderr, sigs)
	}
	return r.remote(ctx, req, act.Argv, sigs)
}

// remote runs argv on the target host.
func (r *Relay) remote(ctx context.Context, req *shimsrv.Request, argv []string, sigs <-chan int) proto.ShimStatus {
	m := r.cfg.Scratch
	// Rewrite before Uploads: it claims the shared-area entries the
	// command names, which Uploads then includes (scratch.Mapper.Uploads).
	cmd := rexec.Command{
		Argv:   rewriteAll(m, argv),
		Dir:    m.Rewrite(req.Dir),
		Env:    rewriteAll(m, dispatch.FilterEnv(req.Env, r.cfg.Baseline)),
		TTY:    ttySize(req),
		Stdin:  req.Stdin,
		Stdout: req.Stdout,
		Stderr: req.Stderr,
	}
	// Output files in a scratch area stay out of the sync while written
	// (docs/exec.md "scratch 路径改写与回传").
	release := r.hold(req.Stdout, req.Stderr)
	up := m.Uploads(rexec.ScratchBudget(cmd))
	cmd.Scratch = up.Files
	p, err := r.cfg.Exec.Start(ctx, cmd)
	if err != nil {
		up.Rollback()
		release()
		closeAll(req)
		r.log.Warn("relay: start remote command", "err", err)
		return proto.ShimStatus{Code: codeFailure, Msg: fmt.Sprintf("cannot reach the target host: %v", err)}
	}
	r.wg.Go(func() {
		<-p.Done()
		release()
		closeAll(req)
	})

	sigCtx, stopSigs := context.WithCancel(ctx)
	var fwd sync.WaitGroup
	fwd.Go(func() { forwardSignals(sigCtx, p, sigs) })
	res, err := p.Wait(ctx)
	stopSigs()
	fwd.Wait()
	finishUpload(up, p)
	switch {
	case ctx.Err() != nil:
		// The shim was killed: kill the command with it.
		p.Abandon()
		return proto.ShimStatus{Code: codeFailure, Msg: "interrupted"}
	case err != nil:
		p.Abandon()
		r.log.Warn("relay: remote command", "err", err)
		return proto.ShimStatus{Code: codeFailure, Msg: fmt.Sprintf("lost the remote command: %v", err)}
	}
	if err := m.Apply(res.Scratch); err != nil {
		r.log.Warn("relay: apply scratch files", "err", err)
	}
	return status(argv[0], res)
}

// finishUpload commits the upload if the command started, which is when
// the server has written the files (scratch.Upload). Once Wait returned,
// Started is final: the server reports the start before the exit.
func finishUpload(up *scratch.Upload, p *rexec.Process) {
	select {
	case <-p.Started():
		up.Commit()
	default:
		up.Rollback()
	}
}

// forwardSignals delivers the shim's signals to the remote command until
// ctx is done.
func forwardSignals(ctx context.Context, p *rexec.Process, sigs <-chan int) {
	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-sigs:
			if !ok {
				return
			}
			// ErrFinished and friends: the command is over; nothing to do.
			_ = p.Signal(ctx, sig)
		}
	}
}

// status converts a remote result into what the shim reproduces.
func status(prog string, res rexec.Result) proto.ShimStatus {
	if e := res.StartErr; e != nil {
		// Like a local program that cannot be run.
		return localexec.NotStarted(e, "%s: %v", prog, e)
	}
	if res.Signal != 0 {
		return proto.ShimStatus{Signal: res.Signal}
	}
	return proto.ShimStatus{Code: res.Code}
}

func rewriteAll(m *scratch.Mapper, ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = m.Rewrite(s)
	}
	return out
}

// hold keeps the given output files out of the scratch sync and returns
// the function that releases them.
func (r *Relay) hold(files ...*os.File) func() {
	var releases []func()
	for _, f := range files {
		if f == nil {
			continue
		}
		rel, err := r.cfg.Scratch.Hold(f)
		if err != nil {
			r.log.Debug("relay: hold output file", "err", err)
			continue
		}
		releases = append(releases, rel)
	}
	return func() {
		for _, rel := range releases {
			rel()
		}
	}
}

// ttySize requests a pseudo-terminal of the shim's terminal size when its
// stdin and stdout are both terminals, as ssh does for an interactive
// command; otherwise the command gets pipes, with stdout and stderr apart.
func ttySize(req *shimsrv.Request) *proto.TTYSize {
	if req.Stdin == nil || req.Stdout == nil {
		return nil
	}
	isTTY := false
	withFd(req.Stdin, func(fd int) {
		_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		isTTY = err == nil
	})
	if !isTTY {
		return nil
	}
	var size *proto.TTYSize
	withFd(req.Stdout, func(fd int) {
		if ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); err == nil {
			size = &proto.TTYSize{Rows: ws.Row, Cols: ws.Col}
		}
	})
	return size
}

// withFd runs fn on f's descriptor. Unlike File.Fd, it never switches the
// descriptor to blocking mode: the open file description is shared with
// Claude (docs/exec.md "shim 与会话主进程").
func withFd(f *os.File, fn func(fd int)) {
	rc, err := f.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) { fn(int(fd)) }) // fails only for a closed file
}

func closeAll(req *shimsrv.Request) {
	for _, f := range []*os.File{req.Stdin, req.Stdout, req.Stderr} {
		if f != nil {
			_ = f.Close() // session main's duplicates; the shim keeps its own
		}
	}
}
