package execsvc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

func TestStdinQueue(t *testing.T) {
	q := newStdinQueue()
	// An empty queue takes a chunk of any size.
	big := make([]byte, stdinQueueMax+1)
	q.put(big)
	put := make(chan struct{})
	go func() {
		defer close(put)
		q.put([]byte("second"))
	}()
	select {
	case <-put:
		t.Fatal("put did not wait for room in a full queue")
	default:
	}
	if data, eof, ok := q.next(); !ok || eof || len(data) != len(big) {
		t.Fatalf("next = %d bytes, eof %v, ok %v", len(data), eof, ok)
	}
	<-put
	q.end()
	q.put([]byte("after end")) // dropped
	if data, _, ok := q.next(); !ok || string(data) != "second" {
		t.Fatalf("next = %q, %v; want second", data, ok)
	}
	if _, eof, ok := q.next(); ok || !eof {
		t.Fatalf("drained queue after end: eof %v ok %v", eof, ok)
	}

	// close releases a waiting put and a waiting next, and drops data.
	q = newStdinQueue()
	q.put(big)
	var wg sync.WaitGroup
	wg.Go(func() { q.put([]byte("x")) })
	q.close()
	wg.Wait()
	if _, eof, ok := q.next(); ok || eof {
		t.Fatalf("next after close: eof %v ok %v", eof, ok)
	}
	q = newStdinQueue()
	wg.Go(func() {
		if _, _, ok := q.next(); ok {
			t.Error("next returned data after close")
		}
	})
	q.close()
	wg.Wait()
}

// flood sends n bytes of stdin in 32 KiB frames. The stream write deadline
// turns a server that stopped reading into a test failure.
func (s *stream) flood(n int) {
	s.t.Helper()
	if err := s.floodErr(n); err != nil {
		s.t.Fatal(err)
	}
}

// floodErr is flood for goroutines other than the test's.
func (s *stream) floodErr(n int) error {
	if err := s.raw.SetWriteDeadline(time.Now().Add(testTimeout)); err != nil {
		return err
	}
	chunk := make([]byte, 32<<10)
	for ; n > 0; n -= len(chunk) {
		if err := s.conn.Send(&proto.ExecFrame{Op: proto.ExecStdin, Data: chunk[:min(n, len(chunk))]}); err != nil {
			return err
		}
	}
	return nil
}

func (s *stream) untilStarted(o *outcome) {
	s.t.Helper()
	for o.pid == 0 {
		if !s.next(o) {
			s.t.Fatal("no ExecStarted")
		}
	}
}

// floodSize is far beyond the pipe and the stream window, but below
// stdinQueueMax.
const floodSize = 4 << 20

func TestStdinBacklogKeepsSignals(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"sleep", "1000"}})
	o := &outcome{}
	s.untilStarted(o)
	s.flood(floodSize)
	s.send(&proto.ExecFrame{Op: proto.ExecSignal, Signal: int(unix.SIGTERM)})
	for s.next(o) {
	}
	if o.exit == nil || o.exit.Signal != int(unix.SIGTERM) {
		t.Fatalf("exit %+v, want signal 15", o.exit)
	}
}

func TestStdinBacklogKeepsAbandon(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"sleep", "1000"}})
	o := &outcome{}
	s.untilStarted(o)
	s.flood(floodSize)
	_ = s.raw.Close()
	waitGone(t, o.pid)
}

func TestStdinBeyondQueueIsKept(t *testing.T) {
	// A command that reads its input only later gets all of it, even when
	// the backlog exceeds the queue and the reader has to wait.
	h := newHarness(t, nil)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	s := h.start(t, &proto.ExecStart{Argv: sh(`read x < "$1"; wc -c`, fifo)})
	const size = stdinQueueMax + 3<<20
	sent := make(chan error, 1)
	go func() {
		err := s.floodErr(size)
		if err == nil {
			err = s.conn.Send(&proto.ExecFrame{Op: proto.ExecStdinEOF})
		}
		sent <- err
	}()
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("go\n")
	_ = w.Close()
	o := s.collect()
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(o.stdout.String()); o.exit.Code != 0 || got != strconv.Itoa(size) {
		t.Fatalf("exit %+v, wc -c = %q, want %d", o.exit, got, size)
	}
}

func TestStdinBacklogOnPTY(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"sleep", "1000"}, TTY: &proto.TTYSize{Rows: 24, Cols: 80}})
	o := &outcome{}
	s.untilStarted(o)
	s.flood(1 << 20)
	s.send(&proto.ExecFrame{Op: proto.ExecSignal, Signal: int(unix.SIGTERM)})
	for s.next(o) {
	}
	if o.exit == nil || o.exit.Signal != int(unix.SIGTERM) {
		t.Fatalf("exit %+v, want signal 15", o.exit)
	}
}
