package execsvc

import (
	"errors"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Limits of one output spool (docs/exec.md "进程与信号").
const (
	// spoolMemory is how much output a spool keeps in memory before it
	// spills to disk.
	spoolMemory = 1 << 20
	// spoolLimit bounds the output a spool holds in all; beyond it the
	// command blocks on writing its output, as it would on a full pipe.
	spoolLimit = 64 << 20
)

// errSpoolAborted is returned by spool.Write after abort.
var errSpoolAborted = errors.New("execsvc: output spool aborted")

// spool buffers one output stream of a command between reading the pipe
// and sending it to the client: while the session has no transport (or
// the client reads slowly), the command keeps running, and its output
// waits here, in memory up to spoolMemory and beyond that in an unlinked
// file in dir, up to spoolLimit in all.
type spool struct {
	dir string

	mu   sync.Mutex
	cond *sync.Cond // signalled when any field below changes
	mem  []byte     // guarded by mu; buffered output not yet in the file
	// The file holds output after mem, from offset rd to wr; data goes
	// to it whenever it already holds some, so that order is kept.
	file    *os.File // guarded by mu; nil until needed
	rd, wr  int64    // guarded by mu
	eof     bool     // guarded by mu; no more writes
	aborted bool     // guarded by mu; reads and writes fail
	fileErr error    // guarded by mu; the file could not be used
}

func newSpool(dir string) *spool {
	s := &spool{dir: dir}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// buffered returns the bytes held; the caller holds mu.
func (s *spool) buffered() int64 {
	return int64(len(s.mem)) + s.wr - s.rd
}

// Write appends p, waiting while the spool is full.
func (s *spool) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.aborted && s.buffered() > 0 && s.buffered()+int64(len(p)) > s.limit() {
		s.cond.Wait()
	}
	if s.aborted {
		return 0, errSpoolAborted
	}
	defer s.cond.Broadcast()
	if s.wr == s.rd && len(s.mem)+len(p) <= spoolMemory || s.fileErr != nil {
		s.mem = append(s.mem, p...)
		return len(p), nil
	}
	if err := s.writeFile(p); err != nil {
		// Without a file the spool holds what memory allows; this write
		// still fits the memory bound's order of magnitude.
		s.fileErr = err
		s.mem = append(s.mem, p...)
	}
	return len(p), nil
}

// limit is the spool's capacity; without a usable file only memory counts.
func (s *spool) limit() int64 {
	if s.fileErr != nil || s.dir == "" {
		return spoolMemory
	}
	return spoolLimit
}

// writeFile appends p to the file; the caller holds mu. The file is
// written under the lock: writes to a local file do not block for long,
// and ordering against Read needs the lock anyway.
func (s *spool) writeFile(p []byte) error {
	if s.dir == "" {
		return errors.New("execsvc: no spool directory")
	}
	if s.file == nil {
		fd, err := unix.Open(s.dir, unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return err
		}
		s.file = os.NewFile(uintptr(fd), "spool")
	}
	if _, err := s.file.WriteAt(p, s.wr); err != nil {
		return err
	}
	s.wr += int64(len(p))
	return nil
}

// Read reads buffered output, waiting for some; it returns io.EOF once
// CloseWrite was called and everything was read.
func (s *spool) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.aborted && s.buffered() == 0 && !s.eof {
		s.cond.Wait()
	}
	switch {
	case s.aborted:
		return 0, errSpoolAborted
	case s.buffered() == 0:
		return 0, io.EOF
	}
	defer s.cond.Broadcast()
	if len(s.mem) > 0 {
		n := copy(p, s.mem)
		s.mem = s.mem[n:]
		if len(s.mem) == 0 {
			s.mem = nil
		}
		return n, nil
	}
	n, err := s.file.ReadAt(p[:min(int64(len(p)), s.wr-s.rd)], s.rd)
	s.rd += int64(n)
	if s.rd == s.wr {
		// Drained: start over at the front of the file.
		s.rd, s.wr = 0, 0
		_ = s.file.Truncate(0) // only frees space
	}
	if n > 0 {
		return n, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return 0, err
}

// CloseWrite marks the end of the output.
func (s *spool) CloseWrite() {
	s.mu.Lock()
	s.eof = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

// abort makes every Read and Write fail, and frees the file.
func (s *spool) abort() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborted = true
	s.mem = nil
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	s.cond.Broadcast()
}
