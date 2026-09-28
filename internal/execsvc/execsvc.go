// Package execsvc runs remote commands for tele server, one command per
// exec stream (docs/exec.md sections 4 to 6).
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
// children running as they would locally (docs/exec.md section 2).
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

	"github.com/ujzk/tele-agent/internal/proto"
)

// DefaultPath is PATH for commands when the target's login shell reported
// none.
const DefaultPath = "/usr/local/bin:/usr/bin:/bin"

// ErrClosed is returned by Serve after Close.
var ErrClosed = errors.New("execsvc: service closed")

// Syncer provides the exec barrier (docs/exec.md section 6).
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
	env    []string
	target proto.TargetInfo
	syncer Syncer
	log    *slog.Logger
	areas  map[proto.ScratchArea]*os.Root

	mu     sync.Mutex
	closed bool                    // guarded by mu
	execs  map[*execution]struct{} // guarded by mu
	wg     sync.WaitGroup          // counts Serve calls that passed the closed check
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
		env:    cfg.BaseEnv,
		target: cfg.Target,
		syncer: cfg.Syncer,
		log:    cfg.Logger,
		areas:  make(map[proto.ScratchArea]*os.Root, len(proto.ScratchAreas)),
		execs:  make(map[*execution]struct{}),
	}
	if s.env == nil {
		s.env = BaseEnv(cfg.Target)
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	for _, a := range proto.ScratchAreas {
		dir := filepath.Join(cfg.ScratchDir, a.Dir())
		if err := os.MkdirAll(dir, 0o700); err != nil {
			s.closeAreas()
			return nil, fmt.Errorf("execsvc: create scratch area: %w", err)
		}
		r, err := os.OpenRoot(dir)
		if err != nil {
			s.closeAreas()
			return nil, fmt.Errorf("execsvc: open scratch area: %w", err)
		}
		s.areas[a] = r
	}
	return s, nil
}

// Serve runs the command requested on c, an exec stream whose
// StreamHeader was already read, and returns when the stream is done.
// Cancelling ctx kills the command like a client that went away. Serve
// closes c. It returns ErrClosed after Close, and an error when no valid
// ExecStart arrived; a command that fails to start is reported to the
// client instead.
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
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	execs := make([]*execution, 0, len(s.execs))
	for e := range s.execs {
		execs = append(execs, e)
	}
	s.mu.Unlock()

	for _, e := range execs {
		e.abort()
	}
	s.wg.Wait()
	s.closeAreas()
	return nil
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

func (s *Service) closeAreas() {
	for _, r := range s.areas {
		_ = r.Close()
	}
}
