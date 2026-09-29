// Package execsvc runs remote commands for tele server, one command per
// exec stream (docs/exec.md "进程与信号", "scratch 路径改写与回传"
// and "exec 屏障").
//
// A command runs as the server's user in its own process group, or as the
// leader of a new session on a pseudo-terminal when the client asks for
// one. Its exit status is sent as soon as the main process ends, carrying
// the exec barrier and the scratch files the command changed; output of
// background children that keep the output pipes open follows until they
// close them, and the stream ends when both pipes reach EOF.
//
// The client ending the stream before the exit status was sent means the
// shim was killed, so the whole process group is killed with it. After
// the exit status only the pipes are closed, which leaves background
// children running as they would locally (docs/exec.md "shim 与会话主进程").
//
// Signals travel in the exec stream behind stdin data. Stdin has a window
// of its own (proto.ExecStdinWindow): the server acknowledges input once
// the command has been handed it, and queues at most a window of input
// the command has not read yet, so it always keeps handling the client's
// frames and a signal or the end of the stream never waits for the
// command to read.
//
// Output goes through a spool (spool.go), bounded and spilling to disk, so
// that a command keeps running while the session has no transport
// (docs/exec.md "进程与信号").
package execsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ujzk/tele-agent/internal/proto"
)

// DefaultPath is PATH for commands when the target's login shell reported
// none.
const DefaultPath = "/usr/local/bin:/usr/bin:/bin"

// ErrClosed is returned by Serve after Close or Terminate.
var ErrClosed = errors.New("execsvc: service closed")

// Syncer provides the exec barrier (docs/exec.md "exec 屏障").
type Syncer interface {
	// Sync numbers every pending file change event and returns the
	// sequence number of the last WatchEvent.
	Sync() uint64
}

// Config configures a Service.
type Config struct {
	// Target describes the user commands run as.
	Target proto.TargetInfo
	// BaseEnv is the environment every command starts from, before the
	// client's variables; nil means BaseEnv(Target).
	BaseEnv []string
	// ScratchDir holds the session's scratch areas.
	ScratchDir string
	// Syncer provides the exec barrier; nil reports WatchSeq 0.
	Syncer Syncer
	// Logger receives diagnostics; nil discards them.
	Logger *slog.Logger
}

// Service runs the commands of one session.
type Service struct {
	env        []string
	target     proto.TargetInfo
	syncer     Syncer
	log        *slog.Logger
	scratchDir string

	mu     sync.Mutex
	closed bool                    // guarded by mu
	execs  map[*execution]struct{} // guarded by mu
	wg     sync.WaitGroup          // counts Serve calls that passed the closed check

	upMu sync.Mutex
	// uploads records the scratch files the service wrote or removed for
	// the client, so that a command running meanwhile does not report
	// them as its own changes (see changedScratch).
	uploads map[scratchKey]uploadRecord // guarded by upMu
	upSeq   uint64                      // guarded by upMu; last uploadRecord.seq
}

// BaseEnv returns the environment a login shell of t would start with, as
// far as commands depend on it: HOME, USER, LOGNAME, SHELL, and PATH from
// the login shell. LANG is taken from the server's own environment, since
// the target user's locale is usually set system-wide.
func BaseEnv(t proto.TargetInfo) []string {
	var env []string
	for _, kv := range [...][2]string{{"HOME", t.Home}, {"USER", t.User}, {"LOGNAME", t.User}, {"SHELL", t.Shell}} {
		if kv[1] != "" {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	path := t.LoginPath
	if path == "" {
		path = DefaultPath
	}
	env = append(env, "PATH="+path)
	if lang := os.Getenv("LANG"); lang != "" {
		env = append(env, "LANG="+lang)
	}
	return env
}

// New creates the scratch area directories below cfg.ScratchDir and
// returns a Service.
func New(cfg Config) (*Service, error) {
	if err := proto.CheckPath(cfg.ScratchDir); err != nil {
		return nil, fmt.Errorf("execsvc: scratch dir: %w", err)
	}
	s := &Service{
		env:        cfg.BaseEnv,
		target:     cfg.Target,
		syncer:     cfg.Syncer,
		log:        cfg.Logger,
		scratchDir: cfg.ScratchDir,
		execs:      make(map[*execution]struct{}),
		uploads:    make(map[scratchKey]uploadRecord),
	}
	if s.env == nil {
		s.env = BaseEnv(cfg.Target)
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	for _, a := range proto.ScratchAreas {
		if err := os.MkdirAll(filepath.Join(cfg.ScratchDir, a.Dir()), 0o700); err != nil {
			return nil, fmt.Errorf("execsvc: create scratch area: %w", err)
		}
	}
	return s, nil
}

// Serve runs the command requested on c, an exec stream whose
// StreamHeader was already read, and returns when the stream is done.
// Cancelling ctx kills the command like a client that went away. Serve
// closes c. It returns ErrClosed after Close or Terminate, and an error
// when no valid ExecStart arrived; a command that fails to start is
// reported to the client instead.
func (s *Service) Serve(ctx context.Context, c net.Conn) error {
	e := newExecution(s, c)
	if !s.add(e) {
		_ = c.Close()
		return ErrClosed
	}
	defer s.remove(e)
	stop := context.AfterFunc(ctx, e.abort)
	defer stop()
	return e.run()
}

// Close kills every command whose main process is still running, closes
// every stream, and waits for all Serve calls to return.
func (s *Service) Close() error {
	for _, e := range s.shutdown() {
		e.abort()
	}
	s.wg.Wait()
	return nil
}

// Terminate ends the session's commands gracefully: SIGTERM to the process
// group of every command whose main process still runs, then, once those
// main processes exited and their exit status was sent, or grace elapsed,
// Close, which kills the rest with SIGKILL. Commands that have not started
// yet are not started.
func (s *Service) Terminate(grace time.Duration) error {
	s.stop(grace, (*execution).statusSent)
	return s.Close()
}

// Stop is what an expired session lease requires (docs/transport.md
// "断线语义"): like the first half of Terminate, but it waits only until the
// main processes exited, not until their exit status was sent, since the
// session has no transport to send it on. Closing the session then kills
// the rest.
func (s *Service) Stop(grace time.Duration) {
	s.stop(grace, (*execution).procDone)
}

func (s *Service) stop(grace time.Duration, done func(*execution) <-chan struct{}) {
	execs := s.shutdown()
	for _, e := range execs {
		e.terminate()
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for _, e := range execs {
		select {
		case <-done(e):
		case <-timer.C:
			return
		}
	}
}

// shutdown stops accepting commands and returns the executions in
// progress.
func (s *Service) shutdown() []*execution {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	execs := make([]*execution, 0, len(s.execs))
	for e := range s.execs {
		execs = append(execs, e)
	}
	return execs
}

func (s *Service) add(e *execution) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.execs[e] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Service) remove(e *execution) {
	s.mu.Lock()
	delete(s.execs, e)
	s.mu.Unlock()
	s.wg.Done()
}
