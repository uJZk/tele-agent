// Package fssvc serves telefs on the target host (docs/telefs.md): every FS
// stream carries one proto.FSRequest, executed with the credentials of the
// server process (the target user), and the watch stream pushes the
// inotify-derived changes that let the client cache entries and attributes
// for long TTLs.
//
// Paths in requests are absolute paths of the remote view. The service
// resolves them under Config.Root, which is "/" in production; tests serve a
// temporary directory instead. Root is a test facility, not a security
// boundary: symlinks in intermediate components are followed as the kernel
// follows them.
//
// Every operation acts on the named node itself (lstat semantics): the last
// path component is never followed, because the client resolves symlinks
// through READLINK and must never have an operation land on a different
// object than the node it named.
//
// Errors are returned as the exact errno of the failing system call
// (docs/coding-standards.md section 4).
package fssvc

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// ErrClosed is returned by ServeWatch after Close.
var ErrClosed = errors.New("fssvc: service closed")

// Config configures a Service.
type Config struct {
	// Root is the directory that the remote view's "/" maps to: "/" in
	// production; tests serve a temporary directory as the root. It is a
	// test facility, not a security boundary.
	Root   string
	Logger *slog.Logger
}

// Service executes FS requests and publishes directory changes. All methods
// are safe for concurrent use.
type Service struct {
	root string
	log  *slog.Logger
	// umask is the process umask, or -1 if it could not be read. The client
	// kernel already applied the caller's umask to create modes, so the
	// server must not apply its own on top (see fixMode).
	umask int

	handles handleTable

	// inotify is the non-blocking inotify descriptor, registered with the
	// runtime poller so that the reader can wait for it with a deadline.
	inotify *os.File
	rawIn   syscall.RawConn

	// readMu serializes reading the inotify descriptor: holding it across
	// a drain lets Sync publish every event queued before it returns
	// (docs/exec.md section 6). Nothing that resolves a path runs under
	// it: see watch.
	readMu sync.Mutex
	evBuf  []byte // guarded by readMu

	mu sync.Mutex // guards the fields below
	w  watchState

	wg sync.WaitGroup // tracks the inotify reader
}

// New creates a Service and starts its inotify reader.
func New(cfg Config) (*Service, error) {
	if err := proto.CheckPath(cfg.Root); err != nil {
		return nil, fmt.Errorf("fssvc: root: %w", err)
	}
	st, err := os.Stat(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("fssvc: root: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("fssvc: root %s is not a directory", cfg.Root)
	}
	s, err := newService(cfg)
	if err != nil {
		return nil, err
	}
	s.wg.Add(1)
	go s.readLoop()
	return s, nil
}

// newService creates a Service without starting the inotify reader, so that
// tests can let the kernel queue overflow.
func newService(cfg Config) (*Service, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("fssvc: inotify_init1: %w", err)
	}
	f := os.NewFile(uintptr(fd), "inotify")
	raw, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("fssvc: inotify raw conn: %w", err)
	}
	s := &Service{
		root:    cfg.Root,
		log:     log,
		umask:   processUmask(),
		inotify: f,
		rawIn:   raw,
		evBuf:   make([]byte, 64<<10),
	}
	s.handles.init()
	s.w.init(randomEpoch())
	return s, nil
}

// randomEpoch picks the first epoch of a service instance at random, so that
// a client talking to a restarted server sees a different epoch even though
// sequence numbers start over.
func randomEpoch() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return binary.NativeEndian.Uint64(b[:])>>1 | 1
}

// processUmask reads the umask from /proc/self/status. Reading it with
// umask(2) would briefly change it for every thread of the process.
func processUmask() int {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return -1
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "Umask:")
		if !ok {
			continue
		}
		m, err := strconv.ParseUint(strings.TrimSpace(v), 8, 32)
		if err != nil {
			return -1
		}
		return int(m)
	}
	return -1
}

// Close stops the watch machinery and closes every open handle. Requests
// served afterwards fail or run without watches.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.w.closed {
		s.mu.Unlock()
		return nil
	}
	s.w.closed = true
	if s.w.streamCancel != nil {
		s.w.streamCancel()
	}
	s.mu.Unlock()

	err := s.inotify.Close()
	s.wg.Wait()
	s.handles.closeAll()
	if err != nil {
		return fmt.Errorf("fssvc: close inotify: %w", err)
	}
	return nil
}

// ServeRequest serves one FS stream whose header was already read: it reads
// one FSRequest, executes it and writes one FSResponse (none for FSForget),
// then closes c. Failures of the operation itself travel in the response;
// the returned error reports only stream failures.
func (s *Service) ServeRequest(ctx context.Context, c net.Conn) error {
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	var req proto.FSRequest
	if err := proto.ReadFrame(c, &req, proto.MaxDataFrame); err != nil {
		return fmt.Errorf("fssvc: read request: %w", err)
	}
	resp := s.do(&req)
	if req.Op == proto.FSForget {
		return nil
	}
	if err := proto.WriteFrame(c, resp); err != nil {
		return fmt.Errorf("fssvc: write %d response: %w", req.Op, err)
	}
	return nil
}

// real maps a remote-view path to the path on this host.
func (s *Service) real(p string) string {
	if s.root == "/" {
		return p
	}
	if p == "/" {
		return s.root
	}
	return s.root + p
}

// child maps the entry name of directory dir to the path on this host.
func (s *Service) child(dir, name string) string {
	if dir == "/" {
		return s.real("/" + name)
	}
	return s.real(dir + "/" + name)
}

// ignoringEINTR retries fn while it fails with EINTR. The Go runtime
// preempts goroutines with signals, and raw x/sys calls on slow file
// systems (NFS, FUSE) return EINTR instead of restarting.
func ignoringEINTR(fn func() error) error {
	for {
		err := fn()
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// errnoOf extracts the errno of a failed system call. Errors that carry no
// errno cannot come from the file system and are reported as EIO.
func errnoOf(err error) uint32 {
	var e unix.Errno
	if errors.As(err, &e) {
		return uint32(e)
	}
	return uint32(unix.EIO)
}

// logDebug logs at debug level; paths are logged only at this level
// (docs/coding-standards.md section 10).
func (s *Service) logDebug(msg string, args ...any) {
	s.log.Debug(msg, args...)
}
